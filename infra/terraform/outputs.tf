output "ecr_repository_url" {
  description = "Push images here. Set repo variable ECR_REPOSITORY to the repo name."
  value       = aws_ecr_repository.main.repository_url
}

output "cache_private_ip" {
  description = "Connect your app tier here (port 6379). Reachable only from the app SG."
  value       = aws_instance.cache.private_ip
}

output "cache_elastic_ip" {
  description = "Stable public EIP of the instance (NOT a cache endpoint — 6379 is firewalled). For reference/SSM only."
  value       = aws_eip.cache.public_ip
}

output "app_security_group_id" {
  description = "Attach this SG to anything that needs to reach the cache on 6379."
  value       = aws_security_group.app.id
}

output "instance_id" {
  description = "EC2 instance id — `aws ssm start-session --target <this>` for ops."
  value       = aws_instance.cache.id
}

output "gha_ci_role_arn" {
  description = "Set repo variable AWS_ROLE_ARN to this (enables CD build+push)."
  value       = aws_iam_role.gha_ci.arn
}

output "gha_deploy_role_arn" {
  description = "Set repo variable AWS_DEPLOY_ROLE_ARN to this (enables CD deploy)."
  value       = aws_iam_role.gha_deploy.arn
}
