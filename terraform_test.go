package main

// What infrastructure-as-code tools (Terraform first) rely on, over HTTP: configuration
// read back exactly as written, the "not configured" error codes, tags everywhere,
// concurrent writes to one resource, and the services they reference (IAM, EC2
// networking, KMS, Secrets Manager, Scheduler). make test-terraform runs the real
// provider against examples/terraform.

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// query sends a Query-protocol request signed for service.
func (c client) query(service string, form url.Values) (int, string) {
	c.t.Helper()
	resp, body := c.raw("POST", "/", []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded",
		"Authorization": "AWS4-HMAC-SHA256 Credential=t/20260101/eu-west-1/" + service + "/aws4_request, SignedHeaders=host, Signature=x"})
	return resp.StatusCode, string(body)
}

func (c client) mustQuery(service string, form url.Values) string {
	c.t.Helper()
	code, body := c.query(service, form)
	if code != 200 {
		c.t.Fatalf("%s %s: %d %s", service, form.Get("Action"), code, body)
	}
	return body
}

func TestS3BucketConfiguration(t *testing.T) {
	_, c := newTestApp(t)
	c.raw("PUT", "/cfg", []byte(`<CreateBucketConfiguration><Tags><Tag><Key>app</Key><Value>x</Value></Tag></Tags></CreateBucketConfiguration>`), nil)
	for sub, code := range map[string]string{"policy": "NoSuchBucketPolicy", "cors": "NoSuchCORSConfiguration", "lifecycle": "NoSuchLifecycleConfiguration",
		"website": "NoSuchWebsiteConfiguration", "publicAccessBlock": "NoSuchPublicAccessBlockConfiguration", "ownershipControls": "OwnershipControlsNotFoundError",
		"replication": "ReplicationConfigurationNotFoundError", "object-lock": "ObjectLockConfigurationNotFoundError"} {
		resp, body := c.raw("GET", "/cfg?"+sub, nil, nil)
		if resp.StatusCode != 404 || !strings.Contains(string(body), "<Code>"+code+"</Code>") {
			t.Fatalf("GET ?%s unset: %d %s", sub, resp.StatusCode, body)
		}
	}
	if _, body := c.raw("GET", "/cfg?encryption", nil, nil); !strings.Contains(string(body), "AES256") {
		t.Fatalf("default encryption %s", body)
	}
	if _, body := c.raw("GET", "/cfg?tagging", nil, nil); !strings.Contains(string(body), "<Key>app</Key><Value>x</Value>") {
		t.Fatalf("tags from CreateBucketConfiguration %s", body)
	}

	// Terraform writes subresources of one bucket concurrently: none may be lost
	docs := map[string]string{
		"policy":            `{"Version":"2012-10-17","Statement":[]}`,
		"cors":              `<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>`,
		"publicAccessBlock": `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls></PublicAccessBlockConfiguration>`,
		"ownershipControls": `<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`,
		"website":           `<WebsiteConfiguration><IndexDocument><Suffix>index.html</Suffix></IndexDocument></WebsiteConfiguration>`,
		"versioning":        `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`,
		"notification":      `<NotificationConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`,
	}
	var wg sync.WaitGroup
	for sub, doc := range docs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, body := c.raw("PUT", "/cfg?"+sub, []byte(doc), nil); resp.StatusCode != 200 {
				t.Errorf("PUT ?%s: %d %s", sub, resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	for sub, doc := range docs {
		if _, body := c.raw("GET", "/cfg?"+sub, nil, nil); !strings.Contains(string(body), doc[strings.Index(doc, ">")+1:][:20]) {
			t.Fatalf("GET ?%s after concurrent writes: %s", sub, body)
		}
	}

	// lifecycle reads back with the minimum object size header the provider waits for
	c.raw("PUT", "/cfg?lifecycle", []byte(`<LifecycleConfiguration><Rule><ID>x</ID><Status>Enabled</Status></Rule></LifecycleConfiguration>`), nil)
	if resp, _ := c.raw("GET", "/cfg?lifecycle", nil, nil); resp.Header.Get("x-amz-transition-default-minimum-object-size") != "all_storage_classes_128K" {
		t.Fatalf("lifecycle header %v", resp.Header)
	}
	if resp, _ := c.raw("DELETE", "/cfg?policy", nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete policy %d", resp.StatusCode)
	}
	if resp, _ := c.raw("GET", "/cfg?policy", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("policy after delete %d", resp.StatusCode)
	}

	// S3 Control tags are the bucket's tags, also when they arrive through the proxy form
	arn := url.PathEscape("arn:aws:s3:::cfg")
	c.raw("POST", "/v20180820/tags/"+arn, []byte(`<TagResourceRequest><Tags><Tag><Key>env</Key><Value>e2e</Value></Tag></Tags></TagResourceRequest>`), nil)
	if _, body := c.raw("GET", "/cfg?tagging", nil, nil); !strings.Contains(string(body), "<Key>env</Key>") || !strings.Contains(string(body), "<Key>app</Key>") {
		t.Fatalf("bucket tags after S3 Control TagResource %s", body)
	}
	c.raw("DELETE", "/v20180820/tags/"+arn+"?tagKeys=app", nil, nil)
	if _, body := c.raw("GET", "/v20180820/tags/"+arn, nil, nil); strings.Contains(string(body), "app") || !strings.Contains(string(body), "env") {
		t.Fatalf("S3 Control list after untag %s", body)
	}

	// object tags: header on PUT, ?tagging read and replace
	c.raw("PUT", "/cfg/k.txt", []byte("x"), map[string]string{"x-amz-tagging": "a=1&b=2"})
	if resp, _ := c.raw("HEAD", "/cfg/k.txt", nil, nil); resp.Header.Get("x-amz-tagging-count") != "2" {
		t.Fatalf("tagging count %v", resp.Header)
	}
	c.raw("PUT", "/cfg/k.txt?tagging", []byte(`<Tagging><TagSet><Tag><Key>c</Key><Value>3</Value></Tag></TagSet></Tagging>`), nil)
	if _, body := c.raw("GET", "/cfg/k.txt?tagging", nil, nil); !strings.Contains(string(body), "<Key>c</Key><Value>3</Value>") || strings.Contains(string(body), "<Key>a</Key>") {
		t.Fatalf("object tagging %s", body)
	}
}

func TestSNSFanOutToSQS(t *testing.T) {
	_, c := newTestApp(t)
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "all"})
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "raw-urgent"})
	arn := between(c.mustQuery("sns", url.Values{"Action": {"CreateTopic"}, "Name": {"events"}, "Tags.member.1.Key": {"app"}, "Tags.member.1.Value": {"x"}}), "<TopicArn>", "</TopicArn>")
	c.mustQuery("sns", url.Values{"Action": {"Subscribe"}, "TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:eu-west-1:123456789012:all"}})
	sub := between(c.mustQuery("sns", url.Values{"Action": {"Subscribe"}, "TopicArn": {arn}, "Protocol": {"sqs"}, "Endpoint": {"arn:aws:sqs:eu-west-1:123456789012:raw-urgent"},
		"Attributes.entry.1.key": {"RawMessageDelivery"}, "Attributes.entry.1.value": {"true"},
		"Attributes.entry.2.key": {"FilterPolicy"}, "Attributes.entry.2.value": {`{"priority":["urgent"]}`}}), "<SubscriptionArn>", "</SubscriptionArn>")
	if body := c.mustQuery("sns", url.Values{"Action": {"GetSubscriptionAttributes"}, "SubscriptionArn": {sub}}); !strings.Contains(body, "<key>FilterPolicyScope</key><value>MessageAttributes</value>") {
		t.Fatalf("subscription attributes %s", body)
	}
	if body := c.mustQuery("sns", url.Values{"Action": {"ListTagsForResource"}, "ResourceArn": {arn}}); !strings.Contains(body, "<Key>app</Key>") {
		t.Fatalf("topic tags %s", body)
	}
	pub := func(msg, prio string) {
		c.mustQuery("sns", url.Values{"Action": {"Publish"}, "TopicArn": {arn}, "Message": {msg}, "Subject": {"s"},
			"MessageAttributes.entry.1.Name": {"priority"}, "MessageAttributes.entry.1.Value.DataType": {"String"}, "MessageAttributes.entry.1.Value.StringValue": {prio}})
	}
	pub("low one", "low")
	pub("urgent one", "urgent")
	recv := func(q string) []any {
		return getList(c.call("AmazonSQS.ReceiveMessage", M{"QueueUrl": q, "MaxNumberOfMessages": 10, "MessageAttributeNames": []string{"All"}}), "Messages")
	}
	all := recv("all")
	if len(all) != 2 {
		t.Fatalf("all: %v", all)
	}
	var env M
	_ = jsonUnmarshal(getStr(all[0].(map[string]any), "Body"), &env)
	if env["Type"] != "Notification" || env["TopicArn"] != arn || env["Message"] != "low one" || getMap(getMap(env, "MessageAttributes"), "priority")["Value"] != "low" {
		t.Fatalf("envelope %v", env)
	}
	raw := recv("raw-urgent")
	if len(raw) != 1 || getStr(raw[0].(map[string]any), "Body") != "urgent one" ||
		getMap(getMap(raw[0].(map[string]any), "MessageAttributes"), "priority")["StringValue"] != "urgent" {
		t.Fatalf("raw + filtered: %v", raw)
	}
}

