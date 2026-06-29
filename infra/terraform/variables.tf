# ── Identity / naming ────────────────────────────────────────────────────────
variable "aws_region" {
  description = "AWS region to deploy into. us-east-1 is the cheapest and fully Free-Tier eligible."
  type        = string
  default     = "us-east-1"
}

variable "project" {
  description = "Project name; used as a prefix for every resource."
  type        = string
  default     = "powerhouse-cache"
}

variable "environment" {
  description = "Environment name (dev/staging/prod). Drives sizing via *.tfvars."
  type        = string
  default     = "prod"
}

# ── Networking ───────────────────────────────────────────────────────────────
variable "network_mode" {
  description = <<-EOT
    How the cache instance reaches ECR/CloudWatch/SSM.
      • "public-free"      — public subnet + public IP, NO NAT/endpoints → $0
                             (the SG still blocks :6379 to everyone but app_sg).
    Future modes ("private-endpoints", "private-nat") are documented in the
    README and can be added without touching the rest of the module.
  EOT
  type        = string
  default     = "public-free"

  validation {
    condition     = var.network_mode == "public-free"
    error_message = "Only \"public-free\" is implemented today. See README §Networking."
  }
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.0.0.0/16"
}

variable "public_subnet_cidrs" {
  description = "CIDRs for the public subnets (one per AZ)."
  type        = list(string)
  default     = ["10.0.1.0/24", "10.0.2.0/24"]
}

# ── Compute / storage ────────────────────────────────────────────────────────
variable "instance_type" {
  description = "EC2 instance type. t3.micro is Free-Tier eligible (750 h/mo for 12 months)."
  type        = string
  default     = "t3.micro"
}

variable "data_volume_gb" {
  description = "Size of the dedicated encrypted EBS volume that holds the WAL (/data). Free Tier covers 30 GB of EBS."
  type        = number
  default     = 8
}

variable "maxmemory_mb" {
  description = "Engine memory cap (POWERHOUSE_MAXMEMORY_MB). Keep it well under the box RAM."
  type        = number
  default     = 700
}

variable "image_tag" {
  description = "ECR image tag to run. CD passes the git short-SHA; defaults to latest for a first manual apply."
  type        = string
  default     = "latest"
}

# ── GitHub OIDC ──────────────────────────────────────────────────────────────
variable "github_owner" {
  description = "GitHub org/user that owns the repo (the OIDC `sub` is pinned to this)."
  type        = string
  default     = "Deepesh123455"
}

variable "github_repo" {
  description = "GitHub repository name."
  type        = string
  default     = "Strata"
}

variable "github_deploy_environment" {
  description = "Protected GitHub Environment the deploy role is bound to."
  type        = string
  default     = "production"
}

variable "lock_table_name" {
  description = "DynamoDB state-lock table name (must match backend.tf and bootstrap). The deploy role is scoped to it."
  type        = string
  default     = "powerhouse-cache-tflock"
}

# ── Observability ────────────────────────────────────────────────────────────
variable "log_retention_days" {
  description = "CloudWatch Logs retention for the container logs."
  type        = number
  default     = 30
}

variable "alarm_email" {
  description = "Optional email subscribed to the SNS alarm topic. Leave empty to skip the subscription."
  type        = string
  default     = ""
}
