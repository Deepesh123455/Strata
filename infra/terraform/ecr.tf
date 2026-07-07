# Private image registry for the cache. Scan-on-push, immutable tags (a given
# :sha can never be overwritten → deploys/rollbacks are unambiguous), and a
# lifecycle policy so old images don't accumulate cost.
# AWS-managed KMS encryption is enabled below; a CMK is unnecessary cost/ops here.
#tfsec:ignore:aws-ecr-repository-customer-key
resource "aws_ecr_repository" "main" {
  name                 = var.project
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
  }
}

resource "aws_ecr_lifecycle_policy" "main" {
  repository = aws_ecr_repository.main.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 7 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      },
      {
        # Retain the 20 most-recent images by push time, regardless of tag.
        # ECR caps tagPrefixList at 10 entries, and our real tags are 12-char
        # hex git SHAs (16 possible prefixes) plus `latest`/`v*`, which a prefix
        # list can't faithfully cover — so use tagStatus="any" as the catch-all.
        rulePriority = 2
        description  = "Keep only the last 20 images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = 20
        }
        action = { type = "expire" }
      },
    ]
  })
}
