# ─────────────────────────────────────────────────────────────────────────────
# Security groups + the EC2 instance IAM role.
# ─────────────────────────────────────────────────────────────────────────────

# Clients (your app tier) get THIS security group. The cache only ever trusts
# this SG as a source — never a CIDR — so "who may talk to the cache" is an
# identity, not an IP range that can drift.
resource "aws_security_group" "app" {
  name        = "${local.name}-app"
  description = "App tier / RESP clients allowed to reach the cache."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-app" }
}

resource "aws_security_group" "cache" {
  name        = "${local.name}-cache"
  description = "Powerhouse cache: 6379 from app SG only, no inbound SSH."
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-cache" }
}

# The ONLY inbound rule: RESP on 6379, sourced from the app SG. No SSH, ever.
resource "aws_vpc_security_group_ingress_rule" "cache_resp" {
  security_group_id            = aws_security_group.cache.id
  description                  = "RESP from the app tier only"
  referenced_security_group_id = aws_security_group.app.id
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
}

# Egress to anywhere — required in public-free mode to pull from ECR and ship
# logs to CloudWatch over the Internet Gateway.
#tfsec:ignore:aws-ec2-no-public-egress-sgr public-free mode pulls ECR/logs over IGW; inbound is the controlled side.
resource "aws_vpc_security_group_egress_rule" "cache_all" {
  security_group_id = aws_security_group.cache.id
  description       = "Outbound to ECR / CloudWatch / SSM"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# ── EC2 instance role: pull ECR, write logs, be SSM-managed ──────────────────
data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name               = "${local.name}-instance"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

# SSM Session Manager (no SSH, fully audited).
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# Read-only pull from ECR (auth token is account-wide; pulls are repo-scoped).
data "aws_iam_policy_document" "instance" {
  statement {
    sid       = "EcrAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid = "EcrPull"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
    ]
    resources = [local.ecr_repo_arn]
  }
  statement {
    sid = "WriteLogs"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    # Log-stream names are created at runtime and can't be enumerated; ":*"
    # scopes this to streams within the cache's own log group.
    #tfsec:ignore:aws-iam-no-policy-wildcards
    resources = ["${aws_cloudwatch_log_group.app.arn}:*"]
  }
  # Read the desired image tag the redeploy script pulls (boot + SSM redeploys).
  statement {
    sid       = "ReadImageTagParam"
    actions   = ["ssm:GetParameter"]
    resources = [aws_ssm_parameter.image_tag.arn]
  }
}

resource "aws_iam_role_policy" "instance" {
  name   = "${local.name}-instance"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.instance.json
}

resource "aws_iam_instance_profile" "instance" {
  name = "${local.name}-instance"
  role = aws_iam_role.instance.name
}

# ── IAM role used by VPC Flow Logs to write to CloudWatch ────────────────────
data "aws_iam_policy_document" "flow_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "flow_logs" {
  name               = "${local.name}-flow-logs"
  assume_role_policy = data.aws_iam_policy_document.flow_assume.json
}

data "aws_iam_policy_document" "flow_logs" {
  statement {
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogStreams",
    ]
    # Scoped to the flow-log group's own (runtime-named) streams.
    #tfsec:ignore:aws-iam-no-policy-wildcards
    resources = ["${aws_cloudwatch_log_group.flow.arn}:*"]
  }
}

resource "aws_iam_role_policy" "flow_logs" {
  name   = "${local.name}-flow-logs"
  role   = aws_iam_role.flow_logs.id
  policy = data.aws_iam_policy_document.flow_logs.json
}
