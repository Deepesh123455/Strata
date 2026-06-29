# ─────────────────────────────────────────────────────────────────────────────
# Backup / DR — nightly snapshots of the WAL volume via Data Lifecycle Manager.
# Restore = create a volume from the latest snapshot, attach, boot; the cache
# replays the WAL. Test this restore quarterly (an untested backup isn't one).
# ─────────────────────────────────────────────────────────────────────────────

data "aws_iam_policy_document" "dlm_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["dlm.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "dlm" {
  name               = "${local.name}-dlm"
  assume_role_policy = data.aws_iam_policy_document.dlm_assume.json
}

resource "aws_iam_role_policy_attachment" "dlm" {
  role       = aws_iam_role.dlm.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSDataLifecycleManagerServiceRole"
}

resource "aws_dlm_lifecycle_policy" "data" {
  description        = "${local.name} daily WAL volume snapshots"
  execution_role_arn = aws_iam_role.dlm.arn
  state              = "ENABLED"

  policy_details {
    resource_types = ["VOLUME"]

    # Snapshot only the WAL volume (matched by its Name tag).
    target_tags = {
      Name = "${local.name}-data"
    }

    schedule {
      name = "daily-7d"

      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = ["03:00"]
      }

      retain_rule {
        count = 7
      }

      copy_tags = true
    }
  }
}
