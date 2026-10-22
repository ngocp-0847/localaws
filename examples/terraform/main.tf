data "aws_caller_identity" "me" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

# Bump to exercise in-place updates across services (make test-terraform applies 1, then 2).
variable "rev" {
  type    = number
  default = 1
}

locals {
  prefix = "tf-e2e"
  tags   = { app = "fpaas", rev = tostring(var.rev) }
}

# ── S3 ───────────────────────────────────────────────────────────────────
resource "aws_s3_bucket" "batch" {
  bucket        = "${local.prefix}-batch"
  force_destroy = true
  tags          = local.tags
}

resource "aws_s3_bucket_versioning" "batch" {
  bucket = aws_s3_bucket.batch.id
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_notification" "batch" {
  bucket      = aws_s3_bucket.batch.id
  eventbridge = true
}

resource "aws_s3_bucket_public_access_block" "batch" {
  bucket                  = aws_s3_bucket.batch.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "batch" {
  bucket = aws_s3_bucket.batch.id
  rule { object_ownership = "BucketOwnerEnforced" }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "batch" {
  bucket = aws_s3_bucket.batch.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "batch" {
  bucket = aws_s3_bucket.batch.id
  rule {
    id     = "expire-tmp"
    status = "Enabled"
    filter { prefix = "tmp/" }
    expiration { days = 7 * var.rev }
  }
}

data "aws_iam_policy_document" "bucket" {
  statement {
    sid       = "DenyInsecure"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.batch.arn, "${aws_s3_bucket.batch.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "batch" {
  bucket = aws_s3_bucket.batch.id
  policy = data.aws_iam_policy_document.bucket.json
}

resource "aws_s3_object" "seed" {
  bucket       = aws_s3_bucket.batch.id
  key          = "seed/hello.txt"
  content      = "hello from terraform, rev ${var.rev}"
  content_type = "text/plain"
}

# ── SQS ──────────────────────────────────────────────────────────────────
resource "aws_sqs_queue" "mail_dlq" {
  name = "${local.prefix}-mail-dlq"
}

resource "aws_sqs_queue" "mail" {
  name                       = "${local.prefix}-mail"
  visibility_timeout_seconds = 60 * var.rev
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.mail_dlq.arn
    maxReceiveCount     = 2 + var.rev
  })
  tags = local.tags
}

resource "aws_sqs_queue" "jobs_fifo" {
  name                        = "${local.prefix}-jobs.fifo"
  fifo_queue                  = true
  content_based_deduplication = true
}

resource "aws_sqs_queue_redrive_allow_policy" "mail_dlq" {
  queue_url = aws_sqs_queue.mail_dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.mail.arn]
  })
}

resource "aws_sqs_queue_policy" "mail" {
  queue_url = aws_sqs_queue.mail.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "sns.amazonaws.com" }
      Action    = "sqs:SendMessage"
      Resource  = aws_sqs_queue.mail.arn
    }]
  })
}

# ── SNS ──────────────────────────────────────────────────────────────────
resource "aws_sns_topic" "batch_complete" {
  name         = "${local.prefix}-batch-complete"
  display_name = "batch complete (rev ${var.rev})"
  tags         = local.tags
}

resource "aws_sns_topic_subscription" "batch_complete_to_mail" {
  topic_arn = aws_sns_topic.batch_complete.arn
  protocol  = "sqs"
  endpoint  = aws_sqs_queue.mail.arn
}

resource "aws_sns_topic_policy" "batch_complete" {
  arn = aws_sns_topic.batch_complete.arn
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sns:Publish"
      Resource  = aws_sns_topic.batch_complete.arn
    }]
  })
}

# ── SSM ──────────────────────────────────────────────────────────────────
resource "aws_ssm_parameter" "db_host" {
  name  = "/${local.prefix}/db/host"
  type  = "String"
  value = "10.0.0.${var.rev}"
}

resource "aws_ssm_parameter" "db_password" {
  name        = "/${local.prefix}/db/password"
  type        = "SecureString"
  value       = "local-secret"
  description = "database password"
  tags        = local.tags
}

# ── CloudWatch Logs ──────────────────────────────────────────────────────
resource "aws_cloudwatch_log_group" "worker" {
  name              = "/ecs/${local.prefix}-worker"
  retention_in_days = var.rev == 1 ? 14 : 30
  tags              = local.tags
}

# ── IAM (stored, not enforced) ───────────────────────────────────────────
data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "task" {
  name               = "${local.prefix}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  description        = "batch task, rev ${var.rev}"
  tags               = local.tags
}

resource "aws_iam_role_policy" "task" {
  name = "inline"
  role = aws_iam_role.task.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = var.rev == 1 ? ["sqs:*", "s3:*"] : ["sqs:*", "s3:*", "ssm:GetParameters"], Resource = "*" }]
  })
}

