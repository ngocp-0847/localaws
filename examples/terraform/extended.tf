# ── KMS / Secrets Manager ────────────────────────────────────────────────
resource "aws_kms_key" "app" {
  description             = "app data, rev ${var.rev}"
  deletion_window_in_days = 7
  enable_key_rotation     = true
  tags                    = { app = "fpaas" }
}

resource "aws_kms_alias" "app" {
  name          = "alias/${local.prefix}-app"
  target_key_id = aws_kms_key.app.key_id
}

resource "aws_ssm_parameter" "api_key" {
  name   = "/${local.prefix}/api/key"
  type   = "SecureString"
  key_id = aws_kms_alias.app.name
  value  = "k-${var.rev}"
}

resource "aws_secretsmanager_secret" "db" {
  name                    = "${local.prefix}/db"
  description             = "db credentials"
  kms_key_id              = aws_kms_key.app.arn
  recovery_window_in_days = 0
  tags                    = { app = "fpaas" }
}

resource "aws_secretsmanager_secret_version" "db" {
  secret_id     = aws_secretsmanager_secret.db.id
  secret_string = jsonencode({ username = "app", password = "pw-${var.rev}" })
}

# ── ECR ──────────────────────────────────────────────────────────────────
resource "aws_ecr_repository" "batch" {
  name                 = "${local.prefix}/batch"
  image_tag_mutability = var.rev == 1 ? "MUTABLE" : "IMMUTABLE"
  force_delete         = true
  image_scanning_configuration { scan_on_push = true }
}

resource "aws_ecr_lifecycle_policy" "batch" {
  repository = aws_ecr_repository.batch.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "keep 10"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 10 }
      action       = { type = "expire" }
    }]
  })
}

# ── EventBridge Scheduler ────────────────────────────────────────────────
resource "aws_scheduler_schedule_group" "batch" {
  name = "${local.prefix}-batch"
  tags = { app = "fpaas" }
}

resource "aws_scheduler_schedule" "nightly" {
  name                         = "${local.prefix}-nightly"
  group_name                   = aws_scheduler_schedule_group.batch.name
  schedule_expression          = "cron(0 ${1 + var.rev} * * ? *)"
  schedule_expression_timezone = "Asia/Tokyo"
  flexible_time_window { mode = "OFF" }
  target {
    arn      = aws_sqs_queue.mail.arn
    role_arn = aws_iam_role.sfn.arn
    input    = jsonencode({ job = "nightly" })
  }
}

resource "aws_cloudwatch_event_target" "nightly_ecs" {
  rule     = aws_cloudwatch_event_rule.tick.name
  arn      = aws_ecs_cluster.worker.arn
  role_arn = aws_iam_role.sfn.arn
  ecs_target {
    task_definition_arn = aws_ecs_task_definition.batch.arn
    task_count          = 1
    launch_type         = "FARGATE"
    network_configuration {
      subnets         = [aws_subnet.private_a.id]
      security_groups = [aws_security_group.worker.id]
    }
  }
}

# ── IAM extras ───────────────────────────────────────────────────────────
resource "aws_iam_user" "ci" {
  name = "${local.prefix}-ci"
  tags = local.tags
}

resource "aws_iam_access_key" "ci" {
  user = aws_iam_user.ci.name
}

resource "aws_iam_user_policy" "ci" {
  name = "deploy"
  user = aws_iam_user.ci.name
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "ecs:*", Resource = "*" }]
  })
}

resource "aws_iam_role_policy_attachment" "task_exec" {
  role       = aws_iam_role.task.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_instance_profile" "bastion" {
  name = "${local.prefix}-bastion"
  role = aws_iam_role.sfn.name
}

resource "aws_iam_openid_connect_provider" "github" {
  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]
}

# ── network extras ───────────────────────────────────────────────────────
data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_subnet" "public_a" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.0.101.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
  tags                    = { Name = "${local.prefix}-public-a" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.prefix}-igw" }
}

resource "aws_eip" "nat" {
  domain = "vpc"
}

resource "aws_nat_gateway" "main" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public_a.id
  depends_on    = [aws_internet_gateway.main]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
}

resource "aws_route" "private_nat" {
  route_table_id         = aws_route_table.private.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.main.id
}

resource "aws_route_table_association" "public_a" {
  subnet_id      = aws_subnet.public_a.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "private_a" {
  subnet_id      = aws_subnet.private_a.id
  route_table_id = aws_route_table.private.id
}

resource "aws_security_group" "db" {
  name        = "${local.prefix}-db"
  description = "postgres"
  vpc_id      = aws_vpc.main.id
}

resource "aws_vpc_security_group_ingress_rule" "db_from_worker" {
  security_group_id            = aws_security_group.db.id
  referenced_security_group_id = aws_security_group.worker.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

resource "aws_vpc_security_group_egress_rule" "db_all" {
  security_group_id = aws_security_group.db.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}

resource "aws_security_group_rule" "worker_https" {
  type              = "ingress"
  security_group_id = aws_security_group.worker.id
  protocol          = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_blocks       = ["10.0.0.0/16"]
  description       = "https from the vpc, rev ${var.rev}"
}

# ── SES extras ───────────────────────────────────────────────────────────
resource "aws_ses_domain_dkim" "example" {
  domain = aws_ses_domain_identity.example.domain
}

resource "aws_ses_identity_notification_topic" "bounce" {
  topic_arn                = aws_sns_topic.batch_complete.arn
  notification_type        = "Bounce"
  identity                 = aws_ses_domain_identity.example.domain
  include_original_headers = true
}

resource "aws_ses_template" "welcome" {
  name    = "${local.prefix}-welcome"
  subject = "Welcome, {{name}} (rev ${var.rev})"
  html    = "<h1>Hello {{name}}</h1>"
  text    = "Hello {{name}}"
}

resource "aws_sesv2_email_identity" "support" {
  email_identity = "support@example.com"
}

# ── S3 extras ────────────────────────────────────────────────────────────
resource "aws_s3_bucket" "assets" {
  bucket = "${local.prefix}-assets"
}

resource "aws_s3_bucket_cors_configuration" "assets" {
  bucket = aws_s3_bucket.assets.id
  cors_rule {
    allowed_methods = ["GET", "PUT"]
    allowed_origins = ["https://app.example.com"]
    allowed_headers = ["*"]
    max_age_seconds = 3000 * var.rev
  }
}

resource "aws_s3_bucket_website_configuration" "assets" {
  bucket = aws_s3_bucket.assets.id
  index_document { suffix = "index.html" }
}
