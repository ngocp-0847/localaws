package main

// SES — mail is captured, never delivered. SES v1 (Query protocol: SendEmail,
// SendRawEmail, SendTemplatedEmail, identities, quota) and SES v2 (REST JSON:
// POST /v2/email/outbound-emails) land in the ses_messages table, where the console and
// test tooling read them back:
//
//	GET    /_localaws/api/ses/messages?to=&since=&limit=   newest first
//	DELETE /_localaws/api/ses/messages                     clear the mailbox
//
// SNS Publish (API and Step Functions) is recorded the same way in sns_messages:
//
//	GET    /_localaws/api/sns/messages?topic=&since=&limit=
//	DELETE /_localaws/api/sns/messages

import (
	"bufio"
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
)

const sesSchema = `
CREATE TABLE IF NOT EXISTS ses_messages(id TEXT PRIMARY KEY, seq INTEGER, ts_ms INTEGER, api TEXT, source TEXT,
  to_addrs TEXT, cc_addrs TEXT, bcc_addrs TEXT, subject TEXT, text_body TEXT, html_body TEXT, raw TEXT, template TEXT, template_data TEXT);
CREATE TABLE IF NOT EXISTS sns_messages(id TEXT PRIMARY KEY, seq INTEGER, ts_ms INTEGER, topic TEXT, subject TEXT, message TEXT, attrs TEXT);
`

const sesNS = `xmlns="http://ses.amazonaws.com/doc/2010-12-01/"`

// SESIdentity: a verified email address or domain. Every identity is verified at once.
type SESIdentity struct {
	Name, Type, Token   string
	DkimTokens          []string
	DkimEnabled         bool
	Topics              map[string]string // Bounce | Complaint | Delivery → topic ARN
	Headers             map[string]bool
	NoForwarding        bool
	MailFrom, MXFailure string
	ConfigSet           string
	Created             int64
}

type SESTemplate struct {
	Name, Subject, Text, HTML string
	Created                   int64
}

func (a *App) sesIdentity(name string) SESIdentity {
	var id SESIdentity
	if a.db.get("sesidentity", name, &id) {
		return id
	}
	id = SESIdentity{Name: name, Type: "EmailAddress", Created: nowMs()}
	if !strings.Contains(name, "@") {
		id.Type, id.Token = "Domain", base64.StdEncoding.EncodeToString([]byte(randHex(16)))
		id.DkimTokens = []string{randHex(16), randHex(16), randHex(16)}
	}
	_ = a.db.put("sesidentity", name, id)
	a.logf("ses identity %s verified", name)
	return id
}

type Mail struct {
	ID, API, Source, Subject, Text, HTML, Raw, Template, TemplateData string
	To, Cc, Bcc                                                       []string
}

