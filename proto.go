package main

// Wire-level helpers shared by every service: ids, timestamps, ARNs, the AWS JSON
// protocol (X-Amz-Target), the Query protocol (STS, EC2, SNS) and S3's REST/XML errors.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowMs() int64 { return time.Now().UnixMilli() }

// epoch is the JSON-protocol timestamp encoding (fractional seconds).
func epoch(msv int64) float64 { return float64(msv) / 1000 }

func isoMs(msv int64) string { return time.UnixMilli(msv).UTC().Format("2006-01-02T15:04:05.000Z") }

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonUnmarshal(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// str renders a decoded JSON value as text (numbers without exponent).
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		return jsonStr(x)
	}
}

// arnTail returns what follows the last ':' or '/' of an ARN (or the input itself).
func arnTail(s string) string {
	if i := strings.LastIndexAny(s, ":/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (a *App) arn(service, resource string) string {
	return fmt.Sprintf("arn:aws:%s:%s:%s:%s", service, a.cfg.Region, a.cfg.Account, resource)
}

// ── JSON protocol ────────────────────────────────────────────────────────
type apiError struct {
	Status int
	Type   string
	Msg    string
}

func (e *apiError) Error() string { return e.Type + ": " + e.Msg }

func errf(status int, typ, format string, args ...any) *apiError {
	return &apiError{status, typ, fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.Header().Set("x-amzn-RequestId", uuid4())
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeJSONErr(w http.ResponseWriter, e *apiError) {
	w.Header().Set("x-amzn-ErrorType", e.Type)
	writeJSON(w, e.Status, map[string]string{"__type": e.Type, "message": e.Msg, "Message": e.Msg})
}

func readJSON(r *http.Request) map[string]any {
	b, _ := io.ReadAll(r.Body)
	out := map[string]any{}
	if len(b) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(b)))
		_ = dec.Decode(&out)
	}
	return out
}

type M = map[string]any

func getStr(m M, k string) string { return str(m[k]) }

func getBool(m M, k string) bool {
	b, _ := m[k].(bool)
	return b
}

func getInt(m M, k string, def int) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getList(m M, k string) []any {
	l, _ := m[k].([]any)
	return l
}

func getMap(m M, k string) M {
	mm, _ := m[k].(map[string]any)
	if mm == nil {
		return M{}
	}
	return mm
}

func strList(l []any) []string {
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, str(x))
	}
	return out
}

// pageOf: pagination tokens are plain offsets — nothing hides behind them.
func pageOf[T any](items []T, token string, limit int) ([]T, string) {
	if limit <= 0 {
		limit = 100
	}
	off := atoi(token)
	if off > len(items) {
		off = len(items)
	}
	end := off + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[off:end], next
}

// ── XML (S3 and the Query protocol) ─────────────────────────────────────
func writeXML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", strings.ToUpper(randHex(8)))
	w.Header().Set("x-amz-id-2", randHex(16))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+body)
}

func xe(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func s3Err(w http.ResponseWriter, status int, code, msg, resource string) {
	extra := ""
	if resource != "" {
		extra = "<Resource>" + xe(resource) + "</Resource>"
	}
	writeXML(w, status, "<Error><Code>"+code+"</Code><Message>"+xe(msg)+"</Message>"+extra+
		"<RequestId>"+strings.ToUpper(randHex(8))+"</RequestId></Error>")
}

func queryErr(w http.ResponseWriter, e *apiError) {
	writeXML(w, e.Status, "<ErrorResponse><Error><Type>Sender</Type><Code>"+e.Type+"</Code><Message>"+
		xe(e.Msg)+"</Message></Error><RequestId>"+uuid4()+"</RequestId></ErrorResponse>")
}