func TestIAMRolesAndPolicies(t *testing.T) {
	_, c := newTestApp(t)
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	body := c.mustQuery("iam", url.Values{"Action": {"CreateRole"}, "RoleName": {"task"}, "AssumeRolePolicyDocument": {doc}, "Tags.member.1.Key": {"a"}, "Tags.member.1.Value": {"b"}})
	if got, _ := url.QueryUnescape(between(body, "<AssumeRolePolicyDocument>", "</AssumeRolePolicyDocument>")); got != doc {
		t.Fatalf("assume role policy round trip: %q", got)
	}
	if !strings.Contains(body, "<Arn>arn:aws:iam::123456789012:role/task</Arn>") {
		t.Fatalf("role %s", body)
	}
	if code, body := c.query("iam", url.Values{"Action": {"CreateRole"}, "RoleName": {"task"}, "AssumeRolePolicyDocument": {doc}}); code != 409 || !strings.Contains(body, "EntityAlreadyExists") {
		t.Fatalf("duplicate role %d %s", code, body)
	}
	managed := "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
	c.mustQuery("iam", url.Values{"Action": {"AttachRolePolicy"}, "RoleName": {"task"}, "PolicyArn": {managed}})
	c.mustQuery("iam", url.Values{"Action": {"PutRolePolicy"}, "RoleName": {"task"}, "PolicyName": {"inline"}, "PolicyDocument": {`{"Statement":[]}`}})
	if body := c.mustQuery("iam", url.Values{"Action": {"ListAttachedRolePolicies"}, "RoleName": {"task"}}); !strings.Contains(body, "<PolicyName>AmazonECSTaskExecutionRolePolicy</PolicyName>") {
		t.Fatalf("attached %s", body)
	}
	if code, body := c.query("iam", url.Values{"Action": {"DeleteRole"}, "RoleName": {"task"}}); code != 409 || !strings.Contains(body, "DeleteConflict") {
		t.Fatalf("delete with policies %d %s", code, body)
	}
	c.mustQuery("iam", url.Values{"Action": {"DetachRolePolicy"}, "RoleName": {"task"}, "PolicyArn": {managed}})
	c.mustQuery("iam", url.Values{"Action": {"DeleteRolePolicy"}, "RoleName": {"task"}, "PolicyName": {"inline"}})
	c.mustQuery("iam", url.Values{"Action": {"DeleteRole"}, "RoleName": {"task"}})
	if code, body := c.query("iam", url.Values{"Action": {"GetRole"}, "RoleName": {"task"}}); code != 404 || !strings.Contains(body, "NoSuchEntity") {
		t.Fatalf("deleted role %d %s", code, body)
	}

	pol := c.mustQuery("iam", url.Values{"Action": {"CreatePolicy"}, "PolicyName": {"logs"}, "PolicyDocument": {`{"Statement":[{"Effect":"Allow","Action":"logs:*","Resource":"*"}]}`}})
	arn := between(pol, "<Arn>", "</Arn>")
	c.mustQuery("iam", url.Values{"Action": {"CreatePolicyVersion"}, "PolicyArn": {arn}, "PolicyDocument": {`{"Statement":[]}`}, "SetAsDefault": {"true"}})
	if body := c.mustQuery("iam", url.Values{"Action": {"GetPolicy"}, "PolicyArn": {arn}}); !strings.Contains(body, "<DefaultVersionId>v2</DefaultVersionId>") {
		t.Fatalf("policy after new default version %s", body)
	}
}

