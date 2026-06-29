# ─────────────────────────────────────────────────────────────────────────────
# Bootstrap — creates the REMOTE STATE backend for the root module.
#
# Chicken-and-egg: the root module stores its state in S3, but something has to
# create that S3 bucket first. This tiny module does exactly that, using LOCAL
# state (committed to .gitignore, not S3). Run it ONCE, by a human, then never
# again:
#
#   cd infra/terraform/bootstrap
#   terraform init
#   terraform apply -var 'state_bucket_name=<globally-unique-name>'
#
# Then put that SAME bucket name into ../backend.tf and `terraform init` the root.
# ─────────────────────────────────────────────────────────────────────────────

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

# The bucket that holds the root module's terraform.tfstate. Versioned (so a bad
# apply can be rolled back), KMS-encrypted, and totally private.
resource "aws_s3_bucket" "state" {
  bucket = var.state_bucket_name

  # State is the crown jewels — never let `terraform destroy` nuke it.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "aws:kms"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket                  = aws_s3_bucket.state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# DynamoDB lock table → two `terraform apply`s can never race on the same state.
resource "aws_dynamodb_table" "lock" {
  name         = var.lock_table_name
  billing_mode = "PAY_PER_REQUEST" # on-demand → ~$0 at this volume
  hash_key     = "LockID"

  attribute {
    name = "LockID"
    type = "S"
  }

  server_side_encryption {
    enabled = true
  }

  point_in_time_recovery {
    enabled = true
  }
}
