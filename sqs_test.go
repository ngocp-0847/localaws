package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSQSLifecycleAndRedrive(t *testing.T) {
	_, c := newTestApp(t)
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "jobs-dlq"})
	q := c.call("AmazonSQS.CreateQueue", M{"QueueName": "jobs", "Attributes": M{
		"VisibilityTimeout": "1",
		"RedrivePolicy":     `{"deadLetterTargetArn":"arn:aws:sqs:eu-west-1:123456789012:jobs-dlq","maxReceiveCount":2}`,
	}})
	if !strings.HasSuffix(str(q["QueueUrl"]), "/123456789012/jobs") {
		t.Fatalf("queue url %v", q["QueueUrl"])
	}
	// a client that builds its own URL against another host still reaches the queue
	own := "http://sqs.example:9324/123456789012/jobs"
	attrs := M{"trace": M{"DataType": "String", "StringValue": "t-1"}}
	sent := c.call("AmazonSQS.SendMessage", M{"QueueUrl": own, "MessageBody": `{"batch_type":"send-mail"}`, "MessageAttributes": attrs})
	if sent["MD5OfMessageBody"] != md5Hex([]byte(`{"batch_type":"send-mail"}`)) || sent["MD5OfMessageAttributes"] == nil {
		t.Fatalf("send %v", sent)
	}

	recv := func() []any {
		return getList(c.call("AmazonSQS.ReceiveMessage", M{"QueueUrl": own, "MaxNumberOfMessages": 10, "MessageAttributeNames": []string{"All"}}), "Messages")
	}
	msgs := recv()
	if len(msgs) != 1 {
		t.Fatalf("first receive %v", msgs)
	}
	m := msgs[0].(map[string]any)
	if getMap(m, "Attributes")["ApproximateReceiveCount"] != "1" || getMap(getMap(m, "MessageAttributes"), "trace")["StringValue"] != "t-1" {
		t.Fatalf("message %v", m)
	}
	if len(recv()) != 0 {
		t.Fatal("in-flight message delivered twice")
	}
	time.Sleep(1100 * time.Millisecond)
	if msgs = recv(); len(msgs) != 1 || getMap(msgs[0].(map[string]any), "Attributes")["ApproximateReceiveCount"] != "2" {
		t.Fatalf("second receive %v", msgs)
	}
	time.Sleep(1100 * time.Millisecond)
	if msgs = recv(); len(msgs) != 0 {
		t.Fatalf("message past maxReceiveCount delivered: %v", msgs)
	}
	dlq := c.call("AmazonSQS.GetQueueAttributes", M{"QueueUrl": "jobs-dlq", "AttributeNames": []string{"All"}})
	if getMap(dlq, "Attributes")["ApproximateNumberOfMessages"] != "1" {
		t.Fatalf("dlq %v", dlq)
	}

	// delete by receipt handle, long polling returns as soon as a message arrives
	c.call("AmazonSQS.SendMessage", M{"QueueUrl": own, "MessageBody": "x"})
	got := recv()[0].(map[string]any)
	c.call("AmazonSQS.DeleteMessage", M{"QueueUrl": own, "ReceiptHandle": got["ReceiptHandle"]})
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.call("AmazonSQS.SendMessage", M{"QueueUrl": own, "MessageBody": "late"})
	}()
	t0 := time.Now()
	lp := getList(c.call("AmazonSQS.ReceiveMessage", M{"QueueUrl": own, "WaitTimeSeconds": 5}), "Messages")
	if len(lp) != 1 || time.Since(t0) > 3*time.Second {
		t.Fatalf("long poll %v after %v", lp, time.Since(t0))
	}

	resp, body := c.raw("POST", "/", []byte(`{"QueueUrl":"nope"}`), map[string]string{"X-Amz-Target": "AmazonSQS.SendMessage", "Content-Type": "application/x-amz-json-1.0"})
	if resp.StatusCode != 400 || !strings.Contains(string(body), "QueueDoesNotExist") {
		t.Fatalf("missing queue: %d %s", resp.StatusCode, body)
	}
}

func TestEventBridgeToSQSAndSES(t *testing.T) {
	_, c := newTestApp(t)
	c.call("AmazonSQS.CreateQueue", M{"QueueName": "batch"})
	c.call("AWSEvents.PutRule", M{"Name": "kick", "EventPattern": `{"source":["fpaas.scheduler"]}`})
	c.call("AWSEvents.PutTargets", M{"Rule": "kick", "Targets": []M{{"Id": "q", "Arn": "arn:aws:sqs:eu-west-1:123456789012:batch", "Input": `{"batch_type":"send-reserve"}`}}})
	c.call("AWSEvents.PutEvents", M{"Entries": []M{{"Source": "fpaas.scheduler", "DetailType": "tick", "Detail": "{}"}}})
	var msgs []any
	for i := 0; i < 50 && len(msgs) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		msgs = getList(c.call("AmazonSQS.ReceiveMessage", M{"QueueUrl": "batch"}), "Messages")
	}
	if len(msgs) != 1 || msgs[0].(map[string]any)["Body"] != `{"batch_type":"send-reserve"}` {
		t.Fatalf("rule → queue: %v", msgs)
	}

	form := url.Values{"Action": {"SendEmail"}, "Source": {"noreply@example.jp"}, "Destination.ToAddresses.member.1": {"a@example.jp"},
		"Message.Subject.Data": {"件名"}, "Message.Body.Text.Data": {"本文"}}
	resp, body := c.raw("POST", "/", []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded",
		"Authorization": "AWS4-HMAC-SHA256 Credential=t/20260101/eu-west-1/ses/aws4_request, SignedHeaders=host, Signature=x"})
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<MessageId>") {
		t.Fatalf("SendEmail: %d %s", resp.StatusCode, body)
	}
	_, body = c.raw("GET", "/_localaws/api/ses/messages?to=a@example.jp", nil, nil)
	var box struct{ Messages []M }
	_ = json.Unmarshal(body, &box)
	if len(box.Messages) != 1 || box.Messages[0]["subject"] != "件名" || box.Messages[0]["text"] != "本文" {
		t.Fatalf("mailbox %s", body)
	}
}
