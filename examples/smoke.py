"""Smoke test with the real AWS SDK (boto3): every emulated service, against a running
localaws started with examples/pipeline/localaws.json.

    localaws -config examples/pipeline/localaws.json -runner noop      (or docker)
    AWS_ENDPOINT_URL=http://localhost:4566 python examples/smoke.py

Exits non-zero on the first broken expectation.
"""
import json
import os
import sys
import tempfile
import time

import boto3
from boto3.s3.transfer import TransferConfig

os.environ.setdefault("AWS_ENDPOINT_URL", "http://localhost:4566")
os.environ.setdefault("AWS_ACCESS_KEY_ID", "test")
os.environ.setdefault("AWS_SECRET_ACCESS_KEY", "test")
s = boto3.Session(region_name=os.environ.get("AWS_REGION", "eu-west-1"))
sts, s3, sfn, ecs, logs, ssm, ev, ec2 = (s.client(n) for n in ("sts", "s3", "stepfunctions", "ecs", "logs", "ssm", "events", "ec2"))


def check(label, cond, detail=""):
    print(("ok   " if cond else "FAIL ") + label + (f"  ({detail})" if detail and not cond else ""))
    if not cond:
        sys.exit(1)


ident = sts.get_caller_identity()
check("sts identity", ident["Account"] == "123456789012", ident)
check("ec2 subnets", [x["SubnetId"] for x in ec2.describe_subnets()["Subnets"]] == ["subnet-0a1b2c3d"])
p = ssm.get_parameter(Name="/worker/greeting", WithDecryption=True)["Parameter"]
check("ssm SecureString decrypted", p["Value"].startswith("hello"), p)
check("ssm SecureString stays encrypted without the flag", ssm.get_parameter(Name="/worker/greeting")["Parameter"]["Value"] != p["Value"])

# S3: multipart through the SDK's transfer manager, ranged read, versions
with tempfile.NamedTemporaryFile(delete=False) as f:
    f.write(os.urandom(12 * 1024 * 1024))
s3.upload_file(f.name, "inbox", "archive/big.bin", Config=TransferConfig(multipart_threshold=5 * 1024 * 1024, multipart_chunksize=5 * 1024 * 1024))
head = s3.head_object(Bucket="inbox", Key="archive/big.bin")
check("s3 multipart upload via TransferManager", head["ContentLength"] == 12 * 1024 * 1024 and head["ETag"].endswith('-3"'), head["ETag"])
rng = s3.get_object(Bucket="inbox", Key="archive/big.bin", Range="bytes=0-9")["Body"].read()
check("s3 ranged GET", len(rng) == 10)
pages = list(s3.get_paginator("list_objects_v2").paginate(Bucket="inbox", Delimiter="/"))
check("s3 delimiter listing", any(cp["Prefix"] == "archive/" for pg in pages for cp in pg.get("CommonPrefixes", [])))

# The pipeline: upload → EventBridge rule → state machine → ECS task → logs
sm_arn = "arn:aws:states:eu-west-1:123456789012:stateMachine:on-upload"
since = time.time()
put = s3.put_object(Bucket="inbox", Key="incoming/report-1.csv", Body=b"a,b\n1,2\n")
check("s3 versioned put", put.get("VersionId") not in (None, "null"))
arn = None
for _ in range(100):
    for e in sfn.list_executions(stateMachineArn=sm_arn)["executions"]:
        d = sfn.describe_execution(executionArn=e["executionArn"])
        if "incoming/report-1.csv" in d["input"] and e["startDate"].timestamp() >= since - 1:
            arn = e["executionArn"]
    if arn:
        break
    time.sleep(0.2)
check("EventBridge started the state machine", arn is not None)
for _ in range(300):
    d = sfn.describe_execution(executionArn=arn)
    if d["status"] != "RUNNING":
        break
    time.sleep(0.5)
check("execution succeeded", d["status"] == "SUCCEEDED", f"{d['status']} {d.get('error')} {d.get('cause', '')[:300]}")
out = json.loads(d["output"])
hist = sfn.get_execution_history(executionArn=arn)["events"]
types = [h["type"] for h in hist]
check("history shape", types[:7] == ["ExecutionStarted", "TaskStateEntered", "TaskScheduled", "TaskStarted", "TaskSubmitted", "TaskSucceeded", "TaskStateExited"], types)
task_arn = out["task"]["taskArn"]
t = ecs.describe_tasks(cluster="work", tasks=[task_arn])["tasks"][0]
check("ecs task stopped cleanly", t["lastStatus"] == "STOPPED" and t["stopCode"] == "EssentialContainerExited", t.get("stoppedReason"))
check("ecs command came from the state machine", "incoming/report-1.csv" in json.dumps(t["overrides"]))
stream = f"worker/app/{task_arn.rsplit('/', 1)[1]}"
lines = []
for _ in range(20):
    lines = [e["message"] for e in logs.get_log_events(logGroupName="/ecs/worker", logStreamName=stream, startFromHead=True)["events"]]
    if lines:
        break
    time.sleep(0.3)
check("container output in CloudWatch Logs", bool(lines), stream)
print("     " + " | ".join(lines[:4]))
found = logs.filter_log_events(logGroupName="/ecs/worker", filterPattern='"processing"')["events"]
check("FilterLogEvents", len(found) >= 1)
print("all good")