func TestEC2NetworksFeedECS(t *testing.T) {
	_, c := newTestApp(t)
	vpc := between(c.mustQuery("ec2", url.Values{"Action": {"CreateVpc"}, "CidrBlock": {"10.1.0.0/16"},
		"TagSpecification.1.ResourceType": {"vpc"}, "TagSpecification.1.Tag.1.Key": {"Name"}, "TagSpecification.1.Tag.1.Value": {"main"}}), "<vpcId>", "</vpcId>")
	if body := c.mustQuery("ec2", url.Values{"Action": {"DescribeRouteTables"}, "Filter.1.Name": {"vpc-id"}, "Filter.1.Value.1": {vpc},
		"Filter.2.Name": {"association.main"}, "Filter.2.Value.1": {"true"}}); !strings.Contains(body, "<main>true</main>") {
		t.Fatalf("main route table %s", body)
	}
	if body := c.mustQuery("ec2", url.Values{"Action": {"DescribeSecurityGroups"}, "Filter.1.Name": {"vpc-id"}, "Filter.1.Value.1": {vpc},
		"Filter.2.Name": {"group-name"}, "Filter.2.Value.1": {"default"}}); !strings.Contains(body, "<groupName>default</groupName>") {
		t.Fatalf("default security group %s", body)
	}
	if body := c.mustQuery("ec2", url.Values{"Action": {"DescribeVpcs"}, "Filter.1.Name": {"tag:Name"}, "Filter.1.Value.1": {"ma*"}}); !strings.Contains(body, vpc) {
		t.Fatalf("tag filter with wildcard %s", body)
	}
	sub := between(c.mustQuery("ec2", url.Values{"Action": {"CreateSubnet"}, "VpcId": {vpc}, "CidrBlock": {"10.1.1.0/24"}}), "<subnetId>", "</subnetId>")
	sg := between(c.mustQuery("ec2", url.Values{"Action": {"CreateSecurityGroup"}, "GroupName": {"web"}, "GroupDescription": {"web"}, "VpcId": {vpc}}), "<groupId>", "</groupId>")
	rule := url.Values{"Action": {"AuthorizeSecurityGroupIngress"}, "GroupId": {sg}, "IpPermissions.1.IpProtocol": {"tcp"},
		"IpPermissions.1.FromPort": {"443"}, "IpPermissions.1.ToPort": {"443"}, "IpPermissions.1.IpRanges.1.CidrIp": {"10.0.0.0/8"}}
	if body := c.mustQuery("ec2", rule); !strings.Contains(body, "<securityGroupRuleId>sgr-") {
		t.Fatalf("authorize %s", body)
	}
	if code, body := c.query("ec2", rule); code != 400 || !strings.Contains(body, "<Response><Errors><Error><Code>InvalidPermission.Duplicate</Code>") {
		t.Fatalf("duplicate rule %d %s", code, body)
	}
	if code, body := c.query("ec2", url.Values{"Action": {"DeleteVpc"}, "VpcId": {vpc}}); code != 400 || !strings.Contains(body, "DependencyViolation") {
		t.Fatalf("delete vpc with subnets %d %s", code, body)
	}
	if code, body := c.query("ec2", url.Values{"Action": {"DescribeSubnets"}, "SubnetId.1": {"subnet-nope"}}); code != 400 || !strings.Contains(body, "InvalidSubnetID.NotFound") {
		t.Fatalf("missing subnet %d %s", code, body)
	}

	// awsvpc RunTask is validated against the subnets that exist
	c.call("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition", M{"family": "w", "networkMode": "awsvpc",
		"containerDefinitions": []any{M{"name": "w", "image": "alpine", "essential": true}}})
	run := func(subnet string) int {
		resp, _ := c.raw("POST", "/", []byte(jsonStr(M{"taskDefinition": "w", "networkConfiguration": M{"awsvpcConfiguration": M{"subnets": []any{subnet}}}})),
			map[string]string{"X-Amz-Target": "AmazonEC2ContainerServiceV20141113.RunTask", "Content-Type": "application/x-amz-json-1.1"})
		return resp.StatusCode
	}
	if run(sub) != 200 || run("subnet-unknown") != 400 {
		t.Fatalf("RunTask subnet validation")
	}
}

