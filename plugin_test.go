package main

import (
	"encoding/json"
	"testing"
)

func TestPluginProtocol(t *testing.T) {
	_, c := newTestApp(t)
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "jobs-dlq"})
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "jobs", "Attributes": M{"RedrivePolicy": `{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:123456789012:jobs-dlq","maxReceiveCount":3}`}})
	c.call("AmazonSQS.SendMessage", M{"QueueUrl": "jobs", "MessageBody": "a"})
	c.call("AmazonSQS.SendMessage", M{"QueueUrl": "jobs-dlq", "MessageBody": "dead"})

	resp, body := c.raw("GET", "/_localaws/plugin", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("manifest %d %s", resp.StatusCode, body)
	}
	var m struct {
		Protocol, Name, Status string
		UI                     struct{ Path string }
		Summary                []struct {
			Key   string
			Value int
			Tone  string
		}
		Sections []struct{ ID string }
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Protocol != pluginProtocol || m.Name != "localaws" || m.UI.Path != "/_localaws/" || len(m.Sections) == 0 {
		t.Fatalf("manifest %s", body)
	}
	vals := map[string]int{}
	for _, s := range m.Summary {
		vals[s.Key] = s.Value
		if s.Key == "dead" && s.Tone != "bad" {
			t.Errorf("dead-lettered messages should be flagged: %+v", s)
		}
	}
	if vals["queues"] != 2 || vals["waiting"] != 1 || vals["dead"] != 1 || m.Status != "degraded" {
		t.Errorf("summary %v status %s", vals, m.Status)
	}

	_, body = c.raw("GET", "/_localaws/plugin/sections/queues", nil, nil)
	var tbl struct {
		Columns []struct{ Key, Type string }
		Rows    []M
	}
	_ = json.Unmarshal(body, &tbl)
	if len(tbl.Columns) == 0 || len(tbl.Rows) != 2 {
		t.Fatalf("queues section %s", body)
	}
	if resp, _ := c.raw("GET", "/_localaws/plugin/sections/nope", nil, nil); resp.StatusCode != 404 {
		t.Errorf("unknown section: %d", resp.StatusCode)
	}
}
