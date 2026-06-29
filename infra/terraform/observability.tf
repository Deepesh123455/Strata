# ─────────────────────────────────────────────────────────────────────────────
# Observability — container logs to CloudWatch + alarms to SNS.
#
# The container ships stdout straight to CloudWatch via the awslogs Docker driver
# (set in user-data), so no CloudWatch agent is needed. CPU + status-check alarms
# work off built-in EC2 metrics. (Memory and /data-disk alarms require the
# CloudWatch agent — that's the documented next step in README §Observability.)
# ─────────────────────────────────────────────────────────────────────────────

#tfsec:ignore:aws-cloudwatch-log-group-customer-key default CloudWatch encryption is sufficient for these non-sensitive ops logs.
resource "aws_cloudwatch_log_group" "app" {
  name              = "/${var.project}/${var.environment}"
  retention_in_days = var.log_retention_days
}

#tfsec:ignore:aws-cloudwatch-log-group-customer-key flow logs are non-sensitive metadata.
resource "aws_cloudwatch_log_group" "flow" {
  name              = "/${var.project}/${var.environment}/vpc-flow"
  retention_in_days = var.log_retention_days
}

resource "aws_sns_topic" "alarms" {
  name = "${local.name}-alarms"
  # AWS-managed SNS key is sufficient for a single alarms topic; a CMK adds
  # cost/ops for no real benefit here.
  #tfsec:ignore:aws-sns-topic-encryption-use-cmk
  kms_master_key_id = "alias/aws/sns"
}

resource "aws_sns_topic_subscription" "email" {
  count     = var.alarm_email == "" ? 0 : 1
  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = var.alarm_email
}

resource "aws_cloudwatch_metric_alarm" "cpu_high" {
  alarm_name          = "${local.name}-cpu-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "CPUUtilization"
  namespace           = "AWS/EC2"
  period              = 300
  statistic           = "Average"
  threshold           = 85
  alarm_description   = "EC2 CPU > 85% for 15m — undersized box or runaway load."
  alarm_actions       = [aws_sns_topic.alarms.arn]
  ok_actions          = [aws_sns_topic.alarms.arn]
  dimensions          = { InstanceId = aws_instance.cache.id }
}

resource "aws_cloudwatch_metric_alarm" "status_check" {
  alarm_name          = "${local.name}-status-check-failed"
  comparison_operator = "GreaterThanOrEqualToThreshold"
  evaluation_periods  = 2
  metric_name         = "StatusCheckFailed"
  namespace           = "AWS/EC2"
  period              = 60
  statistic           = "Maximum"
  threshold           = 1
  alarm_description   = "Instance or system status check failing — host unhealthy."
  alarm_actions       = [aws_sns_topic.alarms.arn]
  dimensions          = { InstanceId = aws_instance.cache.id }
}