func TestTagsAcrossJSONServices(t *testing.T) {
	_, c := newTestApp(t)
	c.call("Logs_20140328.CreateLogGroup", M{"logGroupName": "/g", "tags": M{"a": "1"}})
	if tags := getMap(c.call("Logs_20140328.ListTagsForResource", M{"resourceArn": "arn:aws:logs:eu-west-1:123456789012:log-group:/g:*"}), "tags"); tags["a"] != "1" {
		t.Fatalf("log group tags %v", tags)
	}
	c.call("AmazonSSM.PutParameter", M{"Name": "/p", "Type": "SecureString", "Value": "v", "Tags": []any{M{"Key": "a", "Value": "1"}}})
	if l := getList(c.call("AmazonSSM.ListTagsForResource", M{"ResourceType": "Parameter", "ResourceId": "/p"}), "TagList"); len(l) != 1 {
		t.Fatalf("parameter tags %v", l)
	}
	c.call("AmazonSSM.PutParameter", M{"Name": "/p", "Value": "w", "Overwrite": true})
	if d := getList(c.call("AmazonSSM.DescribeParameters", M{}), "Parameters"); getStr(d[0].(map[string]any), "KeyId") != "alias/aws/ssm" {
		t.Fatalf("overwrite keeps the key %v", d)
	}
	c.call("AWSEvents.PutRule", M{"Name": "r", "ScheduleExpression": "cron(0 12 * * ? *)", "Tags": []any{M{"Key": "a", "Value": "1"}}})
	c.call("AWSEvents.PutTargets", M{"Rule": "r", "Targets": []any{M{"Id": "1", "Arn": "arn:aws:ecs:eu-west-1:123456789012:cluster/default",
		"EcsParameters": M{"TaskDefinitionArn": "x", "TaskCount": 1}, "RetryPolicy": M{"MaximumRetryAttempts": 2}}}})
	tg := getList(c.call("AWSEvents.ListTargetsByRule", M{"Rule": "r"}), "Targets")[0].(map[string]any)
	if getMap(tg, "EcsParameters")["TaskDefinitionArn"] != "x" || getMap(tg, "RetryPolicy") == nil || tg["SqsParameters"] != nil {
		t.Fatalf("targets keep every field (and no empty SqsParameters): %v", tg)
	}
	if l := getList(c.call("AWSEvents.ListTagsForResource", M{"ResourceARN": "arn:aws:events:eu-west-1:123456789012:rule/r"}), "Tags"); len(l) != 1 {
		t.Fatalf("rule tags %v", l)
	}
	cl := c.call("AmazonEC2ContainerServiceV20141113.CreateCluster", M{"clusterName": "c", "settings": []any{M{"name": "containerInsights", "value": "enabled"}},
		"tags": []any{M{"key": "a", "value": "1"}}})
	if s := getList(getMap(cl, "cluster"), "settings"); getStr(s[0].(map[string]any), "value") != "enabled" {
		t.Fatalf("cluster settings %v", cl)
	}
	if l := getList(c.call("AmazonEC2ContainerServiceV20141113.ListTagsForResource", M{"resourceArn": "arn:aws:ecs:eu-west-1:123456789012:cluster/c"}), "tags"); len(l) != 1 {
		t.Fatalf("cluster tags %v", l)
	}
}

