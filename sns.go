package main

// SNS (Query protocol): topics with their attributes, subscriptions, tags, Publish /
// PublishBatch. Every publish is recorded (GET /_localaws/api/sns/messages) and fanned
// out to the topic's subscriptions:
//   sqs   SendMessage to the queue — the SNS JSON envelope, or the bare message with
//         RawMessageDelivery=true (message attributes then become SQS attributes)
//   other protocols are confirmed at once and logged as not delivered
// FilterPolicy is honoured for both scopes (MessageAttributes, MessageBody) with the
// EventBridge pattern matcher — the syntax is the same.

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Topic struct {
	Name    string            `json:"name"`
	Created int64             `json:"created"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

type Subscription struct {
	Arn      string            `json:"arn"`
	TopicArn string            `json:"topicArn"`
	Protocol string            `json:"protocol"`
	Endpoint string            `json:"endpoint"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Created  int64             `json:"created"`
}

func (a *App) topicArn(name string) string { return a.arn("sns", name) }

func (a *App) topic(ref string) (Topic, *apiError) {
	var t Topic
	if !a.db.get("topic", arnTail(ref), &t) {
		return t, errf(404, "NotFound", "Topic does not exist")
	}
	return t, nil
}

func (a *App) ensureTopic(name string) {
	if !a.db.has("topic", name) {
		_ = a.db.put("topic", name, Topic{Name: name, Created: nowMs()})
	}
}

// entries reads a Query-protocol map: <prefix>.entry.N.key / .value
func entries(v url.Values, prefix string) map[string]string {
	out := map[string]string{}
	for i := 1; ; i++ {
		p := prefix + ".entry." + strconv.Itoa(i) + "."
		k, ok := v[p+"key"]
		if !ok {
			return out
		}
		out[k[0]] = v.Get(p + "value")
	}
}

// queryTags reads Tags.member.N.Key / .Value (SNS, IAM).
func queryTags(v url.Values, prefix string) map[string]string {
	out := map[string]string{}
	for i := 1; ; i++ {
		p := prefix + ".member." + strconv.Itoa(i) + "."
		k, ok := v[p+"Key"]
		if !ok {
			return out
		}
		out[k[0]] = v.Get(p + "Value")
	}
}

// snsMessageAttrs reads MessageAttributes.entry.N.Name / .Value.DataType / .StringValue / .BinaryValue
// into the JSON shape SQS and Step Functions use.
func snsMessageAttrs(v url.Values, prefix string) M {
	out := M{}
	for i := 1; ; i++ {
		p := prefix + ".entry." + strconv.Itoa(i) + "."
		n, ok := v[p+"Name"]
		if !ok {
			return out
		}
		a := M{"DataType": v.Get(p + "Value.DataType")}
		if s := v.Get(p + "Value.StringValue"); s != "" {
			a["StringValue"] = s
		}
		if b := v.Get(p + "Value.BinaryValue"); b != "" {
			a["BinaryValue"] = b
		}
		out[n[0]] = a
	}
}

func attrEntries(m map[string]string) string {
	var sb strings.Builder
	sb.WriteString("<Attributes>")
	for _, k := range sortedKeys(m) {
		sb.WriteString("<entry><key>" + xe(k) + "</key><value>" + xe(m[k]) + "</value></entry>")
	}
	sb.WriteString("</Attributes>")
	return sb.String()
}

