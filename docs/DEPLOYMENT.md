# Powerhouse Cache — Production Deployment & DevOps Blueprint

> How we ship the Powerhouse Cache (`DataPlane`) to AWS the way a real
> infrastructure team would: containerized with Docker, provisioned with
> Terraform, delivered through GitHub Actions CI/CD authenticated by **GitHub
> OIDC** (zero long-lived AWS keys), observable, recoverable, and locked down at
> every layer.

This is the companion to [`ARCHITECTURE.md`](./ARCHITECTURE.md). Architecture
explains *what the code does*; this document explains *how it runs in
production* and is the implementation guide for the pipelines and infrastructure
we are about to build.

---

## Table of Contents

1. [Deployment goals & non-negotiables](#1-deployment-goals--non-negotiables)
2. [Target AWS architecture (HLD)](#2-target-aws-architecture-hld)
3. [Why these choices](#3-why-these-choices)
4. [Containerization — Docker](#4-containerization--docker)
5. [Infrastructure as Code — Terraform](#5-infrastructure-as-code--terraform)
6. [Identity & secrets — GitHub OIDC, no static keys](#6-identity--secrets--github-oidc-no-static-keys)
7. [CI/CD — GitHub Actions pipelines](#7-cicd--github-actions-pipelines)
8. [Security — defense in depth](#8-security--defense-in-depth)
9. [Observability — logs, metrics, alarms](#9-observability--logs-metrics-alarms)
10. [Persistence, backup & disaster recovery](#10-persistence-backup--disaster-recovery)
11. [Environments & promotion flow](#11-environments--promotion-flow)
12. [Cost model (Free Tier → scale)](#12-cost-model-free-tier--scale)
13. [Runbooks](#13-runbooks)
14. [Implementation checklist](#14-implementation-checklist)

---

## 1. Deployment goals & non-negotiables

| Goal | Non-negotiable rule |
|---|---|
| **Zero standing cloud credentials** | No AWS access keys in GitHub, ever. All AWS auth via **OIDC + short-lived STS tokens**. |
| **Everything reproducible** | 100% of infrastructure in Terraform, in git. No console click-ops. |
| **Immutable, minimal images** | Multi-stage Docker, distroless/scratch final stage, non-root, pinned digests. |
| **The cache is NEVER publicly reachable** | The data plane has no AUTH/TLS (see ARCHITECTURE §6.6). It lives on a **private subnet**; only the app tier in the same VPC can reach `:6379`. |
| **Durable across restarts & deploys** | The WAL lives on a persistent volume; deploys are graceful (SIGTERM → drain → fsync → close). |
| **Every change gated** | Lint + vet + race tests + vuln scan + image scan + IaC scan must pass before anything ships. |
| **Least privilege everywhere** | IAM roles scoped per pipeline job; security groups scoped per port/source. |
| **Auditable & rollback-able** | Tagged images by git SHA; Terraform state versioned; one-command rollback. |

---

## 2. Target AWS architecture (HLD)

We present **two deployment topologies**. Start with **Topology A** (single EC2,
Free-Tier-friendly — matches the project's "1 GB AWS Free Tier box" goal), and
graduate to **Topology B** (ECS Fargate + EFS) when you need HA and managed scaling.

### Topology A — Single EC2 + Docker (Free Tier start)

```
                          GitHub Actions  ──OIDC──►  AWS STS  ──►  short-lived role
                                │ build & push                          │ terraform apply
                                ▼                                       ▼
                          Amazon ECR  ◄──────────────────────  EC2 pulls image on deploy
                          (powerhouse-cache:sha)
   ┌──────────────────────────── VPC (10.0.0.0/16) ─────────────────────────────────┐
   │                                                                                 │
   │   ┌── Public subnet ──────────────┐      ┌── Private subnet ──────────────────┐ │
   │   │  Your app servers / bastion   │      │  EC2: powerhoused (Docker)         │ │
   │   │  (the only RESP clients)      │ ───► │  • container :6379                 │ │
   │   │                               │ SG   │  • WAL on attached EBS volume      │ │
   │   └───────────────────────────────┘ rule │  • CloudWatch agent (logs/metrics) │ │
   │            ▲ optional bastion (SSM)       └────────────────────────────────────┘ │
   │            │                                            │ EBS snapshot (backup)   │
   └────────────┼────────────────────────────────────────────────────────────────────┘
                │
          Operators via AWS SSM Session Manager (NO public SSH, no key pairs)
```

- **No public IP on the cache instance.** Reach it for ops only via **SSM Session
  Manager** (no inbound SSH, no `0.0.0.0/0:22`).
- Security group on the cache allows `:6379` **only** from the app-tier security
  group — not a CIDR, a *security-group reference*.
- WAL persisted on a dedicated **EBS** volume (survives instance replacement);
  nightly **EBS snapshots** for DR.

### Topology B — ECS Fargate + EFS (HA / scale)

```
   GitHub Actions ─OIDC─► ECR (image)  +  Terraform ─► ECS Service (Fargate)
                                                          │ desired_count=N
   ┌──────────── VPC ────────────────────────────────────┼──────────────────────┐
   │  Private subnets (multi-AZ)                          ▼                       │
   │    ┌── Fargate task ──┐   ┌── Fargate task ──┐   each task mounts EFS access │
   │    │ powerhoused :6379│   │ powerhoused :6379│   point → /data (the WAL)     │
   │    └────────┬─────────┘   └────────┬─────────┘                               │
   │             └──────────┬───────────┘                                         │
   │              Network Load Balancer (internal, TCP :6379)                     │
   │                         ▲ app tier connects here                             │
   └─────────────────────────┼─────────────────────────────────────────────────-─┘
                    CloudWatch Logs + Container Insights + Alarms
```

> **Caveat for Topology B:** the current single-node WAL model is **not** built
> for multiple writers sharing one EFS file. Until replication lands
> (ARCHITECTURE §7), run Fargate at **`desired_count = 1`** with EFS for durable
> storage + fast failover, *not* as an active-active cluster. Document this
> constraint loudly in the Terraform variables.

---

## 3. Why these choices

| Decision | Rationale |
|---|---|
| **ECR over Docker Hub** | Private, IAM-controlled, in-region (no egress cost / rate limits), integrates with image scanning. |
| **OIDC over IAM access keys** | No long-lived secret to leak or rotate; tokens are minted per-run, scoped, and expire in minutes. The single biggest CI/CD security win. |
| **Terraform over CDK/console** | Declarative, provider-agnostic, huge ecosystem, plan/apply review gate, remote state with locking. |
| **EC2 first, Fargate later** | The project explicitly targets a Free-Tier 1 GB box; EC2+Docker is the cheapest path. Fargate is the clean HA upgrade once replication exists. |
| **Distroless/scratch final image** | A Go static binary needs no OS. Minimal image = tiny attack surface, fast pulls, fewer CVEs. |
| **SSM Session Manager over SSH** | No open port 22, no key-pair management, full audit log of every session in CloudTrail. |
| **EBS/EFS for the WAL** | The WAL is the durability story; it must outlive any single container/instance. |

---

## 4. Containerization — Docker

### 4.1 Multi-stage Dockerfile (planned: `DataPlane/Dockerfile`)

```dockerfile
# ---- Stage 1: build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache deps first (go.work + go.mod) for fast incremental builds
COPY go.work ./
COPY DataPlane/go.mod DataPlane/go.sum* DataPlane/
RUN cd DataPlane && go mod download
COPY DataPlane/ DataPlane/
# Static, stripped, reproducible build
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN cd DataPlane && go build -ldflags="-s -w" -o /out/powerhoused ./cmd/powerhoused

# ---- Stage 2: runtime (distroless, non-root) ----
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/powerhoused /powerhoused
# WAL directory; will be a mounted volume in prod
VOLUME ["/data"]
EXPOSE 6379
USER nonroot:nonroot
ENTRYPOINT ["/powerhoused"]
```

Key properties:

- **`CGO_ENABLED=0` + distroless `static`** → a fully static binary on a base with
  no shell, no package manager, no libc — minimal CVE surface and nothing for an
  attacker to pivot into.
- **`-trimpath`, `-ldflags="-s -w"`** → reproducible, stripped, smaller binary.
- **`USER nonroot`** → never runs as root in the container.
- **`VOLUME /data`** → matches `walPath = ./data/powerhouse.wal`; mount the EBS/EFS
  volume here so the WAL persists. *(Set the working dir / path so the process
  writes to `/data`.)*
- **`.dockerignore`** excludes `.git`, `docs/`, `data/*.wal`, test caches.

### 4.2 Runtime configuration (12-factor)

| Setting | Mechanism | Default |
|---|---|---|
| Memory cap | `POWERHOUSE_MAXMEMORY_MB` env (or `-maxmemory` flag) | unlimited; **set to ~700 on a 1 GB box** |
| Port | fixed `:6379` | — |
| WAL path | `./data/powerhouse.wal` (mount `/data`) | — |
| Fsync policy | code default `SyncEverySec` | — |

Container is stopped with **SIGTERM**, which `main.go` already handles:
drain connections → flush+fsync WAL → exit. Set the orchestrator's stop grace
period generously (≥30 s) so draining completes.

### 4.3 Local dev: `docker-compose.yml` (planned)

A compose file runs `powerhoused` + a `redis-cli`-based smoke test, mounting a
local `./data` volume, so contributors validate the exact production image
locally before pushing.

---

## 5. Infrastructure as Code — Terraform

### 5.1 Layout (planned: `infra/terraform/`)

```
infra/terraform/
├── backend.tf          # remote state: S3 bucket + DynamoDB lock table
├── providers.tf        # aws provider, pinned version, default tags
├── variables.tf        # region, env, instance_type, maxmemory_mb, image_tag, ...
├── network.tf          # VPC, public/private subnets, route tables, NAT, IGW
├── security.tf         # security groups (cache SG, app SG), IAM instance profile
├── ecr.tf              # ECR repository + lifecycle policy (keep last N images)
├── compute.tf          # EC2 + EBS (Topology A)  OR  ecs.tf (Topology B)
├── observability.tf    # CloudWatch log group, alarms, dashboard
├── oidc.tf             # GitHub OIDC provider + the CI/CD assume-role
├── backup.tf           # EBS snapshot / AWS Backup plan
├── outputs.tf          # cache private DNS, ECR URL, role ARN
└── envs/
    ├── dev.tfvars
    ├── staging.tfvars
    └── prod.tfvars
```

### 5.2 Remote state (`backend.tf`)

- **S3 bucket** (versioned, SSE-KMS encrypted, public access blocked) for state.
- **DynamoDB table** for state locking → no two `apply`s race.
- State is the crown jewels (it can contain sensitive outputs): bucket policy
  denies all but the CI role + named admins; bucket has versioning for rollback.

### 5.3 What each file provisions

- **`network.tf`** — a `/16` VPC, ≥2 private subnets (multi-AZ) for the cache, a
  public subnet for NAT + app/bastion, an Internet Gateway, a **NAT Gateway** (so
  the private cache can pull from ECR / reach CloudWatch without a public IP), and
  route tables. Optionally **VPC endpoints** for ECR/S3/CloudWatch/SSM to drop the
  NAT (cheaper + more private).
- **`security.tf`** — two security groups:
  - `cache_sg`: **inbound `6379` only from `app_sg`** (SG reference, not CIDR);
    no inbound `22`. Egress to ECR/CloudWatch (or via VPC endpoints).
  - `app_sg`: assigned to whatever connects to the cache.
  - An **IAM instance profile** giving the EC2 box only: pull-from-ECR, write
    CloudWatch logs/metrics, and SSM-managed-instance permissions.
- **`ecr.tf`** — private repo `powerhouse-cache`; **image scan-on-push** enabled;
  lifecycle policy to expire untagged/old images (cost + hygiene); immutable tags.
- **`compute.tf`** (Topology A) — an EC2 instance (e.g. `t3.micro`/`t4g.small`) in
  a private subnet, the instance profile attached, a dedicated **EBS** volume for
  `/data`, and **user-data** that installs Docker + the CloudWatch agent and runs
  the container with `--restart=always`, `-e POWERHOUSE_MAXMEMORY_MB=700`, the EBS
  volume mounted at `/data`, and the SIGTERM grace period set. **No public IP.**
- **`oidc.tf`** — the GitHub OIDC identity provider + the deploy role (see §6).
- **`observability.tf`** / **`backup.tf`** — see §9 / §10.

### 5.4 Terraform discipline

- Provider + module versions **pinned**; `terraform.lock.hcl` committed.
- `terraform fmt -check`, `terraform validate`, and **`tflint` + `tfsec`/`checkov`**
  run in CI on every PR (see §7).
- `plan` is posted as a PR comment for human review; `apply` only runs on merge to
  the env branch, only via the OIDC role.
- Default tags on every resource: `Project=powerhouse-cache`, `Env`, `ManagedBy=terraform`, `GitSha`.

---

## 6. Identity & secrets — GitHub OIDC, no static keys

**The single most important security decision in the whole pipeline.** GitHub
Actions never holds an AWS access key. Instead:

```
GitHub Actions job
   │  1. requests an OIDC token from GitHub's identity provider
   │     (subject = repo:Deepesh123455/Strata:ref:refs/heads/main, etc.)
   ▼
AWS IAM OIDC provider  (token.actions.githubusercontent.com)
   │  2. trust policy verifies issuer + audience (sts.amazonaws.com) + `sub` claim
   ▼
sts:AssumeRoleWithWebIdentity → short-lived (e.g. 15–60 min) credentials
   │  3. scoped to exactly what this job needs (push to ECR / run terraform)
   ▼
AWS API calls (ECR push, terraform apply) — credentials expire automatically
```

### 6.1 The OIDC provider + role (Terraform `oidc.tf`)

- One IAM OIDC provider for `token.actions.githubusercontent.com`.
- A **deploy role** whose **trust policy** is locked down to:
  - `aud` = `sts.amazonaws.com`
  - `sub` = the **specific repo and refs** allowed, e.g.
    `repo:Deepesh123455/Strata:ref:refs/heads/main` and
    `repo:Deepesh123455/Strata:environment:prod`. **Never** use a wildcard
    `repo:*` — that would let any repo assume the role.
- Two scoped roles is even better:
  - `gha-ci-role` — read-only + ECR push (for the CI build job).
  - `gha-deploy-role` — Terraform apply permissions, assumable **only** from the
    protected `prod`/`main` environment.

### 6.2 What replaces "secrets"

- No `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in GitHub. Period.
- The only repo-level config is the **role ARN** (not a secret) and the region.
- App config (`POWERHOUSE_MAXMEMORY_MB`) is non-sensitive; any future real secrets
  (e.g. an AUTH password once implemented) go in **AWS Secrets Manager / SSM
  Parameter Store (SecureString)**, fetched at deploy time by the scoped role —
  never committed.

---

## 7. CI/CD — GitHub Actions pipelines

Planned workflows under `.github/workflows/`. Principle: **CI on every PR (fast,
no cloud); CD on merge (OIDC, gated by environment protection rules).**

### 7.1 `ci.yml` — runs on every pull request

Jobs (parallel where possible), all on `ubuntu-latest` (avoids the dev box's
AppLocker `go test` block noted in ARCHITECTURE §5.8):

1. **lint** — `golangci-lint` (vet, staticcheck, gofmt, ineffassign, etc.).
2. **test** — `go test -race -coverprofile=coverage.out ./...` in `DataPlane/`.
   The `-race` detector is essential given the heavy concurrency (32 shards,
   atomics, goroutines). Upload coverage; fail under a threshold.
3. **build** — `go build ./...` + build the Docker image (no push) to prove the
   Dockerfile works.
4. **govulncheck** — Go vulnerability database scan of dependencies + stdlib.
5. **iac-scan** — `terraform fmt -check`, `terraform validate`, `tflint`,
   `tfsec`/`checkov` on `infra/terraform/`.
6. **terraform-plan** — assume `gha-ci-role` via OIDC, `terraform plan`, post the
   plan as a PR comment for review.

Branch protection: PR cannot merge unless all required checks pass + 1 review.

### 7.2 `cd.yml` — runs on merge to `main` (and tags)

```
permissions:
  id-token: write   # REQUIRED for OIDC
  contents: read
```

Jobs, sequential:

1. **build-and-push** —
   - `aws-actions/configure-aws-credentials` with `role-to-assume: <gha-ci-role-arn>` (OIDC).
   - Build the image, tag it `:<git-sha>` **and** `:latest`, push to ECR.
   - ECR **scan-on-push** runs automatically; optionally `trivy` scan the image and
     fail on HIGH/CRITICAL CVEs.
2. **deploy** — gated by GitHub **Environment = prod** (manual approval + required
   reviewers + only `main`):
   - Assume `gha-deploy-role` via OIDC.
   - `terraform apply` with `-var image_tag=<git-sha>` (Topology A: triggers the EC2
     to pull + restart the container; Topology B: updates the ECS task definition
     and rolls the service).
3. **smoke-test** — from inside the VPC (a small runner or SSM run-command):
   `SET`/`GET`/`TTL` round-trip against the freshly deployed instance; fail the
   deploy (and auto-rollback) if it doesn't answer correctly.

### 7.3 Deployment strategy & rollback

- **Topology A:** image is immutable per SHA; deploy = pull new tag + `docker run`
  the new container after the old one drains (graceful SIGTERM). Rollback =
  re-deploy the previous SHA tag (still in ECR).
- **Topology B:** ECS rolling update with health checks + circuit breaker
  (`deployment_circuit_breaker { rollback = true }`) → a failing task set auto-rolls
  back to the last healthy task definition.
- Every deploy is traceable to a git SHA; rollback is a one-liner (`terraform apply
  -var image_tag=<previous-sha>`).

### 7.4 Supply-chain hardening of the pipeline itself

- All third-party Actions **pinned to a commit SHA**, not a moving tag.
- `permissions:` block set to least privilege per workflow (default read-only;
  `id-token: write` only where OIDC is used).
- Dependabot for Go modules, Docker base images, and GitHub Actions.
- Optionally generate an **SBOM** (`syft`) and **sign the image** (`cosign`,
  keyless via the same OIDC) for full provenance.

---

## 8. Security — defense in depth

Layered, because the data plane itself has no AUTH/TLS (ARCHITECTURE §6.6):

| Layer | Control |
|---|---|
| **Network** | Cache on a **private subnet**, no public IP. SG allows `:6379` only from the app SG. No inbound SSH (SSM only). |
| **Identity** | GitHub OIDC, no static AWS keys. IAM roles least-privilege, scoped per job and per `sub` claim. |
| **Host** | SSM Session Manager (audited, no port 22). Patched AMI. CloudWatch agent only. |
| **Container** | Distroless, non-root, read-only root filesystem (except `/data`), no shell, pinned digest, scanned on push. |
| **Application** | Parser bounds + overflow guards, `maxmemory` cap, command-size cap, read-deadline DoS guard, per-connection panic firewall, CRC on WAL records (all already in code — ARCHITECTURE §6.6). |
| **Data at rest** | EBS/EFS **encrypted with KMS**; S3 state bucket SSE-KMS; ECR encrypted. |
| **Data in transit** | All traffic stays inside the VPC; if a client must cross trust boundaries, front it with a TLS-terminating proxy (`stunnel`/NLB+ACM) — never plaintext RESP over the internet. |
| **Secrets** | None in git. Future secrets in Secrets Manager / SSM SecureString. `.gitignore` already blocks `.env`, `*.pem`, `*.key`, `secrets/`. |
| **Audit** | CloudTrail on all API calls; SSM session logging; Terraform state versioned. |
| **Scanning** | `govulncheck` (deps), `trivy`/ECR-scan (image), `tfsec`/`checkov` (IaC), CodeQL (optional, Go). |

> **Hard rule, repeated because it's the #1 risk:** never put the cache in a public
> subnet or open `:6379` to `0.0.0.0/0`. An unauthenticated Redis-compatible
> endpoint on the internet is mass-exploited within minutes. The SG must reference
> the app security group, full stop.

---

## 9. Observability — logs, metrics, alarms

- **Logs:** the process already prints structured-ish `[SYSTEM]/[ERROR]/[NETWORK]/[PANIC]`
  lines to stdout. The CloudWatch agent (EC2) or the `awslogs`/Fargate driver ships
  stdout to a **CloudWatch Log Group** (`/powerhouse-cache/<env>`), with retention set.
- **Metrics:** EC2/Container Insights for CPU, **memory** (critical — watch it
  against `POWERHOUSE_MAXMEMORY_MB`), network, disk (WAL growth on `/data`).
- **Alarms** (CloudWatch → SNS → email/Slack):
  - Memory > 85% of cap (eviction churn / undersized box).
  - Disk on `/data` > 80% (WAL growing — see §10 compaction).
  - Instance/task unhealthy or restart loop.
  - No connections / connection errors spiking.
- **Future app metrics:** once an `INFO`/metrics endpoint is added at the server
  layer (ARCHITECTURE §7), export ops/sec, hit ratio, evictions, WAL seq, and p99
  latency to CloudWatch or Prometheus.

---

## 10. Persistence, backup & disaster recovery

- **The WAL is the durability story.** It lives on a dedicated **encrypted EBS
  volume** (Topology A) or **EFS access point** (Topology B), mounted at `/data`,
  so it survives container/instance replacement.
- **Graceful deploys lose nothing:** SIGTERM → drain → `walLog.Close()` (flush +
  fsync) is already implemented in `main.go`.
- **Crash safety is built in:** replay truncates the torn tail (ARCHITECTURE §6.4).
- **Backups:** nightly **EBS snapshots** (or **AWS Backup** plan) of the `/data`
  volume, retained per policy (e.g. 7 daily / 4 weekly), cross-region copy for prod.
- **Restore drill (DR):** provision a new instance, attach a volume restored from
  the latest snapshot, start the container → it replays the WAL on boot. **Test
  this restore quarterly** — an untested backup is not a backup.
- **Known scaling risk — WAL growth:** there is no compaction/snapshotting yet
  (ARCHITECTURE §7). Until it lands, **monitor `/data` disk usage** (alarm at 80%)
  and size the volume with headroom. Compaction is the top infra-relevant item on
  the roadmap; once added, recovery time and disk both stop growing unbounded.

---

## 11. Environments & promotion flow

```
feature branch ──PR──► CI (lint/test/scan/plan) ──review──► merge to main
                                                                  │
                                                          CD: build+push ECR
                                                                  │
                                              ┌── deploy to STAGING (auto) ──┐
                                              │   smoke test passes          │
                                              ▼                              │
                                   manual approval (GitHub Environment: prod)│
                                              ▼                              │
                                       deploy to PROD ◄──────────────────────┘
```

- Three Terraform workspaces / `*.tfvars`: `dev`, `staging`, `prod` — identical
  infra, different sizing (`instance_type`, `maxmemory_mb`, `desired_count`,
  snapshot retention).
- **prod** is a GitHub *protected environment*: required reviewers + only deployable
  from `main` + the OIDC `sub` claim pinned to `environment:prod`.

---

## 12. Cost model (Free Tier → scale)

| Item | Free Tier / start | At scale |
|---|---|---|
| Compute | 1× `t3.micro`/`t4g.small` (Free Tier eligible) | Larger EC2 or Fargate `desired_count` |
| Storage | 1× small encrypted EBS for `/data` | Bigger EBS / EFS, snapshot retention |
| ECR | a few small images (lifecycle-pruned) | same, pruned |
| NAT Gateway | the main non-free cost — **prefer VPC endpoints** for ECR/S3/CloudWatch/SSM to avoid it | endpoints |
| CloudWatch | logs/metrics within free allotment + retention caps | scoped retention |
| State | tiny S3 + DynamoDB | negligible |

The biggest sneaky cost on Free Tier is the **NAT Gateway**; using **VPC
endpoints** for ECR/S3/SSM/CloudWatch lets the private instance pull images and
ship logs without a NAT, keeping the bill near zero. Set **AWS Budgets** alarms.

---

## 13. Runbooks

**Deploy a new version**
1. Merge PR to `main`. CD builds `:<sha>`, pushes to ECR, deploys to staging.
2. Verify staging smoke test green. Approve the `prod` environment in GitHub.
3. CD `terraform apply -var image_tag=<sha>`; smoke test runs; done.

**Roll back**
- `terraform apply -var image_tag=<previous-sha>` (Topology A) or let ECS circuit
  breaker auto-roll (Topology B). Image is still in ECR.

**Restore from backup (DR)**
1. Identify the latest good EBS snapshot of `/data`.
2. `terraform apply` with the snapshot ID → new instance + restored volume.
3. Container boots, replays the WAL, serves traffic. Validate with a smoke test.

**Connect for ops (no SSH)**
- `aws ssm start-session --target <instance-id>` → `docker logs powerhoused`.

**Memory pressure alarm fires**
- Check eviction churn; raise `POWERHOUSE_MAXMEMORY_MB` only if the box has RAM,
  else scale the instance. Confirm the cap is set (unset = unlimited = OOM risk).

**Disk alarm on `/data`**
- WAL has grown (no compaction yet). Short term: grow the EBS volume. Track the
  compaction roadmap item.

---

## 14. Implementation checklist

Concrete artifacts to build, in dependency order:

- [ ] `DataPlane/Dockerfile` (multi-stage, distroless, non-root) + `.dockerignore`
- [ ] `docker-compose.yml` for local validation + a `Makefile` (build/test/run/scan)
- [ ] `DataPlane/go.sum` present and committed (needed for reproducible Docker builds)
- [ ] `infra/terraform/` skeleton: backend (S3+DynamoDB), providers, variables
- [ ] `network.tf` (VPC, private/public subnets, NAT **or** VPC endpoints)
- [ ] `security.tf` (cache_sg ← app_sg on 6379 only; instance profile; **no SSH**)
- [ ] `ecr.tf` (repo, scan-on-push, lifecycle policy, immutable tags)
- [ ] `oidc.tf` (GitHub OIDC provider + scoped `gha-ci-role` / `gha-deploy-role` with `sub` pinned to the repo/refs)
- [ ] `compute.tf` (EC2 + encrypted EBS + user-data) **or** `ecs.tf` (Fargate + EFS, `desired_count=1`)
- [ ] `observability.tf` (log group, alarms, SNS) + `backup.tf` (EBS snapshots / AWS Backup)
- [ ] `.github/workflows/ci.yml` (lint, `go test -race`, build, govulncheck, IaC scan, plan-on-PR)
- [ ] `.github/workflows/cd.yml` (OIDC build+push to ECR, env-gated terraform apply, smoke test, rollback)
- [ ] Branch protection on `main` + protected `prod` environment with required reviewers
- [ ] Dependabot config; pin all Actions to commit SHAs
- [ ] (Optional) `cosign` image signing + `syft` SBOM, CodeQL
- [ ] Quarterly DR restore drill documented and scheduled

> **One correctness note for whoever wires the WAL volume:** `walPath` is
> `./data/powerhouse.wal` relative to the process working directory. Make sure the
> container's working directory (and the mounted volume) line up so the WAL lands
> on the persistent `/data` mount — otherwise durability silently writes to the
> ephemeral container layer and is lost on every restart. Verify with a
> SET → restart container → GET test before calling the deploy done.
</content>
