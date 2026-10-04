# localaws

A small AWS emulator that **runs things for real**. One static binary (~12 MB, pure Go,
SQLite inside), one port, starts in well under a second.

- The AWS SDKs and CLI talk to it unchanged — set `AWS_ENDPOINT_URL=http://localhost:4566`.
- State machines are **interpreted** state by state, with the execution history AWS would write.
- ECS tasks **run**: as containers (docker), host processes (exec), or no-ops for pure orchestration tests.
- S3 → EventBridge → Step Functions → ECS → CloudWatch Logs behaves as one pipeline, end to end.
- Everything persists in `data/localaws.sqlite` and is browsable at `http://localhost:4566/_localaws/`.

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
| **S3** | buckets (create/delete/head/list, versioning, location, EventBridge notification), objects (put/get/head/delete/copy, `Range`, `If-Match`/`If-None-Match`, user metadata), ListObjects v1/v2 (prefix, delimiter, continuation), ListObjectVersions, DeleteObjects, multipart (create/upload part/complete/abort/list) | version ids and delete markers, `null` versions on unversioned buckets, multipart ETag `md5-of-md5s-N`, 5 MiB minimum part size, aws-chunked bodies (SigV4 streaming / CRC trailers), path- and virtual-host-style addressing, `BucketNotEmpty`, `NoSuchKey`, … |
| **EventBridge** | Put/Describe/List/Delete/Enable/Disable rule, Put/Remove/List targets, PutEvents, ListRuleNamesByTarget | full pattern syntax (prefix, suffix, wildcard, equals-ignore-case, anything-but, numeric, exists, cidr, `$or`, arrays), Input / InputPath / InputTransformer, `rate()` schedules; S3 `Object Created` / `Object Deleted` events; targets: state machines, log groups |
| **Step Functions** | Create/Update/Delete/Describe/List state machine, ValidateStateMachineDefinition, Start/StartSync/Stop/Describe/List execution, GetExecutionHistory, DescribeStateMachineForExecution | Task, Pass, Choice, Wait, Parallel, Map, Succeed, Fail; InputPath / Parameters / ResultSelector / ResultPath / OutputPath; context object `$$`; all `States.*` intrinsics; Retry (backoff, MaxDelaySeconds, jitter) and Catch; TimeoutSeconds; definition validation; history events with the real types and `previousEventId` chain. Integrations: `ecs:runTask(.sync)`, `states:startExecution(.sync/.sync:2)`, `events:putEvents`, `sns:publish` |
| **ECS** | clusters, Register/Describe/List/Deregister task definition, RunTask, DescribeTasks, ListTasks, StopTask, Create/Update/Delete/Describe/List service | revisions, task lifecycle (PROVISIONING → PENDING → RUNNING → STOPPED), stop codes, exit codes per container, `essential`, awsvpc validation (network configuration, subnets), `secrets` from SSM, `environmentFiles` from S3, awslogs streams `<prefix>/<container>/<task id>`, services kept at `desiredCount` |
| **CloudWatch Logs** | groups, streams, PutLogEvents, GetLogEvents, FilterLogEvents, retention | forward/backward tokens (same token = no more events), filter patterns incl. `{ $.field = value }` |
| **SSM** | Put/Get/GetParameters/ByPath/Describe/Delete(s)/History | versions, Overwrite, SecureString returned encrypted unless `WithDecryption` |
| **STS / EC2 / SNS** | GetCallerIdentity, AssumeRole, GetSessionToken · DescribeSubnets/SecurityGroups/Vpcs/Regions · CreateTopic/ListTopics/Publish | the networks ECS validates against come from the config |

Requests are routed by `X-Amz-Target`, by the SigV4 signing scope, or else treated as S3.
Signatures are **not** verified and IAM is **not** enforced.

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
    "sns":           { "topics": [ "alerts" ] }
  }
}
```

A task definition or state machine exported from a real account (`aws ecs describe-task-definition`,
`aws stepfunctions describe-state-machine`) can be dropped in as is; map its images with
`runner.docker.images`, its ARNs follow `account` / `region`.

## Admin

| | |
|---|---|
| `GET /_localaws/health` | status, version, runner |
| `POST /_localaws/reset` | stop everything, wipe all data, re-apply the config |
| `/_localaws/` | console: executions + history, tasks + container logs, bucket versions, log groups, parameters, API calls |

## Development

```bash
make test      # unit tests (ASL, patterns, filters) + HTTP integration tests (runner=noop)
make release   # static binaries for linux/darwin/windows × amd64/arm64 in dist/
```

Layout: `main.go` (server, routing, admin) · `config.go` · `store.go` (SQLite) · `proto.go` (wire helpers) ·
`s3.go` · `events.go` · `sfn.go` + `asl.go` (Step Functions) · `ecs.go` + `runner.go` · `logs.go` · `ssm.go` ·
`query.go` (STS/EC2/SNS) · `ui.go`.

## Not emulated (yet)

IAM policies and signature checks, Lambda, SQS, DynamoDB, Secrets Manager, KMS, cron schedules
(`rate()` only), Distributed Map, `.waitForTaskToken`, S3 presigned-URL expiry, object lock, encryption
headers. Unsupported operations answer `UnknownOperationException` naming the operation.

## License

MIT — see [LICENSE](LICENSE).