func TestKMSAndSecrets(t *testing.T) {
	_, c := newTestApp(t)
	key := getMap(c.call("TrentService.CreateKey", M{"Description": "d", "Tags": []any{M{"TagKey": "a", "TagValue": "1"}}}), "KeyMetadata")
	c.call("TrentService.CreateAlias", M{"AliasName": "alias/app", "TargetKeyId": key["KeyId"]})
	enc := c.call("TrentService.Encrypt", M{"KeyId": "alias/app", "Plaintext": "aGVsbG8="})
	dec := c.call("TrentService.Decrypt", M{"CiphertextBlob": enc["CiphertextBlob"]})
	if dec["Plaintext"] != "aGVsbG8=" || dec["KeyId"] != key["Arn"] {
		t.Fatalf("decrypt %v", dec)
	}
	c.call("TrentService.ScheduleKeyDeletion", M{"KeyId": key["KeyId"], "PendingWindowInDays": 7})
	if st := getMap(c.call("TrentService.DescribeKey", M{"KeyId": key["Arn"]}), "KeyMetadata")["KeyState"]; st != "PendingDeletion" {
		t.Fatalf("key state %v", st)
	}

	s := c.call("secretsmanager.CreateSecret", M{"Name": "db", "SecretString": `{"user":"app","password":"one"}`})
	c.call("secretsmanager.PutSecretValue", M{"SecretId": "db", "SecretString": `{"user":"app","password":"two"}`})
	prev := c.call("secretsmanager.GetSecretValue", M{"SecretId": s["ARN"], "VersionStage": "AWSPREVIOUS"})
	if !strings.Contains(getStr(prev, "SecretString"), "one") {
		t.Fatalf("previous version %v", prev)
	}
	app, c2 := newTestApp(t)
	c2.call("secretsmanager.CreateSecret", M{"Name": "db", "SecretString": `{"password":"pw"}`})
	arn := getStr(c2.call("secretsmanager.DescribeSecret", M{"SecretId": "db"}), "ARN")
	if v, ok := app.secretValue(arn + ":password::"); !ok || v != "pw" {
		t.Fatalf("ECS valueFrom with a JSON key: %q %v", v, ok)
	}
	if v, ok := app.secretValue(strings.TrimSuffix(arn, arn[len(arn)-7:])); !ok || v != `{"password":"pw"}` {
		t.Fatalf("partial ARN: %q %v", v, ok)
	}
}

