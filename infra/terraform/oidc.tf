# ─────────────────────────────────────────────────────────────────────────────
# GitHub OIDC — the heart of "no static AWS keys". GitHub Actions mints a
# short-lived OIDC token per run; AWS STS exchanges it for temporary creds, but
# ONLY for roles whose trust policy matches this exact repo (and, for deploys,
# the protected `production` environment). Nothing to leak, nothing to rotate.
# ─────────────────────────────────────────────────────────────────────────────

data "tls_certificate" "github" {
  url = "https://token.actions.githubusercontent.com"
}

resource "aws_iam_openid_connect_provider" "github" {
  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.github.certificates[length(data.tls_certificate.github.certificates) - 1].sha1_fingerprint]
}

# ── gha-ci-role: build job — push to ECR. Assumable from ANY ref of THIS repo ─
data "aws_iam_policy_document" "ci_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["${local.github_sub}:*"]
    }
  }
}

resource "aws_iam_role" "gha_ci" {
  name               = "${var.project}-gha-ci-role"
  assume_role_policy = data.aws_iam_policy_document.ci_assume.json
}

data "aws_iam_policy_document" "ci" {
  statement {
    sid       = "EcrAuth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid = "EcrPushPull"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
      "ecr:PutImage",
    ]
    resources = [aws_ecr_repository.main.arn]
  }
}

resource "aws_iam_role_policy" "ci" {
  name   = "${var.project}-gha-ci"
  role   = aws_iam_role.gha_ci.id
  policy = data.aws_iam_policy_document.ci.json
}

# ── gha-deploy-role: terraform apply. Assumable ONLY from `environment:production` ─
data "aws_iam_policy_document" "deploy_assume" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    # The key hardening: pinned to the protected environment, not just the repo,
    # so a random PR can never run a deploy.
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["${local.github_sub}:environment:${var.github_deploy_environment}"]
    }
  }
}

resource "aws_iam_role" "gha_deploy" {
  name               = "${var.project}-gha-deploy-role"
  assume_role_policy = data.aws_iam_policy_document.deploy_assume.json
}

# The deploy role manages exactly the services this module provisions. It is
# service-scoped (not *:*) and IAM/PassRole are restricted to this project's own
# resources. Tighten further from `terraform plan` denials over time.
data "aws_iam_policy_document" "deploy" {
  # Infra services. Resource-level scoping for EC2/VPC/etc. is impractical at
  # apply time, so these are service-scoped — the trust policy (protected env +
  # manual approval) is the real boundary here.
  #tfsec:ignore:aws-iam-no-policy-wildcards deploy role is gated to environment:production + manual approval; service-scoped is intentional.
  statement {
    sid = "Infra"
    actions = [
      "ec2:*",
      "ecr:*",
      "logs:*",
      "sns:*",
      "cloudwatch:*",
      "dlm:*",
    ]
    resources = ["*"]
  }
  statement {
    sid     = "ReadSsmAmi"
    actions = ["ssm:GetParameter", "ssm:GetParameters"]
    # Reading the public AL2023 AMI SSM parameter requires a wildcard resource.
    #tfsec:ignore:aws-iam-no-policy-wildcards
    resources = ["*"]
  }
  # ── App-deploy path (decoupled from infra) ──────────────────────────────────
  # CD's deploy-app job assumes THIS role (it also runs in environment:production)
  # to set the desired image tag and trigger an in-place container redeploy via
  # SSM run-command — no terraform, no instance churn.
  statement {
    sid = "ManageImageTagParam"
    actions = [
      "ssm:PutParameter",
      "ssm:AddTagsToResource",
      "ssm:ListTagsForResource",
      "ssm:DeleteParameter",
    ]
    resources = [aws_ssm_parameter.image_tag.arn]
  }
  statement {
    sid     = "AppDeploySendCommand"
    actions = ["ssm:SendCommand"]
    resources = [
      aws_instance.cache.arn,
      "arn:aws:ssm:${var.aws_region}::document/AWS-RunShellScript",
    ]
  }
  statement {
    sid = "AppDeployReadCommand"
    actions = [
      "ssm:GetCommandInvocation",
      "ssm:ListCommandInvocations",
    ]
    # Command-invocation ids are minted at run time and can't be pre-scoped.
    #tfsec:ignore:aws-iam-no-policy-wildcards
    resources = ["*"]
  }
  # IAM, scoped to this project's roles/policies/profiles only.
  statement {
    sid = "ManageProjectIam"
    actions = [
      "iam:CreateRole",
      "iam:DeleteRole",
      "iam:GetRole",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:ListInstanceProfilesForRole",
      "iam:PutRolePolicy",
      "iam:GetRolePolicy",
      "iam:DeleteRolePolicy",
      "iam:AttachRolePolicy",
      "iam:DetachRolePolicy",
      # Lets a CD-driven apply update a role's TRUST policy (e.g. re-pin the OIDC
      # sub). Without it, changing any assume_role_policy here is denied and must
      # fall back to a human apply. Scoped to this project's roles below.
      "iam:UpdateAssumeRolePolicy",
      "iam:CreateInstanceProfile",
      "iam:DeleteInstanceProfile",
      "iam:GetInstanceProfile",
      "iam:AddRoleToInstanceProfile",
      "iam:RemoveRoleFromInstanceProfile",
      "iam:CreateOpenIDConnectProvider",
      "iam:GetOpenIDConnectProvider",
      "iam:TagOpenIDConnectProvider",
      "iam:UpdateOpenIDConnectProviderThumbprint",
    ]
    resources = [
      "arn:aws:iam::${local.account_id}:role/${var.project}-*",
      "arn:aws:iam::${local.account_id}:instance-profile/${var.project}-*",
      "arn:aws:iam::${local.account_id}:oidc-provider/token.actions.githubusercontent.com",
    ]
  }
  statement {
    sid       = "PassProjectRoles"
    actions   = ["iam:PassRole"]
    resources = ["arn:aws:iam::${local.account_id}:role/${var.project}-*"]
  }
  # Remote state.
  statement {
    sid     = "StateBucket"
    actions = ["s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    # The exact state bucket name is chosen at bootstrap; scope by project prefix.
    #tfsec:ignore:aws-iam-no-policy-wildcards
    resources = ["arn:aws:s3:::${var.project}-tfstate-*", "arn:aws:s3:::${var.project}-tfstate-*/*"]
  }
  statement {
    sid       = "StateLock"
    actions   = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem"]
    resources = ["arn:aws:dynamodb:${var.aws_region}:${local.account_id}:table/${var.lock_table_name}"]
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "${var.project}-gha-deploy"
  role   = aws_iam_role.gha_deploy.id
  policy = data.aws_iam_policy_document.deploy.json
}
