# examples/terraform

A stack written the way you would write it for AWS, applied to localaws: S3 (versioning,
EventBridge notifications, lifecycle, policy, public access block, CORS, website, an object), SQS with
a dead-letter queue and policies, SNS → SQS, SSM parameters, Secrets Manager, KMS, IAM roles / policies /
users / instance profile / OIDC provider, a VPC with subnets, routes, NAT and security groups, an ECS
cluster, task definition and service, a Step Functions state machine, EventBridge rules (pattern and
schedule) with state machine, queue and ECS targets, an EventBridge Scheduler schedule, SES identities
and a template, an ECR repository, a log group.

`provider.tf` points the provider at localaws explicitly. For a module you do not want to edit, use
`localaws terraform-override` instead (see the main README).

```bash
./localaws -region ap-northeast-1 -runner noop &
cd examples/terraform
terraform init
terraform apply                       # var.localaws_endpoint defaults to http://localhost:4566
terraform plan                        # → No changes.
terraform apply -var rev=2            # updates across services in place
terraform destroy -var rev=2
```

`make test-terraform` runs exactly this against a fresh localaws.
