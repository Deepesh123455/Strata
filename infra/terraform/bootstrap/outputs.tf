output "state_bucket_name" {
  description = "Put this into ../backend.tf as `bucket`."
  value       = aws_s3_bucket.state.id
}

output "lock_table_name" {
  description = "Put this into ../backend.tf as `dynamodb_table`."
  value       = aws_dynamodb_table.lock.name
}