resource "aws_iam_policy" "logs" {
  name = "${local.prefix}-logs"
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["logs:*"], Resource = "*", Sid = "Rev${var.rev}" }]
  })
}

resource "aws_iam_role_policy_attachment" "task_logs" {
  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.logs.arn
}

resource "aws_iam_role" "sfn" {
  name = "${local.prefix}-sfn"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "states.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

# ── VPC (what ECS validates awsvpc networking against) ───────────────────
resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "${local.prefix}-vpc" }
}

resource "aws_subnet" "private_a" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "${var.region}a"
  tags              = { Name = "${local.prefix}-private-a" }
}

resource "aws_security_group" "worker" {
  name        = "${local.prefix}-worker"
  description = "batch worker"
  vpc_id      = aws_vpc.main.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# ── ECS ──────────────────────────────────────────────────────────────────
resource "aws_ecs_cluster" "worker" {
  name = "${local.prefix}-worker"
  setting {
    name  = "containerInsights"
    value = "disabled"
  }
}

resource "aws_ecs_task_definition" "batch" {
  family                   = "${local.prefix}-batch"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  task_role_arn            = aws_iam_role.task.arn
  execution_role_arn       = aws_iam_role.task.arn
  container_definitions = jsonencode([{
    name        = "batch"
    image       = "alpine:3"
    essential   = true
    command     = ["echo", "hello", "rev ${var.rev}"]
    environment = [{ name = "QUEUE_URL", value = aws_sqs_queue.mail.id }]
    secrets     = [{ name = "DB_PASSWORD", valueFrom = aws_ssm_parameter.db_password.arn }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.worker.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "batch"
      }
    }
  }])
}

resource "aws_ecs_service" "api" {
  name            = "${local.prefix}-api"
  cluster         = aws_ecs_cluster.worker.id
  task_definition = aws_ecs_task_definition.batch.arn
  desired_count   = var.rev - 1
  launch_type     = "FARGATE"
  network_configuration {
    subnets          = [aws_subnet.private_a.id]
    security_groups  = [aws_security_group.worker.id]
    assign_public_ip = false
  }
}

# ── Step Functions ───────────────────────────────────────────────────────
resource "aws_sfn_state_machine" "on_upload" {
  name     = "${local.prefix}-on-upload"
  role_arn = aws_iam_role.sfn.arn
  tags     = local.tags
  definition = jsonencode({
    Comment = "rev ${var.rev}"
    StartAt = "Run"
    States = {
      Run = {
        Type     = "Task"
        Resource = "arn:aws:states:::ecs:runTask.sync"
        Parameters = {
          Cluster        = aws_ecs_cluster.worker.arn
          TaskDefinition = aws_ecs_task_definition.batch.arn
          LaunchType     = "FARGATE"
          NetworkConfiguration = {
            AwsvpcConfiguration = { Subnets = [aws_subnet.private_a.id], SecurityGroups = [aws_security_group.worker.id] }
          }
        }
        End = true
      }
    }
  })
}

# ── EventBridge ──────────────────────────────────────────────────────────
resource "aws_cloudwatch_event_rule" "on_upload" {
  name        = "${local.prefix}-on-upload"
  description = "S3 upload starts the pipeline (rev ${var.rev})"
  tags        = local.tags
  event_pattern = jsonencode({
    source      = ["aws.s3"]
    detail-type = ["Object Created"]
    detail      = { bucket = { name = [aws_s3_bucket.batch.id] } }
  })
}

resource "aws_cloudwatch_event_target" "on_upload_sfn" {
  rule      = aws_cloudwatch_event_rule.on_upload.name
  target_id = "sfn"
  arn       = aws_sfn_state_machine.on_upload.arn
  role_arn  = aws_iam_role.sfn.arn
}

resource "aws_cloudwatch_event_target" "on_upload_queue" {
  rule      = aws_cloudwatch_event_rule.on_upload.name
  target_id = "queue"
  arn       = aws_sqs_queue.mail.arn
  input_transformer {
    input_paths    = { key = "$.detail.object.key" }
    input_template = "{\"key\": <key>, \"rev\": ${var.rev}}"
  }
}

resource "aws_cloudwatch_event_rule" "tick" {
  name                = "${local.prefix}-tick"
  schedule_expression = "rate(${5 * var.rev} minutes)"
  state               = "DISABLED"
}

# ── SES ──────────────────────────────────────────────────────────────────
resource "aws_ses_email_identity" "noreply" {
  email = "noreply@example.com"
}

resource "aws_ses_domain_identity" "example" {
  domain = "example.com"
}

output "account_id" { value = data.aws_caller_identity.me.account_id }
output "queue_url" { value = aws_sqs_queue.mail.id }
output "state_machine" { value = aws_sfn_state_machine.on_upload.arn }
