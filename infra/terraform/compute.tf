# ─────────────────────────────────────────────────────────────────────────────
# Compute (Topology A): a single EC2 box running the container, with the WAL on
# a dedicated encrypted EBS volume that OUTLIVES the instance.
#
# Deploy model: image_tag is baked into user-data and `user_data_replace_on_change`
# is true, so `terraform apply -var image_tag=<sha>` (what CD runs) REPLACES the
# instance with a fresh one that pulls the new image. The EBS data volume detaches
# from the old box and reattaches to the new one, so the WAL persists and replays
# on boot. The Elastic IP keeps the endpoint stable across replacements.
# (In-place pull via SSM, with zero instance churn, is the production-grade
# upgrade — see README §Deploy model.)
# ─────────────────────────────────────────────────────────────────────────────

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

  user_data_replace_on_change = true
  user_data = templatefile("${path.module}/templates/user-data.sh.tftpl", {
    region        = var.aws_region
    ecr_registry  = "${local.account_id}.dkr.ecr.${var.aws_region}.amazonaws.com"
    image         = "${aws_ecr_repository.main.repository_url}:${var.image_tag}"
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
