provider "aws" {
  region = var.aws_region

  # Stamped on every resource that supports tagging — one place, no drift.
  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}
