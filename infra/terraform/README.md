# Powerhouse Cache — Terraform (AWS)

Provisions the full runtime for the cache on AWS, Free-Tier-friendly and
OIDC-authenticated (no static AWS keys). This is the implementation of
[`docs/DEPLOYMENT.md`](../../docs/DEPLOYMENT.md) §5–6, **Topology A** (single EC2
+ EBS).

## What it creates

| Area | Resources |
|---|---|
| **Network** | VPC, 2 public subnets (multi-AZ), Internet Gateway, route table, **VPC flow logs** |
| **Security** | `cache_sg` (6379 **only** from `app_sg`, no SSH), `app_sg`, EC2 instance role (ECR pull + logs + SSM) |
| **Registry** | ECR repo, scan-on-push, immutable tags, lifecycle expiry |
| **Compute** | EC2 (`t3.micro`) in a public subnet, **encrypted EBS** at `/data`, Elastic IP, IMDSv2-only, user-data runs the hardened container |
| **Identity** | GitHub **OIDC provider** + `gha-ci-role` (ECR push) + `gha-deploy-role` (terraform apply, bound to `environment:production`) |
| **Observability** | CloudWatch log group (container logs via `awslogs` driver), SNS topic, CPU + status-check alarms |
| **Backup/DR** | DLM daily EBS snapshots (7-day retention) |
| **State** (in `bootstrap/`) | S3 (versioned, KMS, private) + DynamoDB lock table |

## Networking modes (cost vs privacy)

`var.network_mode` — only **`public-free`** is implemented today (the chosen
$0 path): the instance sits in a public subnet with a public IP so it can pull
from ECR / ship logs over the IGW with **no NAT Gateway and no VPC endpoints**.
The cache is protected at the **security group** (6379 reachable only from
`app_sg`), not the subnet. Two private modes are designed but not yet wired:

- `private-endpoints` — private subnet + interface endpoints (ECR/Logs/SSM) + S3
  gateway endpoint. ~$21/mo, fully private.
- `private-nat` — private subnet + NAT Gateway. ~$32/mo, simplest private egress.

> The cache has no AUTH/TLS, so **never** add a public ingress rule on 6379.
> Test it via SSM port-forwarding (below), not by opening the port.

## One-time setup

### 1. Bootstrap remote state (human, once)

```bash
cd infra/terraform/bootstrap
terraform init
terraform apply -var 'state_bucket_name=powerhouse-cache-tfstate-<your-unique-suffix>'
```

Authenticate as a human with **short-lived** creds (`aws sso login` is ideal).
Note the two outputs.

### 2. Point the root module at that state

Edit [`backend.tf`](./backend.tf): set `bucket` to the name from step 1 (keep
`region` and `dynamodb_table` in sync). This is the **only** manual edit.

### 3. First apply

```bash
cd infra/terraform
terraform init                 # migrates to the S3 backend
cp terraform.tfvars.example terraform.tfvars   # optional: tweak sizing / alarm_email
terraform apply
```

`image_tag` defaults to `latest`; push an image first (or let CD do it) so the
instance has something to pull.

### 4. Wire CI/CD (turn the pipeline on)

`terraform output` gives the role ARNs. In GitHub **Settings → Variables →
Actions** set:

| Variable | Value |
|---|---|
| `AWS_REGION` | e.g. `us-east-1` |
| `ECR_REPOSITORY` | `powerhouse-cache` (the repo name) |
| `AWS_ROLE_ARN` | `gha_ci_role_arn` output → enables CD build+push |
| `AWS_DEPLOY_ROLE_ARN` | `gha_deploy_role_arn` output → enables CD deploy (app + infra) |

Also create a protected GitHub Environment named **`production`** (required
reviewers) — the deploy role's trust policy is pinned to it, and **both** the
`deploy-app` and `deploy-infra` jobs run in it.

> Only if you override `project`/`environment` from their defaults
> (`powerhouse-cache`/`prod`) in `*.tfvars`: also set repo variables `PROJECT`
> and `ENVIRONMENT` to the same values, because `deploy-app` locates the instance
> and the SSM tag parameter by the `<project>-<environment>` name.

## Deploy model (app deploys are decoupled from infra)

The desired image tag lives in an **SSM Parameter** (`/<project>-<env>/image_tag`),
not in user-data. Two independent paths:

- **App deploy (every green push to `main`, the common case).** CD's `deploy-app`
  job sets the SSM tag parameter to the new git-SHA and reruns the on-box
  `powerhouse-redeploy.sh` via **SSM run-command** — it pulls the new image and
  restarts the container. **No `terraform apply`, no instance replacement.**
- **Infra deploy (only when `infra/terraform/**` changes, or on a `v*` tag).** CD's
  `deploy-infra` job runs `terraform apply`. Because the tag is no longer templated
  into user-data, an image change never alters user-data, so infra apply doesn't
  churn the box. `image_tag` only *seeds* the SSM parameter on first create
  (`ignore_changes = [value]`), so an apply never reverts a live deploy.

On any instance replacement the fresh box reads the tag from SSM and pulls the
last-deployed image; the EBS data volume reattaches (WAL replays) and the Elastic
IP keeps the endpoint stable. **Rollback** = re-run CD against a previous SHA (its
image is still in ECR), which just rewrites the SSM param and reruns the script.

## Ops

```bash
# Connect (no SSH): audited SSM session.
aws ssm start-session --target $(terraform output -raw instance_id)

# Reach 6379 from your laptop WITHOUT exposing it — SSM port forward:
aws ssm start-session --target <id> \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["6379"],"localPortNumber":["6379"]}'
redis-cli -p 6379 PING
```

## Notes / roadmap

- **Memory & `/data`-disk alarms** need the CloudWatch agent (only CPU +
  status-check work off built-in metrics today). The engine's own `maxmemory`
  cap bounds RAM regardless.
- Pin provider versions are in [`versions.tf`](./versions.tf); the committed
  `.terraform.lock.hcl` locks exact provider hashes.
- `*.tfvars` and state are gitignored; `*.tfvars.example` and the lock file are
  committed.

Validated with `terraform fmt -check`, `terraform validate`, and `tfsec` (all
clean) — the same gates CI enforces.