func TestCron(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	for _, tc := range []struct {
		expr, when string
		want       bool
	}{
		{"cron(0 2 * * ? *)", "2026-10-12 02:00", true},
		{"cron(0 2 * * ? *)", "2026-10-12 02:01", false},
		{"cron(*/15 * * * ? *)", "2026-10-12 09:45", true},
		{"cron(0 9 ? * MON-FRI *)", "2026-10-12 09:00", true},  // Monday
		{"cron(0 9 ? * MON-FRI *)", "2026-10-11 09:00", false}, // Sunday
		{"cron(0 0 L * ? *)", "2026-02-28 00:00", true},
		{"cron(0 0 ? * 6L *)", "2026-10-30 00:00", true},  // last Friday
		{"cron(0 0 ? * 2#2 *)", "2026-10-12 00:00", true}, // second Monday
		{"cron(0 0 1W * ? *)", "2026-08-03 00:00", true},  // Aug 1 2026 is a Saturday → Monday 3rd
		{"cron(0 10 * JAN,OCT ? 2026)", "2026-10-11 10:00", true},
	} {
		c, err := parseCron(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		if got := c.match(at(tc.when)); got != tc.want {
			t.Errorf("%s at %s: %v, want %v", tc.expr, tc.when, got, tc.want)
		}
	}
	for _, bad := range []string{"cron(0 2 * * * *)", "cron(0 2 * *)", "cron(61 * * * ? *)"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%s should not parse", bad)
		}
	}
}

func TestSchedulerFires(t *testing.T) {
	a, c := newTestApp(t)
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "nightly"})
	body := jsonStr(M{"ScheduleExpression": "at(2020-01-01T00:00:00)", "FlexibleTimeWindow": M{"Mode": "OFF"}, "ActionAfterCompletion": "DELETE",
		"Target": M{"Arn": "arn:aws:sqs:eu-west-1:123456789012:nightly", "RoleArn": "arn:aws:iam::123456789012:role/s", "Input": `{"job":"x"}`}})
	hdr := map[string]string{"Content-Type": "application/json", "Authorization": "AWS4-HMAC-SHA256 Credential=t/20260101/eu-west-1/scheduler/aws4_request, SignedHeaders=host, Signature=x"}
	if resp, b := c.raw("POST", "/schedules/once", []byte(body), hdr); resp.StatusCode != 200 || !strings.Contains(string(b), "schedule/default/once") {
		t.Fatalf("create schedule %d %s", resp.StatusCode, b)
	}
	if resp, b := c.raw("GET", "/schedules/once", nil, hdr); resp.StatusCode != 200 || !strings.Contains(string(b), `"ScheduleExpressionTimezone":"UTC"`) {
		t.Fatalf("get schedule %d %s", resp.StatusCode, b)
	}
	f := &firing{last: map[string]time.Time{}}
	a.fireSchedules(f, time.Now())
	msgs := getList(c.call("AmazonSQS.ReceiveMessage", M{"QueueUrl": "nightly"}), "Messages")
	if len(msgs) != 1 || getStr(msgs[0].(map[string]any), "Body") != `{"job":"x"}` {
		t.Fatalf("schedule delivery %v", msgs)
	}
	if resp, _ := c.raw("GET", "/schedules/once", nil, hdr); resp.StatusCode != 404 {
		t.Fatalf("at() with ActionAfterCompletion=DELETE should be gone: %d", resp.StatusCode)
	}
}