func (a *App) topicAttrs(t Topic) map[string]string {
	arn := a.topicArn(t.Name)
	out := map[string]string{"TopicArn": arn, "Owner": a.cfg.Account, "DisplayName": "", "SubscriptionsPending": "0",
		"SubscriptionsDeleted": "0", "EffectiveDeliveryPolicy": `{"http":{"defaultHealthyRetryPolicy":{"minDelayTarget":20,"maxDelayTarget":20,"numRetries":3,"numMaxDelayRetries":0,"numNoDelayRetries":0,"numMinDelayRetries":0,"backoffFunction":"linear"},"disableSubscriptionOverrides":false,"defaultRequestPolicy":{"headerContentType":"text/plain; charset=UTF-8"}}}`,
		"Policy": jsonStr(M{"Version": "2008-10-17", "Id": "__default_policy_ID", "Statement": []any{M{"Sid": "__default_statement_ID", "Effect": "Allow",
			"Principal": M{"AWS": "*"}, "Action": []any{"SNS:GetTopicAttributes", "SNS:SetTopicAttributes", "SNS:AddPermission", "SNS:RemovePermission",
				"SNS:DeleteTopic", "SNS:Subscribe", "SNS:ListSubscriptionsByTopic", "SNS:Publish"}, "Resource": arn,
			"Condition": M{"StringEquals": M{"AWS:SourceOwner": a.cfg.Account}}}}})}
	if strings.HasSuffix(t.Name, ".fifo") {
		out["FifoTopic"], out["ContentBasedDeduplication"] = "true", "false"
	}
	for k, v := range t.Attrs {
		out[k] = v
	}
	n := 0
	for _, s := range list[Subscription](a.db, "subscription") {
		if s.TopicArn == arn {
			n++
		}
	}
	out["SubscriptionsConfirmed"] = strconv.Itoa(n)
	return out
}

func (a *App) subAttrs(s Subscription) map[string]string {
	out := map[string]string{"SubscriptionArn": s.Arn, "TopicArn": s.TopicArn, "Protocol": s.Protocol, "Endpoint": s.Endpoint,
		"Owner": a.cfg.Account, "PendingConfirmation": "false", "ConfirmationWasAuthenticated": "true", "RawMessageDelivery": "false"}
	for k, v := range s.Attrs {
		out[k] = v
	}
	if out["FilterPolicy"] != "" && out["FilterPolicyScope"] == "" {
		out["FilterPolicyScope"] = "MessageAttributes"
	}
	return out
}

