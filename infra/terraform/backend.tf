# Remote state in S3 with DynamoDB locking. Created by ../bootstrap.
#
# NOTE: a backend block CANNOT use variables — these are literals on purpose.
# After running bootstrap, set `bucket` to the name you chose there (and keep
# `region` / `dynamodb_table` in sync). This is the ONE manual edit in the repo.
terraform {
  backend "s3" {
    bucket         = "powerhouse-cache-tfstate-CHANGEME"
    key            = "powerhouse-cache/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "powerhouse-cache-tflock"
    encrypt        = true
  }
}