func TestTerraformOverride(t *testing.T) {
	_, c := newTestApp(t)
	dir := t.TempDir()
	mod := `terraform {
  backend "s3" {
    bucket = "corp-state" # the real one
    key    = "api/terraform.tfstate"
    region = "ap-northeast-1"
  }
}
provider "aws" {
  region  = "ap-northeast-1"
  profile = "corp"
  default_tags {
    tags = { alias = "not-an-alias" }
  }
}
provider "aws" {
  alias = "us-east-1"
}
# provider "aws" { alias = "commented" }
data "terraform_remote_state" "general" {
  backend = "s3"
  config = {
    bucket = "corp-state"
    key    = "general/terraform.tfstate"
    region = "ap-northeast-1"
  }
}
data "terraform_remote_state" "local_one" {
  backend = "local"
  config  = { path = "x.tfstate" }
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs strings.Builder
	if rc := terraformOverride([]string{"-dir", dir, "-endpoint", c.url}, &out, &errs); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errs.String())
	}
	got := out.String()
	for _, want := range []string{`backend "s3"`, `bucket = "corp-state"`, `key = "api/terraform.tfstate"`, `use_lockfile = true`,
		`data "terraform_remote_state" "general"`, `key = "general/terraform.tfstate"`, `alias                       = "us-east-1"`,
		`region                      = "eu-west-1"`, `profile                     = null`, `s3control      = "` + c.url + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("override lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "commented") || strings.Contains(got, `"local_one"`) || strings.Count(got, `region                      =`) != 1 {
		t.Errorf("only the alias without a region gets one; comments and non-s3 remote states are ignored:\n%s", got)
	}
	if resp, _ := c.raw("HEAD", "/corp-state", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("state bucket not created: %d", resp.StatusCode)
	}
	if rc := terraformOverride([]string{"-dir", dir, "-endpoint", c.url, "-backend", "local", "-o", "localaws_override.tf"}, &out, &errs); rc != 0 {
		t.Fatal(errs.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "localaws_override.tf")); !strings.Contains(string(b), `backend "local"`) {
		t.Fatalf("-backend local -o: %s", b)
	}
}

func TestS3UnknownSubresources(t *testing.T) {
	_, c := newTestApp(t)
	c.raw("PUT", "/sub", nil, nil)
	if resp, body := c.raw("PUT", "/sub?abac", []byte(`<AbacStatus><Status>Enabled</Status></AbacStatus>`), nil); resp.StatusCode != 200 {
		t.Fatalf("PUT unknown bucket configuration is not a CreateBucket: %d %s", resp.StatusCode, body)
	}
	if _, body := c.raw("GET", "/sub?abac", nil, nil); !strings.Contains(string(body), "<Status>Enabled</Status>") {
		t.Fatalf("GET it back: %s", body)
	}
	if resp, _ := c.raw("GET", "/sub?metadataTable", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("unset unknown configuration: %d", resp.StatusCode)
	}
	if _, body := c.raw("GET", "/sub?list-type=2&prefix=a&x-id=ListObjectsV2", nil, nil); !strings.Contains(string(body), "<ListBucketResult") {
		t.Fatalf("listing params are not subresources: %s", body)
	}
	c.raw("PUT", "/sub/obj", []byte("body"), nil)
	if resp, _ := c.raw("PUT", "/sub/obj?retention", []byte("<Retention/>"), nil); resp.StatusCode != 501 {
		t.Fatalf("object retention: %d", resp.StatusCode)
	}
	if _, body := c.raw("GET", "/sub/obj", nil, nil); string(body) != "body" {
		t.Fatalf("object clobbered by an unsupported subresource: %q", body)
	}
}