func (a *App) snsQuery(v url.Values) (string, *apiError) {
	action := v.Get("Action")
	meta := "<ResponseMetadata><RequestId>" + uuid4() + "</RequestId></ResponseMetadata>"
	wrap := func(inner string) string {
		return "<" + action + "Response " + snsNS + "><" + action + "Result>" + inner + "</" + action + "Result>" + meta + "</" + action + "Response>"
	}
	switch action {
	case "CreateTopic":
		name := v.Get("Name")
		if name == "" {
			return "", errf(400, "InvalidParameter", "Invalid parameter: Name")
		}
		attrs := entries(v, "Attributes")
		var t Topic
		if !a.db.get("topic", name, &t) {
			t = Topic{Name: name, Created: nowMs(), Attrs: attrs}
			_ = a.db.put("topic", name, t)
		}
		a.tagResource(a.topicArn(name), queryTags(v, "Tags"))
		return wrap("<TopicArn>" + a.topicArn(name) + "</TopicArn>"), nil
	case "ListTopics":
		var sb strings.Builder
		for _, t := range list[Topic](a.db, "topic") {
			sb.WriteString("<member><TopicArn>" + a.topicArn(t.Name) + "</TopicArn></member>")
		}
		return wrap("<Topics>" + sb.String() + "</Topics>"), nil
	case "GetTopicAttributes":
		t, e := a.topic(v.Get("TopicArn"))
		if e != nil {
			return "", e
		}
		return wrap(attrEntries(a.topicAttrs(t))), nil
	case "SetTopicAttributes":
		t, e := a.topic(v.Get("TopicArn"))
		if e != nil {
			return "", e
		}
		if t.Attrs == nil {
			t.Attrs = map[string]string{}
		}
		t.Attrs[v.Get("AttributeName")] = v.Get("AttributeValue")
		_ = a.db.put("topic", t.Name, t)
		return wrap(""), nil
	case "DeleteTopic":
		arn := v.Get("TopicArn")
		for _, s := range list[Subscription](a.db, "subscription") {
			if s.TopicArn == arn {
				_ = a.db.del("subscription", s.Arn)
			}
		}
		_ = a.db.del("topic", arnTail(arn))
		a.untagAll(arn)
		return wrap(""), nil
	case "Subscribe":
		t, e := a.topic(v.Get("TopicArn"))
		if e != nil {
			return "", e
		}
		s := Subscription{TopicArn: a.topicArn(t.Name), Protocol: v.Get("Protocol"), Endpoint: v.Get("Endpoint"), Attrs: entries(v, "Attributes"), Created: nowMs()}
		for _, cur := range list[Subscription](a.db, "subscription") {
			if cur.TopicArn == s.TopicArn && cur.Protocol == s.Protocol && cur.Endpoint == s.Endpoint {
				return wrap("<SubscriptionArn>" + cur.Arn + "</SubscriptionArn>"), nil
			}
		}
		s.Arn = s.TopicArn + ":" + uuid4()
		_ = a.db.put("subscription", s.Arn, s)
		a.logf("sns subscription %s → %s %s", t.Name, s.Protocol, s.Endpoint)
		return wrap("<SubscriptionArn>" + s.Arn + "</SubscriptionArn>"), nil
	case "ConfirmSubscription":
		for _, s := range list[Subscription](a.db, "subscription") {
			if s.TopicArn == v.Get("TopicArn") {
				return wrap("<SubscriptionArn>" + s.Arn + "</SubscriptionArn>"), nil
			}
		}
		return "", errf(404, "NotFound", "Subscription does not exist")
	case "Unsubscribe":
		_ = a.db.del("subscription", v.Get("SubscriptionArn"))
		return wrap(""), nil
	case "GetSubscriptionAttributes", "SetSubscriptionAttributes":
		var s Subscription
		if !a.db.get("subscription", v.Get("SubscriptionArn"), &s) {
			return "", errf(404, "NotFound", "Subscription does not exist")
		}
		if action == "GetSubscriptionAttributes" {
			return wrap(attrEntries(a.subAttrs(s))), nil
		}
		if s.Attrs == nil {
			s.Attrs = map[string]string{}
		}
		if val := v.Get("AttributeValue"); val == "" {
			delete(s.Attrs, v.Get("AttributeName"))
		} else {
			s.Attrs[v.Get("AttributeName")] = val
		}
		_ = a.db.put("subscription", s.Arn, s)
		return wrap(""), nil
	case "ListSubscriptions", "ListSubscriptionsByTopic":
		var sb strings.Builder
		for _, s := range list[Subscription](a.db, "subscription") {
			if action == "ListSubscriptionsByTopic" && s.TopicArn != v.Get("TopicArn") {
				continue
			}
			sb.WriteString("<member><SubscriptionArn>" + xe(s.Arn) + "</SubscriptionArn><TopicArn>" + xe(s.TopicArn) + "</TopicArn><Protocol>" +
				xe(s.Protocol) + "</Protocol><Endpoint>" + xe(s.Endpoint) + "</Endpoint><Owner>" + a.cfg.Account + "</Owner></member>")
		}
		return wrap("<Subscriptions>" + sb.String() + "</Subscriptions>"), nil
	case "TagResource":
		a.tagResource(v.Get("ResourceArn"), queryTags(v, "Tags"))
		return wrap(""), nil
	case "UntagResource":
		a.untagResource(v.Get("ResourceArn"), members(v, "TagKeys"))
		return wrap(""), nil
	case "ListTagsForResource":
		var sb strings.Builder
		tags := a.tagsOf(v.Get("ResourceArn"))
		for _, k := range sortedKeys(tags) {
			sb.WriteString("<member><Key>" + xe(k) + "</Key><Value>" + xe(tags[k]) + "</Value></member>")
		}
		return wrap("<Tags>" + sb.String() + "</Tags>"), nil
	case "Publish":
		t, e := a.topic(v.Get("TopicArn"))
		if e != nil {
			return "", e
		}
		id := a.recordSNS(t.Name, v.Get("Subject"), v.Get("Message"), snsMessageAttrs(v, "MessageAttributes"), v.Get("MessageGroupId"), v.Get("MessageDeduplicationId"))
		return wrap("<MessageId>" + id + "</MessageId>"), nil
	case "PublishBatch":
		t, e := a.topic(v.Get("TopicArn"))
		if e != nil {
			return "", e
		}
		var sb strings.Builder
		for i := 1; ; i++ {
			p := "PublishBatchRequestEntries.member." + strconv.Itoa(i) + "."
			if _, ok := v[p+"Id"]; !ok {
				break
			}
			id := a.recordSNS(t.Name, v.Get(p+"Subject"), v.Get(p+"Message"), snsMessageAttrs(v, p+"MessageAttributes"), v.Get(p+"MessageGroupId"), v.Get(p+"MessageDeduplicationId"))
			sb.WriteString("<member><Id>" + xe(v.Get(p+"Id")) + "</Id><MessageId>" + id + "</MessageId></member>")
		}
		return wrap("<Successful>" + sb.String() + "</Successful><Failed/>"), nil
	}
	return "", errf(400, "InvalidAction", "localaws sns: action %s is not implemented", action)
}

