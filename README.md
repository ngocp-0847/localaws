# localaws

A small AWS emulator that **runs things for real**. One static binary (~12 MB, pure Go,
SQLite inside), one port, starts in well under a second.

- The AWS SDKs and CLI talk to it unchanged — set `AWS_ENDPOINT_URL=http://localhost:4566`.
- State machines are **interpreted** state by state, with the execution history AWS would write.
- ECS tasks **run**: as containers (docker), host processes (exec), or no-ops for pure orchestration tests.
- S3 → EventBridge → Step Functions → ECS → CloudWatch Logs behaves as one pipeline, end to end.
- **Terraform provisions it**: the AWS provider applies, plans clean, updates and destroys against it —
  your own module unchanged, through a generated `*_override.tf` ([Terraform](#terraform)).
- Everything persists in `data/localaws.sqlite`.
- A **web console** at `http://localhost:4566/_localaws/`, laid out like the AWS console, to look *and act*:
  upload to S3, start / stop executions and watch them on the graph, run and stop tasks with their logs
  live, edit parameters, send events — built into the binary.

## Quick start

```bash
go build -o localaws .                          # or: make build / make release
./localaws                                      # empty account 000000000000, us-east-1, runner docker

# a whole environment from one file (buckets, rules, state machines, task definitions, parameters…)
./localaws -config examples/pipeline/localaws.json
AWS_ENDPOINT_URL=http://localhost:4566 python examples/smoke.py

# or in Docker (tasks run on the host's engine through the mounted socket)
docker compose up -d
```

Point any tool at it:

```bash
export AWS_ENDPOINT_URL=http://localhost:4566 AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1
aws s3 cp report.csv s3://inbox/incoming/report.csv
aws stepfunctions list-executions --state-machine-arn arn:aws:states:us-east-1:000000000000:stateMachine:on-upload
```

## What is emulated

| Service | Operations | Behaviour that matches AWS |
|---|---|---|
| **S3** | buckets (create/delete/head/list, versioning, location, notification), bucket configuration (policy, tagging, CORS, lifecycle, website, encryption, public access block, ownership controls, ACL, logging, replication, object lock, accelerate, request payment — stored and read back), objects (put/get/head/delete/copy, `Range`, `If-Match`/`If-None-Match` on reads **and writes**, user metadata, tagging), ListObjects v1/v2 (prefix, delimiter, continuation), ListObjectVersions, DeleteObjects, multipart (create/upload part/complete/abort/list), S3 Control tags | version ids and delete markers, `null` versions on unversioned buckets, multipart ETag `md5-of-md5s-N`, 5 MiB minimum part size, aws-chunked bodies (SigV4 streaming / CRC trailers), path- and virtual-host-style addressing, the "not configured" codes (`NoSuchBucketPolicy`, `NoSuchTagSet`, …), default SSE-S3, `BucketNotEmpty`, `NoSuchKey`, … |
| **EventBridge** | Put/Describe/List/Delete/Enable/Disable rule, Put/Remove/List targets, PutEvents, TestEventPattern, ListRuleNamesByTarget | full pattern syntax (prefix, suffix, wildcard, equals-ignore-case, anything-but, numeric, exists, cidr, `$or`, arrays), Input / InputPath / InputTransformer, `rate()` and `cron()` schedules; S3 `Object Created` / `Object Deleted` events; targets: state machines, queues, topics, ECS tasks (`EcsParameters`), log groups; every target field kept as put; tags |
| **Step Functions** | Create/Update/Delete/Describe/List state machine, ValidateStateMachineDefinition, Start/StartSync/Stop/Describe/List execution, GetExecutionHistory, DescribeStateMachineForExecution | Task, Pass, Choice, Wait, Parallel, Map, Succeed, Fail; InputPath / Parameters / ResultSelector / ResultPath / OutputPath; context object `$$`; all `States.*` intrinsics; Retry (backoff, MaxDelaySeconds, jitter) and Catch; TimeoutSeconds; definition validation; history events with the real types and `previousEventId` chain. Integrations: `ecs:runTask(.sync)`, `states:startExecution(.sync/.sync:2)`, `events:putEvents`, `sns:publish` |
| **ECS** | clusters (settings, configuration, capacity providers), Register/Describe/List/Deregister/Delete task definition, RunTask, DescribeTasks, ListTasks, StopTask, Create/Update/Delete/Describe/List service, tags | revisions, task lifecycle (PROVISIONING → PENDING → RUNNING → STOPPED), stop codes, exit codes per container, `essential`, awsvpc validation (network configuration, subnets that exist), `secrets` from SSM and Secrets Manager (JSON keys too), `environmentFiles` from S3, awslogs streams `<prefix>/<container>/<task id>`, services kept at `desiredCount` (DRAINING → INACTIVE) |
| **CloudWatch Logs** | groups (KMS key, class), streams, PutLogEvents, GetLogEvents, FilterLogEvents, retention, tags | forward/backward tokens (same token = no more events), filter patterns incl. `{ $.field = value }` |
| **SSM** | Put/Get/GetParameters/ByPath/Describe/Delete(s)/History, tags | versions, Overwrite (keeps what it does not restate), KeyId / tier / AllowedPattern, SecureString returned encrypted unless `WithDecryption` |
| **SQS** | Create/Delete/List/GetQueueUrl/Purge, Get/SetQueueAttributes, Send/SendBatch, Receive (long polling), Delete/DeleteBatch, ChangeMessageVisibility(Batch), tags, ListDeadLetterSourceQueues — AWS JSON 1.0 | visibility timeout, DelaySeconds, `ApproximateReceiveCount`, **RedrivePolicy → DLQ after maxReceiveCount**, real body / message-attribute MD5s (the SDKs verify them), FIFO groups + dedup; any URL whose last segment names the queue is accepted (`SQS_BASE_URL` + name); EventBridge rules and `sqs:sendMessage` tasks can target queues |
| **SES** | v1 SendEmail / SendRawEmail / SendTemplatedEmail, identities (email, domain, DKIM, notification topics, MAIL FROM), templates, configuration sets, quota (Query); v2 SendEmail, email identities, tags | mail is **captured, never delivered** — read it back at `GET /_localaws/api/ses/messages?to=&since=`; every identity is verified at once |
| **SNS** | topics (attributes, policies, FIFO), Subscribe / Unsubscribe / subscription attributes, Publish / PublishBatch, tags | publishes are **delivered to SQS subscriptions** (SNS envelope, or `RawMessageDelivery`), `FilterPolicy` on attributes or body; every publish is recorded at `GET /_localaws/api/sns/messages`; EventBridge rules and `sns:publish` tasks can target topics |
| **IAM** | roles, managed policies (versions), inline policies, attachments, users and access keys, instance profiles, OIDC providers, service-linked roles, tags | stored and read back exactly (documents URL-encoded, as IAM returns them), `DeleteConflict` while attached; AWS managed policies (`arn:aws:iam::aws:policy/…`) exist; **never enforced** |
| **EC2** | VPCs (DNS attributes, main route table, default security group and network ACL), subnets, security groups and rules (`sgr-…`), route tables / routes / associations, internet and NAT gateways, Elastic IPs, availability zones, tags, filters | records only — nothing is routed; `DependencyViolation` on deletes in the wrong order; awsvpc RunTask validates against these subnets |
| **KMS** | keys, aliases, key policies, rotation, enable / disable, scheduled deletion, tags, Encrypt / Decrypt / ReEncrypt / GenerateDataKey | the ciphertext is a reversible envelope naming its key — nothing is secret; `alias/aws/*` keys appear on first use |
| **Secrets Manager** | secrets, versions and staging labels (`AWSCURRENT` / `AWSPREVIOUS`), Get/Put/Update/BatchGet, resource policies, tags, scheduled / forced deletion, restore, GetRandomPassword | ECS `secrets` resolve here, whole or one JSON key |
| **ECR** | repositories (tag mutability, scan on push, encryption), lifecycle and repository policies, tags, GetAuthorizationToken | no registry: images are mapped with `runner.docker.images` |
| **EventBridge Scheduler** | schedules, schedule groups, tags (REST) | schedules **fire**: `rate()`, `cron()` in `ScheduleExpressionTimezone`, `at()` one-time, `StartDate` / `EndDate`, `ActionAfterCompletion=DELETE`; targets as for rules, with the schedule's `Input` |
| **STS** | GetCallerIdentity, AssumeRole, AssumeRoleWithWebIdentity, GetSessionToken | the identity is `-identity` |

Requests are routed by `X-Amz-Target`, by the SigV4 signing scope, or else treated as S3.
Signatures are **not** verified and IAM is **not** enforced. Writes that read and modify one resource
(a bucket's configuration, a role's policies, …) are serialised, so tools that send them in parallel —
Terraform does — lose none.

## How tasks run

| `runner.mode` | What `RunTask` starts |
|---|---|
| `docker` (default) | `docker run --rm` per container. Several containers share one network namespace through a pause container (as awsvpc does): one `localhost`. `runner.docker.images` maps image names (e.g. ECR URIs → local tags). |
| `exec` | the essential container's `entryPoint + command` as a process (cwd `runner.exec.dir`). |
| `noop` | nothing runs: the container logs its command and exits with `LOCALAWS_NOOP_EXIT` (default 0) after `LOCALAWS_NOOP_SLEEP` (default 200ms) from its environment. |

Every container gets, unless already defined: `AWS_ENDPOINT_URL` (back to this emulator —
`http://host.docker.internal:<port>` for docker, `http://127.0.0.1:<port>` otherwise), `AWS_REGION`,
`AWS_DEFAULT_REGION`, dummy `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` and `AWS_EXECUTION_ENV`.
So the code inside a task uses the emulated S3 / SSM / … without changes. Turn it off with `"injectAws": false`.

Stopping an execution stops the tasks it started; restarting the emulator marks what was running as
ABORTED / STOPPED, the way AWS reports work lost with its host.

## Configuration

Flags > `LOCALAWS_*` environment variables > config file > defaults.

| Flag | Env | Default | |
|---|---|---|---|
| `-config` | `LOCALAWS_CONFIG` | — | JSON file: settings + resources |
| `-port` | `LOCALAWS_PORT` | 4566 | all services and the UI |
| `-data` | `LOCALAWS_DATA` | `data` | SQLite + object blobs |
| `-account` / `-region` | `LOCALAWS_ACCOUNT` / `LOCALAWS_REGION` | 000000000000 / us-east-1 | used in every ARN |
| `-identity` | `LOCALAWS_IDENTITY` | `localaws` | IAM user name or full ARN for `GetCallerIdentity` |
| `-runner` | `LOCALAWS_RUNNER` | docker | docker \| exec \| noop |
| `-docker-network` | `LOCALAWS_DOCKER_NETWORK` | — | network for task containers |
| `-exec-dir` | `LOCALAWS_EXEC_DIR` | — | working directory for runner=exec |
| `-task-endpoint` | `LOCALAWS_TASK_ENDPOINT` | see above | `AWS_ENDPOINT_URL` given to tasks |
| `-time-scale` | `LOCALAWS_TIME_SCALE` | 1 | multiplies Wait states, retry delays and timeouts (0.1 = 10× faster) |
| `-log-api` | `LOCALAWS_LOG_API=1` | off | log every API call |

The config file declares resources with the shapes the AWS APIs take; it is applied at every start,
idempotently (changed definitions become new revisions / updates). `${ACCOUNT}`, `${REGION}`,
`${env:NAME}` and `${env:NAME:-default}` are substituted; `definitionFile` / `specFile` are relative to it.

```jsonc
{
  "account": "123456789012", "region": "eu-west-1", "identity": "developer",
  "runner": { "mode": "docker", "docker": { "network": "my-net", "images": { "<ecr uri>": "local:tag" } }, "env": { "LOG_LEVEL": "debug" } },
  "resources": {
    "s3":            { "buckets": [ { "name": "inbox", "versioning": true, "eventBridge": true } ] },
    "ec2":           { "subnets": [ { "id": "subnet-1", "vpcId": "vpc-1" } ], "securityGroups": [ { "id": "sg-1" } ] },
    "logs":          { "groups": [ "/ecs/worker" ] },
    "ssm":           { "parameters": [ { "name": "/app/db/password", "type": "SecureString", "value": "${env:DB_PASS:-local}" } ] },
    "ecs":           { "clusters": [ "work" ], "taskDefinitions": [ { "specFile": "worker.taskdef.json" } ], "services": [] },
    "stepFunctions": { "stateMachines": [ { "name": "on-upload", "definitionFile": "on-upload.asl.json" } ] },
    "events":        { "rules": [ { "name": "on-upload", "eventPattern": { "source": ["aws.s3"] }, "targets": [ { "Id": "1", "Arn": "arn:aws:states:${REGION}:${ACCOUNT}:stateMachine:on-upload" } ] } ] },
    "sns":           { "topics": [ "alerts" ] },
    "sqs":           { "queues": [ { "name": "jobs", "deadLetterQueue": "jobs-dlq", "maxReceiveCount": 3, "visibilityTimeout": 60 } ] }
  }
}
```

A task definition or state machine exported from a real account (`aws ecs describe-task-definition`,
`aws stepfunctions describe-state-machine`) can be dropped in as is; map its images with
`runner.docker.images`, its ARNs follow `account` / `region`.

## Terraform

The AWS provider runs against localaws as against an account: `apply` creates, a second `plan`
reports **no changes**, edits update in place, `destroy` removes — `make test-terraform` does exactly
that with the real provider over [`examples/terraform`](examples/terraform) (74 resources of 60 types: S3 with its
configuration, SQS + DLQ, SNS → SQS, SSM, Secrets Manager, KMS, IAM, VPC / subnets / security groups /
routes / NAT, ECS cluster + task definition + service, Step Functions, EventBridge rules and targets,
Scheduler, SES, ECR, CloudWatch Logs).

**Your existing Terraform, unchanged.** Point it at localaws with an override file — Terraform merges
`*_override.tf` over the module, so the code itself is untouched and deleting the file targets AWS again:

```bash
localaws terraform-override -dir infra/envs/e2e -endpoint http://localhost:4566 -o localaws_override.tf
terraform -chdir=infra/envs/e2e init && terraform -chdir=infra/envs/e2e apply

# or let the wrapper write the override before each command and remove it afterwards (like tflocal)
LOCALAWS_ENDPOINT=http://localhost:4566 scripts/terraform-localaws -chdir=infra/envs/e2e apply -auto-approve
```

The generated file overrides every `provider "aws"` block of the module — the default one and each
`alias` — with the localaws endpoints, dummy credentials, and `profile`, `allowed_account_ids`, … set
to null. `-backend`:

| | state goes to |
|---|---|
| `auto` (default) | `s3` when the module has an `s3` backend, else unchanged |
| `s3` | localaws's own S3, same bucket and key (the bucket is created); `use_lockfile` works. `terraform_remote_state` sources that read S3 follow, so stacks that read each other's outputs apply in order |
| `local` | `terraform.localaws.tfstate` next to the module |
| `keep` | wherever the module says |

Things to know:

- localaws has **one account and one region** (`-account`, `-region`): start it with the region your
  code uses (and, if you like, the real account id, so ARNs look familiar). Provider aliases for other
  regions land in the same place.
- Bucket tags go through S3 Control, whose host is `<account>.localhost`. The override sets the
  provider's `http_proxy` to localaws, which answers such requests itself — no DNS needed. Its
  `https_proxy` is localaws too: a call to a service with no endpoint is **refused** (`CONNECT` → 403,
  logged) instead of reaching real AWS.
- Services localaws does not emulate (Lambda, RDS, CloudFront, ELB, Route 53, ACM, WAF, …) fail with
  `UnknownOperationException … not emulated`. Keep them out of the stack you apply here (a separate
  module, `count = var.localaws ? 0 : 1`, …).
- Creation takes as long as the provider's own waiters (SQS queues ≈25 s, an S3 lifecycle ≈55 s — the
  same as on AWS); localaws answers at once. Cache providers in CI with `TF_PLUGIN_CACHE_DIR`.

In CI (GitHub Actions):

```yaml
- run: ./localaws -region ap-northeast-1 -runner noop -data "$RUNNER_TEMP/localaws" &
- uses: hashicorp/setup-terraform@v3
  with: { terraform_wrapper: false }
- run: |
    for i in $(seq 50); do curl -fsS localhost:4566/_localaws/health && break; sleep 0.1; done
    scripts/terraform-localaws -chdir=infra/e2e init -input=false
    scripts/terraform-localaws -chdir=infra/e2e apply -auto-approve -input=false
```

## Web console

`http://localhost:4566/_localaws/` — a small AWS-console look-alike for developers and QA. It speaks the
same AWS protocols to the emulator that an application does, so what you click is what an SDK call does.

| Service | What you can do |
|---|---|
| S3 | buckets (create with versioning / EventBridge, delete, toggle both), folder browser, drag-and-drop upload, preview, download, versions and delete markers, metadata |
| Step Functions | state machines (create / edit with a live graph and ASL validation, delete), executions (start with input, stop, re-run with the same input); an execution's **Graph view** coloured by state, **Table view**, **Events**, input / output, per-state input, output, error and the ECS task it started |
| ECS | clusters, tasks (running / stopped, stop), **Run task** (task definition, subnets, command and environment overrides), a task's containers, exit codes, stop reason and **live logs**; task definitions with revisions (register a new one from JSON, deregister); services (desired count) |
| EventBridge | rules (create from a pattern or `rate()`, target picker, enable / disable, delete), **test a pattern** against an event, **send events** and see which rules match |
| CloudWatch Logs | groups, streams by last event, a stream viewer that follows new lines (level filter, JSON expanded on click), group-wide search with filter patterns |
| Parameter Store | list, create / edit (String, StringList, SecureString), reveal values, delete |
| VPC · API activity | the declared subnets / security groups; every API call received, with status and duration |

Top bar: service search (`Alt+S`), auto-refresh switch, light / dark, account menu (identity, runner,
**Reset emulator**). Stack: React 19 + TypeScript, Zustand, Tailwind CSS 4, shadcn/ui on Radix, Vite;
~145 KB gzipped, embedded in the Go binary (`console/dist` is committed, so `go build` needs no Node).

```bash
make console        # rebuild console/dist after changing console/src
make console-dev    # hot reload on :5173, API calls proxied to LOCALAWS_URL (default http://localhost:4566)
make console-test   # drives the console in your installed Chrome (playwright-core): 12 QA flows
```

## Admin

| | |
|---|---|
| `GET /_localaws/health` | status, version, runner |
| `POST /_localaws/reset` | stop everything, wipe all data, re-apply the config |
| `GET /_localaws/api/info` · `/api/calls` | what the console's header and API activity page read |
| `GET`/`DELETE /_localaws/api/ses/messages` | captured mail (`?to=&since=<ms>&limit=`), newest first / clear |
| `GET`/`DELETE /_localaws/api/sns/messages` | recorded SNS publishes (`?topic=&since=`) |
| `GET /_localaws/api/sqs/messages?queue=` | peek a queue's messages without receiving them |
| `GET /_localaws/plugin` · `/plugin/sections/<id>` | **e2e-studio plugin** (protocol `e2e-studio.plugin/v1`): status, summary metrics, and tables (queues, captured mail, executions, ECS tasks, buckets, API activity) |
| `/_localaws/` | the web console |

## As an e2e-studio plugin

localaws stays a standalone tool, but it also speaks the read-only
[e2e-studio](https://github.com/ngocp-0847/e2e-studio) plugin protocol, so a testing studio
can show its state next to the test runs: declare it in the studio environment —

```yaml
plugins:
  - name: localaws
    url: http://127.0.0.1:4566        # or service: <a service of the environment>
    manifest: /_localaws/plugin
```

— and the studio's **Plugins** page shows its status (degraded when messages are dead-lettered
or executions failed), summary metrics, live tables, and this console embedded. Nothing in
localaws depends on the studio.

## Development

```bash
make test      # unit tests (ASL, patterns, filters, cron) + HTTP integration tests (runner=noop)
make test-terraform   # the real AWS provider over examples/terraform: apply → plan clean → update → plan clean → destroy
make release   # static binaries for linux/darwin/windows × amd64/arm64 in dist/
```

Layout: `main.go` (server, routing, admin) · `config.go` · `store.go` (SQLite) · `proto.go` (wire helpers) ·
`s3.go` · `events.go` · `sfn.go` + `asl.go` (Step Functions) · `ecs.go` + `runner.go` · `logs.go` · `ssm.go` ·
`query.go` (STS, Query routing) · `sqs.go` · `sns.go` · `ses.go` (SES, mailbox) · `iam.go` · `ec2.go` · `kms.go` · `secrets.go` ·
`ecr.go` · `scheduler.go` (Scheduler, `cron()`) · `tags.go` · `tfgen.go` (`terraform-override`) · `plugin.go` (e2e-studio plugin) ·
`console.go` (embedded web console) · `console/` (its React source) · `scripts/` (`terraform-localaws`, `test-terraform.sh`).

## Not emulated (yet)

IAM policy evaluation and signature checks, Lambda, DynamoDB, RDS, CloudFront, ELB, Route 53, ACM, WAF,
Distributed Map, `.waitForTaskToken`, S3 presigned-URL expiry, object lock enforcement, SNS deliveries
other than SQS, real encryption. Unsupported operations answer `UnknownOperationException` naming the
operation (a whole service: `… is not emulated`).

## License

MIT — see [LICENSE](LICENSE).
