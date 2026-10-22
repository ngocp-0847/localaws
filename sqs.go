package main

// SQS (AWS JSON 1.0, X-Amz-Target AmazonSQS.*): queues, send / receive / delete with
// visibility timeouts, long polling, delays, ApproximateReceiveCount and redrive to a
// dead-letter queue after maxReceiveCount — what a consumer such as sqs-consumer depends
// on. Queue URLs are <publicUrl>/<account>/<name>; any URL whose last segment names a
// queue is accepted, so a client that builds its own URLs (SQS_BASE_URL + name) works.
// EventBridge rules and Step Functions (sqs:sendMessage) can target queues too.
//
// Messages live in the sqs_messages table. Body and attribute MD5s are the real ones —
// the SDKs verify them.

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Queue struct {
	Name    string            `json:"name"`
	Attrs   map[string]string `json:"attrs"`
	Tags    map[string]string `json:"tags,omitempty"`
	Created int64             `json:"created"`
	Updated int64             `json:"updated"`
}

type QueueCfg struct {
	Name              string            `json:"name"`
	Attributes        map[string]string `json:"attributes"`
	DeadLetter        string            `json:"deadLetterQueue"` // name of the DLQ (created if missing)
	MaxReceiveCount   int               `json:"maxReceiveCount"` // with deadLetterQueue; default 3
	VisibilityTimeout int               `json:"visibilityTimeout"`
}

const sqsSchema = `
CREATE TABLE IF NOT EXISTS sqs_messages(
  id TEXT PRIMARY KEY, queue TEXT, seq INTEGER, body TEXT, md5 TEXT, attrs TEXT, md5_attrs TEXT,
  sent_ms INTEGER, visible_ms INTEGER, receive_count INTEGER DEFAULT 0, first_receive_ms INTEGER DEFAULT 0,
  receipt TEXT DEFAULT '', group_id TEXT DEFAULT '', dedup_id TEXT DEFAULT '');
CREATE INDEX IF NOT EXISTS sqs_messages_q ON sqs_messages(queue, visible_ms, seq);
CREATE INDEX IF NOT EXISTS sqs_messages_r ON sqs_messages(receipt);
`

var sqsDefaults = map[string]string{
	"VisibilityTimeout":             "30",
	"DelaySeconds":                  "0",
	"MaximumMessageSize":            "262144",
	"MessageRetentionPeriod":        "345600",
	"ReceiveMessageWaitTimeSeconds": "0",
}

func (a *App) queueURL(name string) string {
	return strings.TrimRight(a.cfg.PublicURL, "/") + "/" + a.cfg.Account + "/" + name
}

