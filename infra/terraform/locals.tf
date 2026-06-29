data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

# Latest Amazon Linux 2023 AMI (x86_64) — resolved at plan time from the public
# SSM parameter AWS maintains, so we always launch a patched base image.
data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

locals {
  name         = "${var.project}-${var.environment}"
  account_id   = data.aws_caller_identity.current.account_id
  github_sub   = "repo:${var.github_owner}/${var.github_repo}"
  ecr_repo_arn = "arn:aws:ecr:${var.aws_region}:${local.account_id}:repository/${var.project}"
}
