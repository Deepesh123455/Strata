# ─────────────────────────────────────────────────────────────────────────────
# Compute (Topology A): a single EC2 box running the container, with the WAL on
# a dedicated encrypted EBS volume that OUTLIVES the instance.
#
# Deploy model (DECOUPLED — app deploys never touch Terraform):
#   • The desired image tag lives in an SSM Parameter (below). CD's app-deploy job
#     updates that parameter and reruns the on-box redeploy script via SSM
#     run-command — NO terraform apply, NO instance churn.
#   • User-data no longer bakes the tag in, so an image change does not alter
#     user-data and therefore does not replace the instance. `terraform apply`
#     only replaces the box when the user-data SCRIPT itself changes (infra work).
#   • On any instance replacement the fresh box reads the tag from SSM and pulls
#     the last-deployed image, so infra and app state stay consistent. The EBS
#     data volume reattaches (WAL persists/replays); the Elastic IP is stable.
# Rollback = set the SSM tag param to a previous SHA + rerun the script (CD does
# this by re-running against that SHA; the old image is still in ECR).
# ─────────────────────────────────────────────────────────────────────────────

# Desired ECR image tag for the cache container. CD owns the LIVE value (updates
# it out-of-band on each app deploy); Terraform only SEEDS it on first create and
# then ignores changes, so an infra apply never reverts a deploy.
resource "aws_ssm_parameter" "image_tag" {
  name        = "/${local.name}/image_tag"
  description = "Desired ECR image tag for the cache container (owned by CD app deploys; read by the box on boot/redeploy)."
  type        = "String"
  value       = var.image_tag
  tags        = { Name = "${local.name}-image-tag" }

  lifecycle {
    ignore_changes = [value]
  }
}

# Dedicated, encrypted volume for /data (the WAL). Separate from the root disk so
# it survives instance replacement. retain on destroy of the *instance*; only an
# explicit destroy of this resource removes it.
# AWS-managed EBS encryption is enabled below; a CMK is unnecessary cost/ops for
# a single free-tier volume.
#tfsec:ignore:aws-ec2-volume-encryption-customer-key
resource "aws_ebs_volume" "data" {
  availability_zone = aws_subnet.public[0].availability_zone
  size              = var.data_volume_gb
  type              = "gp3"
  encrypted         = true

  tags = { Name = "${local.name}-data" }
}

resource "aws_eip" "cache" {
  domain   = "vpc"
  instance = aws_instance.cache.id
  tags     = { Name = local.name }

  depends_on = [aws_internet_gateway.main]
}

resource "aws_instance" "cache" {
  ami                    = data.aws_ssm_parameter.al2023.value
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.cache.id]
  iam_instance_profile   = aws_iam_instance_profile.instance.name

  # IMDSv2 only — blocks the SSRF-to-credential-theft class of attacks.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  root_block_device {
    encrypted   = true
    volume_size = 8
    volume_type = "gp3"
  }

  # Replaces the box only when the user-data SCRIPT changes (infra work) — NOT on
  # an image tag change, since the tag is no longer templated in (it's read from
  # SSM at boot/redeploy). This is what decouples app deploys from instance churn.
  user_data_replace_on_change = true
  user_data = templatefile("${path.module}/templates/user-data.sh.tftpl", {
    region        = var.aws_region
    ecr_registry  = "${local.account_id}.dkr.ecr.${var.aws_region}.amazonaws.com"
    ecr_repo_url  = aws_ecr_repository.main.repository_url
    tag_param     = aws_ssm_parameter.image_tag.name
    maxmemory_mb  = var.maxmemory_mb
    docker_memory = "${var.maxmemory_mb + 150}m"
    log_group     = aws_cloudwatch_log_group.app.name
    volume_serial = replace(aws_ebs_volume.data.id, "-", "")
  })

  tags = { Name = local.name }
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.cache.id
}
