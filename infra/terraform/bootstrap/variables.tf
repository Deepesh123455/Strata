variable "aws_region" {
  description = "Region for the state bucket + lock table."
  type        = string
  default     = "us-east-1"
}

variable "state_bucket_name" {
  description = "Globally-unique S3 bucket name for Terraform remote state. Must match the `bucket` in ../backend.tf."
  type        = string
}

variable "lock_table_name" {
  description = "DynamoDB table name for state locking. Must match the `dynamodb_table` in ../backend.tf."
  type        = string
  default     = "powerhouse-cache-tflock"
}
