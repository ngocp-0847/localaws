package main

// End to end over HTTP, the way an SDK talks to it: an object lands in a bucket with
// EventBridge notifications, a rule starts a state machine, the state machine runs an
// ECS task (runner=noop), retries, catches, maps — and the history, task and log stream
// read back through the same APIs.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type client struct {
	t   *testing.T
	url string
}

func (c client) call(target string, in any) M {
	c.t.Helper()
	req, _ := http.NewRequest("POST", c.url+"/", bytes.NewReader([]byte(jsonStr(in))))
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out M
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		c.t.Fatalf("%s: %d %v", target, resp.StatusCode, out)
	}
	return out
}

func (c client) raw(method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func newTestApp(t *testing.T) (*App, client) {
	t.Helper()
	cfg, err := loadConfig([]string{"-data", t.TempDir(), "-runner", "noop", "-account", "123456789012", "-region", "eu-west-1", "-time-scale", "0.01"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close) // runs after the server is closed (cleanups are LIFO)
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, client{t, srv.URL}
}

func waitExec(t *testing.T, c client, arn string) M {
	t.Helper()
	for i := 0; i < 200; i++ {
		d := c.call("AWSStepFunctions.DescribeExecution", M{"executionArn": arn})
		if d["status"] != "RUNNING" {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("execution did not finish")
	return nil
}

func TestS3Basics(t *testing.T) {
	_, c := newTestApp(t)
	if r, _ := c.raw("PUT", "/data", nil, nil); r.StatusCode != 200 {
		t.Fatalf("create bucket: %d", r.StatusCode)
	}
	c.raw("PUT", "/data?versioning", []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), nil)
	r1, _ := c.raw("PUT", "/data/a/one.txt", []byte("0123456789"), map[string]string{"x-amz-meta-owner": "me"})
	r2, _ := c.raw("PUT", "/data/a/one.txt", []byte("v2"), nil)
	v1 := r1.Header.Get("x-amz-version-id")
	if v1 == "" || v1 == r2.Header.Get("x-amz-version-id") {
		t.Fatal("versioned puts must return distinct version ids")
	}
	r, b := c.raw("GET", "/data/a/one.txt?versionId="+v1, nil, map[string]string{"Range": "bytes=2-4"})
	if r.StatusCode != 206 || string(b) != "234" || r.Header.Get("x-amz-meta-owner") != "me" {
		t.Fatalf("ranged versioned get: %d %q %v", r.StatusCode, b, r.Header)
	}
	c.raw("PUT", "/data/b.txt", []byte("b"), nil)
	_, list := c.raw("GET", "/data?list-type=2&delimiter=/", nil, nil)
	if !strings.Contains(string(list), "<Prefix>a/</Prefix>") || !strings.Contains(string(list), "<Key>b.txt</Key>") {
		t.Fatalf("delimiter listing: %s", list)
	}
	if r, _ := c.raw("DELETE", "/data/b.txt", nil, nil); r.Header.Get("x-amz-delete-marker") != "true" {
		t.Fatal("delete on a versioned bucket must create a delete marker")
	}
	if r, _ := c.raw("GET", "/data/b.txt", nil, nil); r.StatusCode != 404 {
		t.Fatalf("deleted object: %d", r.StatusCode)
	}
	// multipart
	_, init := c.raw("POST", "/data/big.bin?uploads", nil, nil)
	id := between(string(init), "<UploadId>", "</UploadId>")
	p1 := bytes.Repeat([]byte("a"), 5<<20)
	rp1, _ := c.raw("PUT", "/data/big.bin?partNumber=1&uploadId="+id, p1, nil)
	rp2, _ := c.raw("PUT", "/data/big.bin?partNumber=2&uploadId="+id, []byte("tail"), nil)
	done := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + rp1.Header.Get("ETag") + `</ETag></Part><Part><PartNumber>2</PartNumber><ETag>` +
		rp2.Header.Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`
	if r, b := c.raw("POST", "/data/big.bin?uploadId="+id, []byte(done), nil); r.StatusCode != 200 || !strings.Contains(string(b), "-2&quot;") {
		t.Fatalf("complete multipart: %d %s", r.StatusCode, b)
	}
	if r, _ := c.raw("HEAD", "/data/big.bin", nil, nil); r.Header.Get("Content-Length") != "5242884" {
		t.Fatalf("multipart size: %s", r.Header.Get("Content-Length"))
	}
	if r, _ := c.raw("DELETE", "/data", nil, nil); r.StatusCode != 409 {
		t.Fatalf("deleting a non-empty bucket: %d", r.StatusCode)
	}
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	v, _, _ := strings.Cut(rest, b)
	return v
}

func TestPipeline(t *testing.T) {
	a, c := newTestApp(t)
	c.call("AmazonEC2ContainerServiceV20141113.CreateCluster", M{"clusterName": "work"})
	c.call("Logs_20140328.CreateLogGroup", M{"logGroupName": "/ecs/worker"})
	c.call("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition", M{"family": "worker", "networkMode": "awsvpc",
		"containerDefinitions": []any{M{"name": "app", "image": "example/app:1", "command": []any{"run"},
			"environment":      []any{M{"name": "MODE", "value": "batch"}},
			"secrets":          []any{M{"name": "DB_PASS", "valueFrom": "/app/db-pass"}},
			"logConfiguration": M{"logDriver": "awslogs", "options": M{"awslogs-group": "/ecs/worker", "awslogs-stream-prefix": "w"}}}}})
	c.call("AmazonSSM.PutParameter", M{"Name": "/app/db-pass", "Type": "SecureString", "Value": "s3cret"})
	def := `{"StartAt":"Import","States":{
	  "Import":{"Type":"Task","Resource":"arn:aws:states:::ecs:runTask.sync","ResultPath":null,
	    "Parameters":{"Cluster":"work","TaskDefinition":"worker","LaunchType":"FARGATE",
	      "NetworkConfiguration":{"AwsvpcConfiguration":{"Subnets":["subnet-1"]}},
	      "Overrides":{"ContainerOverrides":[{"Name":"app","Command.$":"States.Array('import', States.Format('--key={}', $.detail.object.key))"}]}},
	    "Next":"Fanout"},
	  "Fanout":{"Type":"Map","ItemsPath":"$.items","ItemSelector":{"v.$":"$$.Map.Item.Value"},"ResultPath":"$.mapped",
	    "ItemProcessor":{"StartAt":"Double","States":{"Double":{"Type":"Pass","Parameters":{"d.$":"States.MathAdd($.v, $.v)"},"End":true}}},"Next":"Branch"},
	  "Branch":{"Type":"Choice","Choices":[{"Variable":"$.detail.object.key","StringMatches":"*.fail","Next":"Flaky"}],"Default":"Done"},
	  "Flaky":{"Type":"Task","Resource":"arn:aws:states:::ecs:runTask.sync",
	    "Parameters":{"Cluster":"work","TaskDefinition":"worker","NetworkConfiguration":{"AwsvpcConfiguration":{"Subnets":["subnet-1"]}},
	      "Overrides":{"ContainerOverrides":[{"Name":"app","Environment":[{"Name":"LOCALAWS_NOOP_EXIT","Value":"3"}]}]}},
	    "Retry":[{"ErrorEquals":["States.TaskFailed"],"MaxAttempts":1,"IntervalSeconds":1}],
	    "Catch":[{"ErrorEquals":["States.ALL"],"ResultPath":"$.error","Next":"Done"}],"Next":"Done"},
	  "Done":{"Type":"Succeed"}}}`
	sm := c.call("AWSStepFunctions.CreateStateMachine", M{"name": "pipeline", "definition": def, "roleArn": "arn:aws:iam::123456789012:role/x"})
	c.call("AWSEvents.PutRule", M{"Name": "on-upload", "EventPattern": `{"source":["aws.s3"],"detail":{"bucket":{"name":["inbox"]}}}`})
	c.call("AWSEvents.PutTargets", M{"Rule": "on-upload", "Targets": []any{M{"Id": "1", "Arn": sm["stateMachineArn"],
		"InputTransformer": M{"InputPathsMap": M{"key": "$.detail.object.key"}, "InputTemplate": `{"detail":{"object":{"key":"<key>"}},"items":[1,2,3]}`}}}})
	c.raw("PUT", "/inbox", nil, nil)
	c.raw("PUT", "/inbox?notification", []byte(`<NotificationConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`), nil)

	for _, key := range []string{"in/ok.csv", "in/bad.fail"} {
		c.raw("PUT", "/inbox/"+key, []byte("x"), nil)
		var exec M
		for i := 0; i < 100 && exec == nil; i++ {
			for _, e := range getList(c.call("AWSStepFunctions.ListExecutions", M{"stateMachineArn": sm["stateMachineArn"]}), "executions") {
				d := c.call("AWSStepFunctions.DescribeExecution", M{"executionArn": e.(map[string]any)["executionArn"]})
				if strings.Contains(getStr(d, "input"), key) {
					exec = d
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if exec == nil {
			t.Fatalf("%s: no execution started", key)
		}
		done := waitExec(t, c, getStr(exec, "executionArn"))
		if done["status"] != "SUCCEEDED" {
			t.Fatalf("%s: %v %v %v", key, done["status"], done["error"], done["cause"])
		}
		out := getStr(done, "output")
		if !strings.Contains(out, `"mapped":[{"d":2},{"d":4},{"d":6}]`) {
			t.Errorf("%s: map output %s", key, out)
		}
		if key == "in/bad.fail" && !strings.Contains(out, `"Error":"States.TaskFailed"`) {
			t.Errorf("caught error missing from output: %s", out)
		}
		types := []string{}
		for _, e := range getList(c.call("AWSStepFunctions.GetExecutionHistory", M{"executionArn": getStr(done, "executionArn")}), "events") {
			types = append(types, getStr(e.(map[string]any), "type"))
		}
		h := strings.Join(types, " ")
		for _, want := range []string{"ExecutionStarted TaskStateEntered TaskScheduled TaskStarted TaskSubmitted TaskSucceeded TaskStateExited",
			"MapStateEntered MapStateStarted", "ChoiceStateEntered", "ExecutionSucceeded"} {
			if !strings.Contains(h, want) {
				t.Errorf("%s: history lacks %q: %s", key, want, h)
			}
		}
		if key == "in/bad.fail" && strings.Count(h, "TaskFailed") != 2 {
			t.Errorf("expected one retry (2 TaskFailed): %s", h)
		}
	}
	// the first task: command from the state, env + secret resolved, log stream written
	var task *Task
	for _, x := range list[Task](a.db, "task") {
		x := x
		if len(x.Containers[0].Command) > 0 && x.Containers[0].Command[0] == "import" {
			task = &x
			break
		}
	}
	if task == nil {
		t.Fatal("import task not found")
	}
	env := envMap(task.Containers[0].Env)
	if task.Containers[0].Command[1] != "--key=in/ok.csv" && task.Containers[0].Command[1] != "--key=in/bad.fail" {
		t.Errorf("command = %v", task.Containers[0].Command)
	}
	if env["MODE"] != "batch" || env["DB_PASS"] != "s3cret" || !strings.Contains(env["AWS_ENDPOINT_URL"], "4566") {
		t.Errorf("env = %v", env)
	}
	desc := c.call("AmazonEC2ContainerServiceV20141113.DescribeTasks", M{"cluster": "work", "tasks": []any{task.Arn}})
	td := getList(desc, "tasks")[0].(map[string]any)
	if td["lastStatus"] != "STOPPED" || td["stopCode"] != "EssentialContainerExited" {
		t.Errorf("task = %v %v", td["lastStatus"], td["stopCode"])
	}
	evs := c.call("Logs_20140328.GetLogEvents", M{"logGroupName": "/ecs/worker", "logStreamName": "w/app/" + task.ID, "startFromHead": true})
	if len(getList(evs, "events")) == 0 {
		t.Error("no log events in the task's stream")
	}
	// strictness that real AWS has
	req, _ := http.NewRequest("POST", c.url+"/", strings.NewReader(`{"cluster":"work","taskDefinition":"worker"}`))
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerServiceV20141113.RunTask")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("awsvpc RunTask without network configuration: %d", resp.StatusCode)
	}
}

func TestStopExecutionStopsTask(t *testing.T) {
	_, c := newTestApp(t)
	c.call("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition", M{"family": "slow",
		"containerDefinitions": []any{M{"name": "app", "image": "x", "environment": []any{M{"name": "LOCALAWS_NOOP_SLEEP", "value": "1h"}}}}})
	def := `{"StartAt":"Run","States":{"Run":{"Type":"Task","Resource":"arn:aws:states:::ecs:runTask.sync","Parameters":{"TaskDefinition":"slow"},"End":true}}}`
	sm := c.call("AWSStepFunctions.CreateStateMachine", M{"name": "slow", "definition": def, "roleArn": "r"})
	ex := c.call("AWSStepFunctions.StartExecution", M{"stateMachineArn": sm["stateMachineArn"], "name": "one"})
	time.Sleep(300 * time.Millisecond)
	c.call("AWSStepFunctions.StopExecution", M{"executionArn": ex["executionArn"], "error": "Stop", "cause": "test"})
	d := waitExec(t, c, getStr(ex, "executionArn"))
	if d["status"] != "ABORTED" {
		t.Fatalf("status = %v", d["status"])
	}
	tasks := getList(c.call("AmazonEC2ContainerServiceV20141113.ListTasks", M{"desiredStatus": "STOPPED"}), "taskArns")
	if len(tasks) != 1 {
		t.Fatalf("stopped tasks = %v", tasks)
	}
}

func TestConsoleAndPatternTester(t *testing.T) {
	_, c := newTestApp(t)
	r, body := c.raw("GET", "/_localaws/", nil, map[string]string{"Accept-Encoding": "gzip"})
	if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("console index: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	_ = body
	info := c.raw
	if r, b := info("GET", "/_localaws/api/info", nil, nil); r.StatusCode != 200 || !strings.Contains(string(b), `"account":"123456789012"`) {
		t.Fatalf("api/info: %d %s", r.StatusCode, b)
	}
	got := c.call("AWSEvents.TestEventPattern", M{"EventPattern": `{"source":["my.app"],"detail":{"n":[{"numeric":[">",1]}]}}`,
		"Event": `{"id":"1","source":"my.app","detail-type":"x","detail":{"n":5}}`})
	if got["Result"] != true {
		t.Fatalf("TestEventPattern = %v", got)
	}
}