func (a *App) captureMail(m Mail) string {
	if m.ID == "" {
		m.ID = "0100" + randHex(12) + "-" + uuid4() + "-000000"
	}
	_ = a.db.exec(`INSERT INTO ses_messages(id, seq, ts_ms, api, source, to_addrs, cc_addrs, bcc_addrs, subject, text_body, html_body, raw, template, template_data)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, a.db.next("ses"), nowMs(), m.API, m.Source, jsonStr(nz(m.To)), jsonStr(nz(m.Cc)), jsonStr(nz(m.Bcc)),
		m.Subject, m.Text, m.HTML, m.Raw, m.Template, m.TemplateData)
	a.logf("ses %s from %s to %s (captured, %d bytes)", m.API, m.Source, strings.Join(m.To, ","), len(m.Text)+len(m.HTML)+len(m.Raw))
	return m.ID
}

func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// members collects Query-protocol lists: Destination.ToAddresses.member.1, .2, …
func members(v url.Values, prefix string) []string {
	var out []string
	for i := 1; ; i++ {
		s, ok := v[prefix+".member."+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out = append(out, s[0])
	}
}

func (a *App) sesQuery(v url.Values) (string, *apiError) {
	action := v.Get("Action")
	meta := "<ResponseMetadata><RequestId>" + uuid4() + "</RequestId></ResponseMetadata>"
	wrap := func(inner string) string {
		return "<" + action + "Response " + sesNS + "><" + action + "Result>" + inner + "</" + action + "Result>" + meta + "</" + action + "Response>"
	}
	switch action {
	case "SendEmail":
		to := members(v, "Destination.ToAddresses")
		if v.Get("Source") == "" || len(to)+len(members(v, "Destination.CcAddresses"))+len(members(v, "Destination.BccAddresses")) == 0 {
			return "", errf(400, "ValidationError", "Source and at least one destination are required")
		}
		id := a.captureMail(Mail{API: "SendEmail", Source: v.Get("Source"), To: to, Cc: members(v, "Destination.CcAddresses"),
			Bcc: members(v, "Destination.BccAddresses"), Subject: v.Get("Message.Subject.Data"),
			Text: v.Get("Message.Body.Text.Data"), HTML: v.Get("Message.Body.Html.Data")})
		return wrap("<MessageId>" + id + "</MessageId>"), nil
	case "SendTemplatedEmail":
		id := a.captureMail(Mail{API: "SendTemplatedEmail", Source: v.Get("Source"), To: members(v, "Destination.ToAddresses"),
			Cc: members(v, "Destination.CcAddresses"), Bcc: members(v, "Destination.BccAddresses"),
			Template: v.Get("Template"), TemplateData: v.Get("TemplateData")})
		return wrap("<MessageId>" + id + "</MessageId>"), nil
	case "SendRawEmail":
		raw, err := base64.StdEncoding.DecodeString(v.Get("RawMessage.Data"))
		if err != nil {
			raw = []byte(v.Get("RawMessage.Data"))
		}
		m := parseRaw(string(raw))
		if src := v.Get("Source"); src != "" {
			m.Source = src
		}
		if d := members(v, "Destinations"); len(d) > 0 {
			m.To = d
		}
		id := a.captureMail(m)
		return wrap("<MessageId>" + id + "</MessageId>"), nil
	case "VerifyEmailIdentity", "VerifyEmailAddress":
		a.sesIdentity(v.Get("EmailAddress"))
		return wrap(""), nil
	case "VerifyDomainIdentity":
		id := a.sesIdentity(v.Get("Domain"))
		return wrap("<VerificationToken>" + id.Token + "</VerificationToken>"), nil
	case "VerifyDomainDkim":
		id := a.sesIdentity(v.Get("Domain"))
		id.DkimEnabled = true
		_ = a.db.put("sesidentity", id.Name, id)
		return wrap("<DkimTokens>" + membersXML(id.DkimTokens) + "</DkimTokens>"), nil
	case "ListIdentities", "ListVerifiedEmailAddresses":
		var names []string
		for _, id := range list[SESIdentity](a.db, "sesidentity") {
			if t := v.Get("IdentityType"); (t == "" || t == id.Type) && (action == "ListIdentities" || id.Type == "EmailAddress") {
				names = append(names, id.Name)
			}
		}
		if action == "ListVerifiedEmailAddresses" {
			return wrap("<VerifiedEmailAddresses>" + membersXML(names) + "</VerifiedEmailAddresses>"), nil
		}
		return wrap("<Identities>" + membersXML(names) + "</Identities>"), nil
	case "DeleteIdentity", "DeleteVerifiedEmailAddress":
		_ = a.db.del("sesidentity", v.Get("Identity")+v.Get("EmailAddress"))
		return wrap(""), nil
	case "GetIdentityVerificationAttributes", "GetIdentityDkimAttributes", "GetIdentityNotificationAttributes", "GetIdentityMailFromDomainAttributes":
		var sb strings.Builder
		for _, n := range members(v, "Identities") {
			var id SESIdentity
			if !a.db.get("sesidentity", n, &id) {
				continue // unknown identities are left out, as SES does
			}
			sb.WriteString("<entry><key>" + xe(n) + "</key><value>")
			switch action {
			case "GetIdentityVerificationAttributes":
				sb.WriteString("<VerificationStatus>Success</VerificationStatus>")
				if id.Token != "" {
					sb.WriteString("<VerificationToken>" + id.Token + "</VerificationToken>")
				}
			case "GetIdentityDkimAttributes":
				st := "NotStarted"
				if id.DkimEnabled {
					st = "Success"
				}
				sb.WriteString("<DkimEnabled>" + boolS(id.DkimEnabled) + "</DkimEnabled><DkimVerificationStatus>" + st + "</DkimVerificationStatus>")
				if id.Type == "Domain" {
					sb.WriteString("<DkimTokens>" + membersXML(id.DkimTokens) + "</DkimTokens>")
				}
			case "GetIdentityNotificationAttributes":
				for _, t := range []string{"Bounce", "Complaint", "Delivery"} {
					if arn := id.Topics[t]; arn != "" {
						sb.WriteString("<" + t + "Topic>" + xe(arn) + "</" + t + "Topic>")
					}
					sb.WriteString("<HeadersIn" + t + "NotificationsEnabled>" + boolS(id.Headers[t]) + "</HeadersIn" + t + "NotificationsEnabled>")
				}
				sb.WriteString("<ForwardingEnabled>" + boolS(!id.NoForwarding) + "</ForwardingEnabled>")
			default:
				if id.MailFrom != "" {
					sb.WriteString("<MailFromDomain>" + xe(id.MailFrom) + "</MailFromDomain><MailFromDomainStatus>Success</MailFromDomainStatus>")
				}
				b := id.MXFailure
				if b == "" {
					b = "UseDefaultValue"
				}
				sb.WriteString("<BehaviorOnMXFailure>" + b + "</BehaviorOnMXFailure>")
			}
			sb.WriteString("</value></entry>")
		}
		root := map[string]string{"GetIdentityVerificationAttributes": "VerificationAttributes", "GetIdentityDkimAttributes": "DkimAttributes",
			"GetIdentityNotificationAttributes": "NotificationAttributes", "GetIdentityMailFromDomainAttributes": "MailFromDomainAttributes"}[action]
		return wrap("<" + root + ">" + sb.String() + "</" + root + ">"), nil
	case "SetIdentityNotificationTopic", "SetIdentityFeedbackForwardingEnabled", "SetIdentityHeadersInNotificationsEnabled",
		"SetIdentityDkimEnabled", "SetIdentityMailFromDomain":
		var id SESIdentity
		if !a.db.get("sesidentity", v.Get("Identity"), &id) {
			return "", errf(400, "InvalidParameterValue", "Identity %s is invalid.", v.Get("Identity"))
		}
		switch action {
		case "SetIdentityNotificationTopic":
			if id.Topics == nil {
				id.Topics = map[string]string{}
			}
			id.Topics[v.Get("NotificationType")] = v.Get("SnsTopic")
		case "SetIdentityFeedbackForwardingEnabled":
			id.NoForwarding = v.Get("ForwardingEnabled") != "true"
		case "SetIdentityHeadersInNotificationsEnabled":
			if id.Headers == nil {
				id.Headers = map[string]bool{}
			}
			id.Headers[v.Get("NotificationType")] = v.Get("Enabled") == "true"
		case "SetIdentityDkimEnabled":
			id.DkimEnabled = v.Get("DkimEnabled") == "true"
		default:
			id.MailFrom, id.MXFailure = v.Get("MailFromDomain"), v.Get("BehaviorOnMXFailure")
		}
		_ = a.db.put("sesidentity", id.Name, id)
		return wrap(""), nil
	case "CreateTemplate", "UpdateTemplate":
		t := SESTemplate{Name: v.Get("Template.TemplateName"), Subject: v.Get("Template.SubjectPart"), Text: v.Get("Template.TextPart"),
			HTML: v.Get("Template.HtmlPart"), Created: nowMs()}
		exists := a.db.has("sestemplate", t.Name)
		if action == "CreateTemplate" && exists {
			return "", errf(400, "AlreadyExists", "Template %s already exists.", t.Name)
		}
		if action == "UpdateTemplate" && !exists {
			return "", errf(400, "TemplateDoesNotExist", "Template %s does not exist.", t.Name)
		}
		_ = a.db.put("sestemplate", t.Name, t)
		return wrap(""), nil
	case "GetTemplate":
		var t SESTemplate
		if !a.db.get("sestemplate", v.Get("TemplateName"), &t) {
			return "", errf(400, "TemplateDoesNotExist", "Template %s does not exist.", v.Get("TemplateName"))
		}
		inner := "<TemplateName>" + xe(t.Name) + "</TemplateName><SubjectPart>" + xe(t.Subject) + "</SubjectPart>"
		if t.Text != "" {
			inner += "<TextPart>" + xe(t.Text) + "</TextPart>"
		}
		if t.HTML != "" {
			inner += "<HtmlPart>" + xe(t.HTML) + "</HtmlPart>"
		}
		return wrap("<Template>" + inner + "</Template>"), nil
	case "ListTemplates":
		var sb strings.Builder
		for _, t := range list[SESTemplate](a.db, "sestemplate") {
			sb.WriteString("<member><Name>" + xe(t.Name) + "</Name><CreatedTimestamp>" + isoMs(t.Created) + "</CreatedTimestamp></member>")
		}
		return wrap("<TemplatesMetadata>" + sb.String() + "</TemplatesMetadata>"), nil
	case "DeleteTemplate":
		_ = a.db.del("sestemplate", v.Get("TemplateName"))
		return wrap(""), nil
	case "CreateConfigurationSet":
		name := v.Get("ConfigurationSet.Name")
		if a.db.has("sesconfigset", name) {
			return "", errf(400, "ConfigurationSetAlreadyExists", "Configuration set <%s> already exists.", name)
		}
		_ = a.db.put("sesconfigset", name, M{"name": name})
		return wrap(""), nil
	case "DescribeConfigurationSet":
		name := v.Get("ConfigurationSetName")
		if !a.db.has("sesconfigset", name) {
			return "", errf(400, "ConfigurationSetDoesNotExist", "Configuration set <%s> does not exist.", name)
		}
		return wrap("<ConfigurationSet><Name>" + xe(name) + "</Name></ConfigurationSet><EventDestinations/>"), nil
	case "DeleteConfigurationSet":
		_ = a.db.del("sesconfigset", v.Get("ConfigurationSetName"))
		return wrap(""), nil
	case "ListConfigurationSets":
		var sb strings.Builder
		for _, n := range a.ids("sesconfigset") {
			sb.WriteString("<member><Name>" + xe(n) + "</Name></member>")
		}
		return wrap("<ConfigurationSets>" + sb.String() + "</ConfigurationSets>"), nil
	case "DescribeActiveReceiptRuleSet":
		return wrap(""), nil
	case "ListReceiptRuleSets":
		return wrap("<RuleSets/>"), nil
	case "GetSendQuota":
		return wrap("<Max24HourSend>200000</Max24HourSend><MaxSendRate>100</MaxSendRate><SentLast24Hours>0</SentLast24Hours>"), nil
	case "GetAccountSendingEnabled":
		return wrap("<Enabled>true</Enabled>"), nil
	}
	return "", errf(400, "InvalidAction", "localaws ses: action %s is not implemented", action)
}

func parseRaw(raw string) Mail {
	m := Mail{API: "SendRawEmail", Raw: raw}
	msg, err := mail.ReadMessage(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		return m
	}
	m.Subject = decodeHeader(msg.Header.Get("Subject"))
	m.Source = msg.Header.Get("From")
	addrs := func(h string) []string {
		l, _ := msg.Header.AddressList(h)
		var out []string
		for _, x := range l {
			out = append(out, x.Address)
		}
		return out
	}
	m.To, m.Cc, m.Bcc = addrs("To"), addrs("Cc"), addrs("Bcc")
	body, _ := io.ReadAll(msg.Body)
	if strings.HasPrefix(msg.Header.Get("Content-Type"), "text/html") {
		m.HTML = string(body)
	} else if !strings.HasPrefix(msg.Header.Get("Content-Type"), "multipart/") {
		m.Text = string(body)
	}
	return m
}

func decodeHeader(s string) string {
	if out, err := new(mime.WordDecoder).DecodeHeader(s); err == nil {
		return out
	}
	return s
}

func (a *App) sesV2Identity(id SESIdentity) M {
	typ := map[string]string{"EmailAddress": "EMAIL_ADDRESS", "Domain": "DOMAIN"}[id.Type]
	dkim := M{"SigningEnabled": id.DkimEnabled || id.Type == "Domain", "Status": "SUCCESS", "SigningAttributesOrigin": "AWS_SES",
		"CurrentSigningKeyLength": "RSA_2048_BIT", "NextSigningKeyLength": "RSA_2048_BIT", "Tokens": nz(id.DkimTokens)}
	if id.Type != "Domain" {
		dkim["Status"] = "NOT_STARTED"
	}
	mx := id.MXFailure
	if mx == "" {
		mx = "USE_DEFAULT_VALUE"
	}
	mail := M{"BehaviorOnMxFailure": strings.ToUpper(mx), "MailFromDomainStatus": "SUCCESS"}
	if id.MailFrom != "" {
		mail["MailFromDomain"] = id.MailFrom
	}
	return M{"IdentityType": typ, "FeedbackForwardingStatus": !id.NoForwarding, "VerifiedForSendingStatus": true, "VerificationStatus": "SUCCESS",
		"DkimAttributes": dkim, "MailFromAttributes": mail, "Policies": M{}, "ConfigurationSetName": id.ConfigSet,
		"Tags": tagList(a.tagsOf(a.arn("ses", "identity/"+id.Name)), "Key", "Value")}
}

// sesV2 handles the v2 REST API: SendEmail (POST /v2/email/outbound-emails), email
// identities and tags.
func (a *App) sesV2(w http.ResponseWriter, r *http.Request) {
	in := readJSON(r)
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2/email/"), "/")
	switch {
	case path == "identities" && r.Method == http.MethodPost:
		name := getStr(in, "EmailIdentity")
		if a.db.has("sesidentity", name) {
			writeJSONErr(w, errf(400, "AlreadyExistsException", "Email identity %s already exist.", name))
			return
		}
		id := a.sesIdentity(name)
		id.ConfigSet = getStr(in, "ConfigurationSetName")
		_ = a.db.put("sesidentity", id.Name, id)
		a.tagResource(a.arn("ses", "identity/"+name), tagsFromList(getList(in, "Tags"), "Key", "Value"))
		out := a.sesV2Identity(id)
		writeJSON(w, 200, M{"IdentityType": out["IdentityType"], "VerifiedForSendingStatus": true, "DkimAttributes": out["DkimAttributes"]})
		return
	case path == "identities" && r.Method == http.MethodGet:
		out := []any{}
		for _, id := range list[SESIdentity](a.db, "sesidentity") {
			out = append(out, M{"IdentityName": id.Name, "IdentityType": a.sesV2Identity(id)["IdentityType"], "SendingEnabled": true, "VerificationStatus": "SUCCESS"})
		}
		writeJSON(w, 200, M{"EmailIdentities": out})
		return
	case strings.HasPrefix(path, "identities/"):
		parts := strings.SplitN(strings.TrimPrefix(path, "identities/"), "/", 2)
		var id SESIdentity
		if !a.db.get("sesidentity", parts[0], &id) {
			writeJSONErr(w, errf(404, "NotFoundException", "Email identity %s does not exist.", parts[0]))
			return
		}
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			writeJSON(w, 200, a.sesV2Identity(id))
		case len(parts) == 1 && r.Method == http.MethodDelete:
			_ = a.db.del("sesidentity", id.Name)
			a.untagAll(a.arn("ses", "identity/"+id.Name))
			writeJSON(w, 200, M{})
		case r.Method == http.MethodPut:
			switch parts[1] {
			case "feedback":
				id.NoForwarding = !getBool(in, "EmailForwardingEnabled")
			case "dkim":
				id.DkimEnabled = getBool(in, "SigningEnabled")
			case "mail-from":
				id.MailFrom, id.MXFailure = getStr(in, "MailFromDomain"), getStr(in, "BehaviorOnMxFailure")
			case "configuration-set":
				id.ConfigSet = getStr(in, "ConfigurationSetName")
			}
			_ = a.db.put("sesidentity", id.Name, id)
			writeJSON(w, 200, M{})
		default:
			writeJSON(w, 200, M{})
		}
		return
	case path == "tags":
		arn := r.URL.Query().Get("ResourceArn")
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, M{"Tags": tagList(a.tagsOf(arn), "Key", "Value")})
		case http.MethodPost:
			a.tagResource(getStr(in, "ResourceArn"), tagsFromList(getList(in, "Tags"), "Key", "Value"))
			writeJSON(w, 200, M{})
		case http.MethodDelete:
			a.untagResource(arn, r.URL.Query()["TagKeys"])
			writeJSON(w, 200, M{})
		}
		return
	case path == "account" && r.Method == http.MethodGet:
		writeJSON(w, 200, M{"ProductionAccessEnabled": true, "SendingEnabled": true, "EnforcementStatus": "HEALTHY", "DedicatedIpAutoWarmupEnabled": false,
			"SendQuota": M{"Max24HourSend": 200000, "MaxSendRate": 100, "SentLast24Hours": 0}})
		return
	}
	if r.Method != http.MethodPost || path != "outbound-emails" {
		writeJSONErr(w, errf(404, "NotFoundException", "localaws sesv2: %s %s is not implemented", r.Method, r.URL.Path))
		return
	}
	dest := getMap(in, "Destination")
	content := getMap(in, "Content")
	m := Mail{API: "v2.SendEmail", Source: getStr(in, "FromEmailAddress"), To: strList(getList(dest, "ToAddresses")),
		Cc: strList(getList(dest, "CcAddresses")), Bcc: strList(getList(dest, "BccAddresses"))}
	switch {
	case len(getMap(content, "Simple")) > 0:
		s := getMap(content, "Simple")
		m.Subject = getStr(getMap(s, "Subject"), "Data")
		m.Text = getStr(getMap(getMap(s, "Body"), "Text"), "Data")
		m.HTML = getStr(getMap(getMap(s, "Body"), "Html"), "Data")
	case len(getMap(content, "Raw")) > 0:
		raw, _ := base64.StdEncoding.DecodeString(getStr(getMap(content, "Raw"), "Data"))
		pm := parseRaw(string(raw))
		pm.API, pm.Source = "v2.SendEmail", m.Source
		if len(m.To) > 0 {
			pm.To = m.To
		}
		m = pm
	case len(getMap(content, "Template")) > 0:
		t := getMap(content, "Template")
		m.Template, m.TemplateData = getStr(t, "TemplateName"), getStr(t, "TemplateData")
	}
	writeJSON(w, 200, M{"MessageId": a.captureMail(m)})
}

// mailbox / sns admin endpoints
func (a *App) adminMessages(w http.ResponseWriter, r *http.Request, kind string) {
	table := map[string]string{"ses": "ses_messages", "sns": "sns_messages"}[kind]
	if r.Method == http.MethodDelete {
		_ = a.db.exec("DELETE FROM " + table)
		writeJSON(w, 200, M{"deleted": true})
		return
	}
	q := r.URL.Query()
	limit := atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	since := int64(atoi(q.Get("since")))
	out := []M{}
	if kind == "ses" {
		rows, err := a.db.db.Query(`SELECT id, ts_ms, api, source, to_addrs, cc_addrs, bcc_addrs, subject, text_body, html_body, template, template_data
			FROM ses_messages WHERE ts_ms >= ? AND (? = '' OR to_addrs LIKE '%' || ? || '%') ORDER BY seq DESC LIMIT ?`, since, q.Get("to"), q.Get("to"), limit)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, api, src, to, cc, bcc, subj, text, html, tpl, tplData string
				var ts int64
				_ = rows.Scan(&id, &ts, &api, &src, &to, &cc, &bcc, &subj, &text, &html, &tpl, &tplData)
				var tl, cl, bl []string
				_ = jsonUnmarshal(to, &tl)
				_ = jsonUnmarshal(cc, &cl)
				_ = jsonUnmarshal(bcc, &bl)
				out = append(out, M{"id": id, "timestamp": ts, "api": api, "source": src, "to": tl, "cc": cl, "bcc": bl,
					"subject": subj, "text": text, "html": html, "template": tpl, "templateData": tplData})
			}
		}
	} else {
		rows, err := a.db.db.Query(`SELECT id, ts_ms, topic, subject, message, attrs FROM sns_messages
			WHERE ts_ms >= ? AND (? = '' OR topic = ?) ORDER BY seq DESC LIMIT ?`, since, q.Get("topic"), q.Get("topic"), limit)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, topic, subj, msg, attrs string
				var ts int64
				_ = rows.Scan(&id, &ts, &topic, &subj, &msg, &attrs)
				var am any
				_ = jsonUnmarshal(attrs, &am)
				out = append(out, M{"id": id, "timestamp": ts, "topic": topic, "subject": subj, "message": msg, "attributes": am})
			}
		}
	}
	writeJSON(w, 200, M{"messages": out})
}