// queueName takes a queue URL (any host) or an ARN or a bare name.
func queueName(s string) string {
	s = strings.TrimRight(s, "/")
	if strings.HasPrefix(s, "arn:") {
		return arnTail(s)
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (a *App) queue(urlOrName string) (Queue, *apiError) {
	var q Queue
	if !a.db.get("queue", queueName(urlOrName), &q) {
		return q, errf(400, "QueueDoesNotExist", "The specified queue does not exist.")
	}
	return q, nil
}

func (a *App) createQueue(name string, attrs, tags map[string]string) (Queue, *apiError) {
	if name == "" || len(name) > 80 {
		return Queue{}, errf(400, "InvalidParameterValue", "Queue name must be 1-80 characters")
	}
	var cur Queue
	if a.db.get("queue", name, &cur) {
		for k, v := range attrs {
			if cv, ok := cur.Attrs[k]; ok && cv != v && k != "Policy" {
				return cur, errf(400, "QueueAlreadyExists", "A queue already exists with the same name and a different value for attribute %s", k)
			}
		}
		return cur, nil
	}
	q := Queue{Name: name, Attrs: map[string]string{}, Tags: tags, Created: nowMs(), Updated: nowMs()}
	for k, v := range sqsDefaults {
		q.Attrs[k] = v
	}
	if strings.HasSuffix(name, ".fifo") {
		q.Attrs["FifoQueue"] = "true"
	}
	for k, v := range attrs {
		q.Attrs[k] = v
	}
	return q, nilErr(a.db.put("queue", name, q))
}

func nilErr(err error) *apiError {
	if err != nil {
		return errf(500, "InternalError", "%v", err)
	}
	return nil
}

// ── message attribute MD5 (the algorithm the SDKs verify) ───────────────
func md5Hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func md5Attrs(attrs M) string {
	if len(attrs) == 0 {
		return ""
	}
	names := make([]string, 0, len(attrs))
	for k := range attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var buf []byte
	put := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		buf = append(buf, n[:]...)
		buf = append(buf, b...)
	}
	for _, k := range names {
		v, _ := attrs[k].(map[string]any)
		dt := str(v["DataType"])
		put([]byte(k))
		put([]byte(dt))
		if strings.HasPrefix(dt, "Binary") {
			buf = append(buf, 2)
			raw, _ := base64.StdEncoding.DecodeString(str(v["BinaryValue"]))
			put(raw)
		} else {
			buf = append(buf, 1)
			put([]byte(str(v["StringValue"])))
		}
	}
	return md5Hex(buf)
}

// ── send / receive ───────────────────────────────────────────────────────
func (a *App) sendMessage(q Queue, body string, delay int, attrs M, group, dedup string) (M, *apiError) {
	if body == "" {
		return nil, errf(400, "MissingParameter", "The request must contain the parameter MessageBody.")
	}
	if max := atoi(q.Attrs["MaximumMessageSize"]); max > 0 && len(body) > max {
		return nil, errf(400, "InvalidParameterValue", "One or more parameters are invalid. Reason: Message must be shorter than %d bytes.", max)
	}
	if delay < 0 {
		delay = atoi(q.Attrs["DelaySeconds"])
	}
	if q.Attrs["FifoQueue"] == "true" && dedup != "" {
		var n int
		_ = a.db.db.QueryRow(`SELECT count(*) FROM sqs_messages WHERE queue=? AND dedup_id=? AND sent_ms > ?`,
			q.Name, dedup, nowMs()-5*60*1000).Scan(&n)
		if n > 0 {
			return M{"MessageId": uuid4(), "MD5OfMessageBody": md5Hex([]byte(body))}, nil
		}
	}
	id, now := uuid4(), nowMs()
	m5, m5a := md5Hex([]byte(body)), md5Attrs(attrs)
	err := a.db.exec(`INSERT INTO sqs_messages(id, queue, seq, body, md5, attrs, md5_attrs, sent_ms, visible_ms, group_id, dedup_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, q.Name, a.db.next("sqs"), body, m5, jsonStr(attrs), m5a, now, now+int64(delay)*1000, group, dedup)
	if err != nil {
		return nil, nilErr(err)
	}
	out := M{"MessageId": id, "MD5OfMessageBody": m5}
	if m5a != "" {
		out["MD5OfMessageAttributes"] = m5a
	}
	if group != "" {
		out["SequenceNumber"] = strconv.FormatInt(now, 10) + "0000000"
	}
	return out, nil
}

type sqsMsg struct {
	id, body, md5, attrs, md5a, group, dedup string
	sent, firstRecv                          int64
	count                                    int
}

// redrive returns the DLQ name and max receive count of a queue's RedrivePolicy.
func redrive(q Queue) (string, int) {
	var p M
	if jsonUnmarshal(q.Attrs["RedrivePolicy"], &p) != nil || p == nil {
		return "", 0
	}
	return queueName(str(p["deadLetterTargetArn"])), getInt(p, "maxReceiveCount", 0)
}

func (a *App) receive(q Queue, max, visibility int) []M {
	dlq, maxRecv := redrive(q)
	now := nowMs()
	a.db.mu.Lock()
	defer a.db.mu.Unlock()
	rows, err := a.db.db.Query(`SELECT id, body, md5, attrs, md5_attrs, sent_ms, receive_count, first_receive_ms, group_id, dedup_id
		FROM sqs_messages WHERE queue=? AND visible_ms <= ? ORDER BY seq LIMIT ?`, q.Name, now, max*4+10)
	if err != nil {
		return nil
	}
	var cand []sqsMsg
	for rows.Next() {
		var m sqsMsg
		_ = rows.Scan(&m.id, &m.body, &m.md5, &m.attrs, &m.md5a, &m.sent, &m.count, &m.firstRecv, &m.group, &m.dedup)
		cand = append(cand, m)
	}
	rows.Close()
	var out []M
	lockedGroups := map[string]bool{}
	if q.Attrs["FifoQueue"] == "true" {
		// a FIFO group with an in-flight message delivers nothing else
		gr, err := a.db.db.Query(`SELECT DISTINCT group_id FROM sqs_messages WHERE queue=? AND visible_ms > ? AND receive_count > 0`, q.Name, now)
		if err == nil {
			for gr.Next() {
				var g string
				_ = gr.Scan(&g)
				lockedGroups[g] = true
			}
			gr.Close()
		}
	}
	for _, m := range cand {
		if len(out) >= max {
			break
		}
		if lockedGroups[m.group] {
			continue
		}
		if dlq != "" && maxRecv > 0 && m.count >= maxRecv {
			// moved, keeping its id and body, as AWS does; its receive count carries over
			_, _ = a.db.db.Exec(`UPDATE sqs_messages SET queue=?, visible_ms=?, receipt='' WHERE id=?`, dlq, now, m.id)
			continue
		}
		receipt := base64.RawURLEncoding.EncodeToString([]byte(m.id + "#" + randHex(12)))
		first := m.firstRecv
		if first == 0 {
			first = now
		}
		m.count++
		if _, err := a.db.db.Exec(`UPDATE sqs_messages SET visible_ms=?, receive_count=?, first_receive_ms=?, receipt=? WHERE id=?`,
			now+int64(visibility)*1000, m.count, first, receipt, m.id); err != nil {
			continue
		}
		if m.group != "" {
			lockedGroups[m.group] = true
		}
		sys := M{
			"ApproximateReceiveCount":          strconv.Itoa(m.count),
			"SentTimestamp":                    strconv.FormatInt(m.sent, 10),
			"ApproximateFirstReceiveTimestamp": strconv.FormatInt(first, 10),
			"SenderId":                         "AIDALOCALAWS",
		}
		if m.group != "" {
			sys["MessageGroupId"] = m.group
			sys["MessageDeduplicationId"] = m.dedup
		}
		msg := M{"MessageId": m.id, "ReceiptHandle": receipt, "MD5OfBody": m.md5, "Body": m.body, "Attributes": sys}
		var attrs M
		_ = jsonUnmarshal(m.attrs, &attrs)
		if len(attrs) > 0 {
			msg["MessageAttributes"] = attrs
			msg["MD5OfMessageAttributes"] = m.md5a
		}
		out = append(out, msg)
	}
	return out
}

func (a *App) messageByReceipt(q Queue, receipt string) (string, bool) {
	var id string
	err := a.db.db.QueryRow(`SELECT id FROM sqs_messages WHERE queue=? AND receipt=?`, q.Name, receipt).Scan(&id)
	return id, err == nil
}

func (a *App) sqsAPI(r *http.Request, op string, in M) (any, *apiError) {
	switch op {
	case "CreateQueue":
		q, e := a.createQueue(getStr(in, "QueueName"), mapStr(getMap(in, "Attributes")), mapStr(getMap(in, "tags")))
		if e != nil {
			return nil, e
		}
		return M{"QueueUrl": a.queueURL(q.Name)}, nil
	case "GetQueueUrl":
		q, e := a.queue(getStr(in, "QueueName"))
		if e != nil {
			return nil, e
		}
		return M{"QueueUrl": a.queueURL(q.Name)}, nil
	case "ListQueues":
		prefix := getStr(in, "QueueNamePrefix")
		urls := []string{}
		for _, q := range list[Queue](a.db, "queue") {
			if strings.HasPrefix(q.Name, prefix) {
				urls = append(urls, a.queueURL(q.Name))
			}
		}
		page, next := pageOf(urls, getStr(in, "NextToken"), getInt(in, "MaxResults", 1000))
		out := M{"QueueUrls": page}
		if next != "" {
			out["NextToken"] = next
		}
		return out, nil
	case "DeleteQueue":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		_ = a.db.exec(`DELETE FROM sqs_messages WHERE queue=?`, q.Name)
		return M{}, nilErr(a.db.del("queue", q.Name))
	case "PurgeQueue":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		return M{}, nilErr(a.db.exec(`DELETE FROM sqs_messages WHERE queue=?`, q.Name))
	case "GetQueueAttributes":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		want := strList(getList(in, "AttributeNames"))
		all := a.queueAttrs(q)
		out := map[string]string{}
		for k, v := range all {
			if len(want) == 0 || contains(want, "All") || contains(want, k) {
				out[k] = v
			}
		}
		return M{"Attributes": out}, nil
	case "SetQueueAttributes":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		for k, v := range mapStr(getMap(in, "Attributes")) {
			if v == "" && (k == "Policy" || k == "RedrivePolicy" || k == "RedriveAllowPolicy") {
				delete(q.Attrs, k) // an empty policy removes it
				continue
			}
			q.Attrs[k] = v
		}
		q.Updated = nowMs()
		return M{}, nilErr(a.db.put("queue", q.Name, q))
	case "TagQueue", "UntagQueue", "ListQueueTags":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		if q.Tags == nil {
			q.Tags = map[string]string{}
		}
		switch op {
		case "TagQueue":
			for k, v := range mapStr(getMap(in, "Tags")) {
				q.Tags[k] = v
			}
		case "UntagQueue":
			for _, k := range strList(getList(in, "TagKeys")) {
				delete(q.Tags, k)
			}
		default:
			return M{"Tags": q.Tags}, nil
		}
		return M{}, nilErr(a.db.put("queue", q.Name, q))
	case "SendMessage":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		return a.sendMessage(q, getStr(in, "MessageBody"), getInt(in, "DelaySeconds", -1), getMap(in, "MessageAttributes"),
			getStr(in, "MessageGroupId"), getStr(in, "MessageDeduplicationId"))
	case "SendMessageBatch":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		ok, failed := []M{}, []M{}
		for _, x := range getList(in, "Entries") {
			en, _ := x.(map[string]any)
			res, e := a.sendMessage(q, getStr(en, "MessageBody"), getInt(en, "DelaySeconds", -1), getMap(en, "MessageAttributes"),
				getStr(en, "MessageGroupId"), getStr(en, "MessageDeduplicationId"))
			if e != nil {
				failed = append(failed, M{"Id": getStr(en, "Id"), "Code": e.Type, "Message": e.Msg, "SenderFault": true})
				continue
			}
			res["Id"] = getStr(en, "Id")
			ok = append(ok, res)
		}
		return M{"Successful": ok, "Failed": failed}, nil
	case "ReceiveMessage":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		max := getInt(in, "MaxNumberOfMessages", 1)
		if max < 1 || max > 10 {
			return nil, errf(400, "InvalidParameterValue", "Value %d for parameter MaxNumberOfMessages is invalid. Reason: Must be between 1 and 10, if provided.", max)
		}
		vis := getInt(in, "VisibilityTimeout", atoi(q.Attrs["VisibilityTimeout"]))
		wait := getInt(in, "WaitTimeSeconds", atoi(q.Attrs["ReceiveMessageWaitTimeSeconds"]))
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		for {
			if msgs := a.receive(q, max, vis); len(msgs) > 0 {
				return M{"Messages": msgs}, nil
			}
			if !time.Now().Before(deadline) {
				return M{}, nil
			}
			select {
			case <-r.Context().Done():
				return M{}, nil
			case <-a.stop:
				return M{}, nil
			case <-time.After(100 * time.Millisecond):
			}
			if q, e = a.queue(q.Name); e != nil {
				return nil, e
			}
		}
	case "DeleteMessage":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		id, ok := a.messageByReceipt(q, getStr(in, "ReceiptHandle"))
		if !ok {
			// AWS accepts a stale handle of a message already deleted; a garbage one is an error
			if getStr(in, "ReceiptHandle") == "" {
				return nil, errf(400, "ReceiptHandleIsInvalid", "The input receipt handle is invalid.")
			}
			return M{}, nil
		}
		return M{}, nilErr(a.db.exec(`DELETE FROM sqs_messages WHERE id=?`, id))
	case "DeleteMessageBatch":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		ok := []M{}
		for _, x := range getList(in, "Entries") {
			en, _ := x.(map[string]any)
			if id, found := a.messageByReceipt(q, getStr(en, "ReceiptHandle")); found {
				_ = a.db.exec(`DELETE FROM sqs_messages WHERE id=?`, id)
			}
			ok = append(ok, M{"Id": getStr(en, "Id")})
		}
		return M{"Successful": ok, "Failed": []M{}}, nil
	case "ChangeMessageVisibility":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		id, ok := a.messageByReceipt(q, getStr(in, "ReceiptHandle"))
		if !ok {
			return nil, errf(400, "MessageNotInflight", "Message does not exist or is not available for visibility timeout change.")
		}
		return M{}, nilErr(a.db.exec(`UPDATE sqs_messages SET visible_ms=? WHERE id=?`, nowMs()+int64(getInt(in, "VisibilityTimeout", 0))*1000, id))
	case "ChangeMessageVisibilityBatch":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		ok, failed := []M{}, []M{}
		for _, x := range getList(in, "Entries") {
			en, _ := x.(map[string]any)
			id, found := a.messageByReceipt(q, getStr(en, "ReceiptHandle"))
			if !found {
				failed = append(failed, M{"Id": getStr(en, "Id"), "Code": "MessageNotInflight", "SenderFault": true})
				continue
			}
			_ = a.db.exec(`UPDATE sqs_messages SET visible_ms=? WHERE id=?`, nowMs()+int64(getInt(en, "VisibilityTimeout", 0))*1000, id)
			ok = append(ok, M{"Id": getStr(en, "Id")})
		}
		return M{"Successful": ok, "Failed": failed}, nil
	case "ListDeadLetterSourceQueues":
		q, e := a.queue(getStr(in, "QueueUrl"))
		if e != nil {
			return nil, e
		}
		urls := []string{}
		for _, s := range list[Queue](a.db, "queue") {
			if d, _ := redrive(s); d == q.Name {
				urls = append(urls, a.queueURL(s.Name))
			}
		}
		return M{"queueUrls": urls}, nil
	}
	return nil, errf(400, "UnsupportedOperation", "localaws sqs: %s is not implemented", op)
}

func (a *App) queueAttrs(q Queue) map[string]string {
	out := map[string]string{}
	for k, v := range q.Attrs {
		out[k] = v
	}
	now := nowMs()
	var visible, inflight, delayed int
	_ = a.db.db.QueryRow(`SELECT
		coalesce(sum(CASE WHEN visible_ms <= ? THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN visible_ms > ? AND receive_count > 0 THEN 1 ELSE 0 END),0),
		coalesce(sum(CASE WHEN visible_ms > ? AND receive_count = 0 THEN 1 ELSE 0 END),0)
		FROM sqs_messages WHERE queue=?`, now, now, now, q.Name).Scan(&visible, &inflight, &delayed)
	out["ApproximateNumberOfMessages"] = strconv.Itoa(visible)
	out["ApproximateNumberOfMessagesNotVisible"] = strconv.Itoa(inflight)
	out["ApproximateNumberOfMessagesDelayed"] = strconv.Itoa(delayed)
	out["QueueArn"] = a.arn("sqs", q.Name)
	out["CreatedTimestamp"] = strconv.FormatInt(q.Created/1000, 10)
	out["LastModifiedTimestamp"] = strconv.FormatInt(q.Updated/1000, 10)
	return out
}

// peekMessages lists a queue's messages without receiving them (console / test tooling).
func (a *App) peekMessages(name string, limit int) []M {
	rows, err := a.db.db.Query(`SELECT id, body, attrs, sent_ms, visible_ms, receive_count FROM sqs_messages WHERE queue=? ORDER BY seq DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []M{}
	now := nowMs()
	for rows.Next() {
		var id, body, attrs string
		var sent, vis int64
		var cnt int
		_ = rows.Scan(&id, &body, &attrs, &sent, &vis, &cnt)
		var am M
		_ = jsonUnmarshal(attrs, &am)
		state := "available"
		if vis > now {
			state = map[bool]string{true: "inflight", false: "delayed"}[cnt > 0]
		}
		out = append(out, M{"messageId": id, "body": body, "attributes": am, "sent": sent, "receiveCount": cnt, "state": state})
	}
	return out
}

func (a *App) seedQueues(qs []QueueCfg) error {
	for _, c := range qs {
		attrs := map[string]string{}
		for k, v := range c.Attributes {
			attrs[k] = v
		}
		if c.VisibilityTimeout > 0 {
			attrs["VisibilityTimeout"] = strconv.Itoa(c.VisibilityTimeout)
		}
		if c.DeadLetter != "" {
			if _, e := a.createQueue(c.DeadLetter, nil, nil); e != nil && e.Type != "QueueAlreadyExists" {
				return e
			}
			mr := c.MaxReceiveCount
			if mr <= 0 {
				mr = 3
			}
			attrs["RedrivePolicy"] = jsonStr(M{"deadLetterTargetArn": a.arn("sqs", c.DeadLetter), "maxReceiveCount": mr})
		}
		var cur Queue
		if a.db.get("queue", c.Name, &cur) {
			for k, v := range attrs {
				cur.Attrs[k] = v
			}
			cur.Updated = nowMs()
			if err := a.db.put("queue", c.Name, cur); err != nil {
				return err
			}
			continue
		}
		if _, e := a.createQueue(c.Name, attrs, nil); e != nil {
			return e
		}
	}
	return nil
}

// sqsQueryCode: the error code the Query protocol used (x-amzn-query-error), which the
// SDKs report as the error code and Terraform matches on (AWS.SimpleQueueService.NonExistentQueue
// is how it learns a queue is gone).
func sqsQueryCode(typ string) string {
	switch typ {
	case "QueueDoesNotExist":
		return "AWS.SimpleQueueService.NonExistentQueue"
	case "QueueDeletedRecently", "MessageNotInflight", "PurgeQueueInProgress", "UnsupportedOperation",
		"BatchEntryIdsNotDistinct", "TooManyEntriesInBatchRequest", "EmptyBatchRequest", "InvalidBatchEntryId":
		return "AWS.SimpleQueueService." + typ
	}
	return typ
}

func mapStr(m M) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = str(v)
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