// recordSNS records a publish and delivers it to the topic's subscriptions.
func (a *App) recordSNS(topic, subject, message string, attrs M, group, dedup string) string {
	id := uuid4()
	_ = a.db.exec(`INSERT INTO sns_messages(id, seq, ts_ms, topic, subject, message, attrs) VALUES(?,?,?,?,?,?,?)`,
		id, a.db.next("sns"), nowMs(), topic, subject, message, jsonStr(attrs))
	arn := a.topicArn(topic)
	delivered := 0
	for _, s := range list[Subscription](a.db, "subscription") {
		if s.TopicArn != arn || !a.snsFilter(s, message, attrs) {
			continue
		}
		if s.Protocol != "sqs" {
			a.logf("sns %s → %s %s: protocol not emulated — message not delivered", topic, s.Protocol, s.Endpoint)
			continue
		}
		q, e := a.queue(s.Endpoint)
		if e != nil {
			a.logf("sns %s → %s: %s", topic, s.Endpoint, e.Msg)
			continue
		}
		body, qattrs := message, attrs
		if s.Attrs["RawMessageDelivery"] != "true" {
			body, qattrs = a.snsEnvelope(arn, id, subject, message, attrs), nil
		}
		if dedup == "" && group != "" {
			dedup = id
		}
		if _, e := a.sendMessage(q, body, -1, qattrs, group, dedup); e != nil {
			a.logf("sns %s → %s: %s", topic, q.Name, e.Msg)
			continue
		}
		delivered++
	}
	a.logf("sns publish to %s (%d bytes, delivered to %d subscription(s))", topic, len(message), delivered)
	return id
}

func (a *App) snsEnvelope(topicArn, id, subject, message string, attrs M) string {
	env := M{"Type": "Notification", "MessageId": id, "TopicArn": topicArn, "Message": message,
		"Timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "SignatureVersion": "1",
		"Signature": base64.StdEncoding.EncodeToString([]byte("localaws")), "SigningCertURL": a.cfg.PublicURL + "/_localaws/sns.pem",
		"UnsubscribeURL": a.cfg.PublicURL + "/?Action=Unsubscribe&SubscriptionArn=" + topicArn}
	if subject != "" {
		env["Subject"] = subject
	}
	if len(attrs) > 0 {
		ma := M{}
		for k, v := range attrs {
			vm, _ := v.(map[string]any)
			typ, val := str(vm["DataType"]), str(vm["StringValue"])
			if strings.HasPrefix(typ, "Binary") {
				val = str(vm["BinaryValue"])
			}
			ma[k] = M{"Type": typ, "Value": val}
		}
		env["MessageAttributes"] = ma
	}
	return jsonStr(env)
}

// snsFilter applies a subscription's FilterPolicy.
func (a *App) snsFilter(s Subscription, message string, attrs M) bool {
	fp := s.Attrs["FilterPolicy"]
	if fp == "" {
		return true
	}
	var pat map[string]any
	if json.Unmarshal([]byte(fp), &pat) != nil {
		return false
	}
	if s.Attrs["FilterPolicyScope"] == "MessageBody" {
		var body map[string]any
		return json.Unmarshal([]byte(message), &body) == nil && matchPattern(pat, body)
	}
	doc := M{}
	for k, v := range attrs {
		vm, _ := v.(map[string]any)
		val := str(vm["StringValue"])
		switch typ := str(vm["DataType"]); {
		case strings.HasPrefix(typ, "Number"):
			if n, err := strconv.ParseFloat(val, 64); err == nil {
				doc[k] = n
				continue
			}
		case strings.HasPrefix(typ, "String.Array"):
			var arr []any
			if json.Unmarshal([]byte(val), &arr) == nil {
				doc[k] = arr
				continue
			}
		}
		doc[k] = val
	}
	return matchPattern(pat, doc)
}
