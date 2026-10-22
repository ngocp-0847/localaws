# The AWS provider pointed at localaws. Copy this file next to your own Terraform
# (or use the module in ../../terraform/provider) — every service the provider talks to
# goes to one endpoint, credentials are dummies, and the checks that need real AWS are off.
terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = ">= 5.0" }
  }
}

variable "localaws_endpoint" {
  type    = string
  default = "http://localhost:4566"
}

variable "region" {
  type    = string
  default = "ap-northeast-1"
}

provider "aws" {
  region                      = var.region
  access_key                  = "test"
  secret_key                  = "test"
  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_requesting_account_id  = false # GetCallerIdentity is emulated: data.aws_caller_identity works
  skip_region_validation      = true
  s3_use_path_style           = true

  # S3 Control (bucket tags in provider v6) puts the account id in front of the host:
  # 000000000000.localhost — a name that does not resolve everywhere. Requests to it go
  # to localaws as a plain HTTP proxy, which answers them itself. Calls to localhost /
  # 127.0.0.1 never use the proxy.
  http_proxy = var.localaws_endpoint
  # …and anything without an endpoint here would go to real AWS over HTTPS: through
  # this proxy it is refused instead (localaws answers CONNECT with 403).
  https_proxy = var.localaws_endpoint

  endpoints {
    s3             = var.localaws_endpoint
    s3control      = var.localaws_endpoint
    sqs            = var.localaws_endpoint
    sns            = var.localaws_endpoint
    ssm            = var.localaws_endpoint
    sts            = var.localaws_endpoint
    iam            = var.localaws_endpoint
    kms            = var.localaws_endpoint
    ec2            = var.localaws_endpoint
    ecs            = var.localaws_endpoint
    ecr            = var.localaws_endpoint
    sfn            = var.localaws_endpoint
    events         = var.localaws_endpoint
    scheduler      = var.localaws_endpoint
    logs           = var.localaws_endpoint
    ses            = var.localaws_endpoint
    sesv2          = var.localaws_endpoint
    secretsmanager = var.localaws_endpoint
    lambda         = var.localaws_endpoint
  }
}
