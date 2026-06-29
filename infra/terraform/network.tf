# ─────────────────────────────────────────────────────────────────────────────
# Network — "public-free" mode (see var.network_mode).
#
# The instance lives in a PUBLIC subnet with a public IP so it can pull from ECR
# and ship logs to CloudWatch over the Internet Gateway — NO NAT Gateway, NO VPC
# endpoints → $0. Security is enforced at the SECURITY GROUP, not the subnet:
# port 6379 is reachable ONLY from the app security group (see security.tf), and
# there is no inbound SSH at all (ops go through SSM Session Manager).
# ─────────────────────────────────────────────────────────────────────────────

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = local.name }
}

# Flow logs to CloudWatch → an audit trail of every accepted/rejected packet.
resource "aws_flow_log" "vpc" {
  vpc_id          = aws_vpc.main.id
  traffic_type    = "ALL"
  log_destination = aws_cloudwatch_log_group.flow.arn
  iam_role_arn    = aws_iam_role.flow_logs.arn
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = local.name }
}

resource "aws_subnet" "public" {
  count             = length(var.public_subnet_cidrs)
  vpc_id            = aws_vpc.main.id
  cidr_block        = var.public_subnet_cidrs[count.index]
  availability_zone = data.aws_availability_zones.available.names[count.index]
  # Public IP is intentional in public-free mode; the cache SG firewalls 6379 to
  # the app SG only, so the box is reachable for egress but the cache is not.
  #tfsec:ignore:aws-ec2-no-public-ip-subnet
  map_public_ip_on_launch = true

  tags = { Name = "${local.name}-public-${count.index}" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.name}-public" }
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.main.id
}

resource "aws_route_table_association" "public" {
  count          = length(aws_subnet.public)
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}
