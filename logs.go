package main

// CloudWatch Logs: groups, streams, PutLogEvents, GetLogEvents (forward / backward
// tokens with the real "same token means no more events" contract), FilterLogEvents
// (terms, "quoted phrases", ?any-of, and { $.field = value } JSON conditions joined by &&).

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type LogGroup struct {
	Name      string `json:"name"`
	Created   int64  `json:"created"`
	Retention int    `json:"retention,omitempty"`
}

type LogStream struct {
	Group   string `json:"group"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

func (a *App) groupExists(g string) bool { return a.db.has("loggroup", g) }

func (a *App) ensureGroup(g string) {
	if !a.groupExists(g) {
		_ = a.db.put("loggroup", g, LogGroup{Name: g, Created: nowMs()})
	}
}

func (a *App) ensureStream(g, s string) {
	if !a.db.has("logstream", g+"\x00"+s) {
		_ = a.db.put("logstream", g+"\x00"+s, LogStream{Group: g, Name: s, Created: nowMs()})
	}
}

func (a *App) appendLog(g, s, msg string) {
	now := nowMs()
	_ = a.db.exec(`INSERT INTO log_events(grp, stream, ts_ms, ingest_ms, message) VALUES(?,?,?,?,?)`, g, s, now, now, msg)
}

type logEvent struct {
	ID, Ts, Ingest int64
	Stream, Msg    string
}

func (a *App) queryEvents(q string, args ...any) []logEvent {
	rows, err := a.db.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []logEvent
	for rows.Next() {
		var e logEvent
		_ = rows.Scan(&e.ID, &e.Stream, &e.Ts, &e.Ingest, &e.Msg)
		out = append(out, e)
	}
	return out
}

const evCols = `id, stream, ts_ms, ingest_ms, message`

func (a *App) streamEvents(g, s string, limit int) []logEvent {
	return a.queryEvents(`SELECT `+evCols+` FROM log_events WHERE grp=? AND stream=? ORDER BY id LIMIT ?`, g, s, limit)
}

// ── filter patterns ──────────────────────────────────────────────────────
var jsonCond = regexp.MustCompile(`\$\.([A-Za-z0-9_.\[\]]+)\s*(=|!=|>=|<=|>|<)\s*("(?:[^"\\]|\\.)*"|[^\s&|}]+)`)

func filterMatch(pattern, msg string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}
	if strings.HasPrefix(pattern, "{") && strings.HasSuffix(pattern, "}") {
		var doc any
		if json.Unmarshal([]byte(msg), &doc) != nil {
			return false
		}
		for _, cond := range strings.Split(strings.Trim(pattern, "{} "), "&&") {
			m := jsonCond.FindStringSubmatch(cond)
			if m == nil {
				return false
			}
			v, err := jsonPath("$."+m[1], doc)
			if err != nil {
				return false
			}
			want := strings.Trim(m[3], `"`)
			got := str(v)
			wantN, e1 := strconv.ParseFloat(want, 64)
			gotN, ok := v.(float64)
			cmp := strings.Compare(got, want)
			if e1 == nil && ok {
				cmp = map[bool]int{true: -1, false: 0}[gotN < wantN]
				if gotN > wantN {
					cmp = 1
				}
			} else if strings.Contains(want, "*") && (m[2] == "=" || m[2] == "!=") {
				hit := wildcard(want, got)
				if (m[2] == "=") != hit {
					return false
				}
				continue
			}
			ok2 := map[string]bool{"=": cmp == 0, "!=": cmp != 0, ">": cmp > 0, "<": cmp < 0, ">=": cmp >= 0, "<=": cmp <= 0}[m[2]]
			if !ok2 {
				return false
			}
		}
		return true
	}
	var any_, all []string
	for _, t := range regexp.MustCompile(`\?"[^"]*"|"[^"]*"|\S+`).FindAllString(pattern, -1) {
		if strings.HasPrefix(t, "?") {
			any_ = append(any_, strings.Trim(t[1:], `"`))
		} else {
			all = append(all, strings.Trim(t, `"`))
		}
	}
	for _, t := range all {
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			if strings.Contains(msg, t[1:]) {
				return false
			}
			continue
		}
		if !strings.Contains(msg, t) {
			return false
		}
	}
	if len(any_) == 0 {
		return true
	}
	for _, t := range any_ {
		if strings.Contains(msg, t) {
			return true
		}
	}
	return false
}

// ── API ──────────────────────────────────────────────────────────────────
func (a *App) logsAPI(op string, in M) (any, *apiError) {
	group := getStr(in, "logGroupName")
	if group == "" && getStr(in, "logGroupIdentifier") != "" {
		group = strings.TrimSuffix(arnTail(getStr(in, "logGroupIdentifier")), ":*")
	}
	notFound := errf(400, "ResourceNotFoundException", "The specified log group does not exist.")
	switch op {
	case "CreateLogGroup":
		if a.groupExists(group) {
			return nil, errf(400, "ResourceAlreadyExistsException", "The specified log group already exists")
		}
		a.ensureGroup(group)
		return M{}, nil
	case "DeleteLogGroup":
		if !a.groupExists(group) {
			return nil, notFound
		}
		_ = a.db.del("loggroup", group)
		_ = a.db.exec(`DELETE FROM log_events WHERE grp=?`, group)
		_ = a.db.exec(`DELETE FROM resources WHERE kind='logstream' AND id LIKE ?`, group+"\x00%")
		return M{}, nil
	case "PutRetentionPolicy", "DeleteRetentionPolicy":
		var g LogGroup
		if !a.db.get("loggroup", group, &g) {
			return nil, notFound
		}
		g.Retention = getInt(in, "retentionInDays", 0)
		_ = a.db.put("loggroup", group, g)
		return M{}, nil
	case "DescribeLogGroups":
		out := []any{}
		for _, g := range list[LogGroup](a.db, "loggroup") {
			if p := getStr(in, "logGroupNamePrefix"); p != "" && !strings.HasPrefix(g.Name, p) {
				continue
			}
			m := M{"logGroupName": g.Name, "creationTime": g.Created, "storedBytes": 0, "metricFilterCount": 0,
				"arn": a.arn("logs", "log-group:"+g.Name+":*"), "logGroupArn": a.arn("logs", "log-group:"+g.Name), "logGroupClass": "STANDARD"}
			if g.Retention > 0 {
				m["retentionInDays"] = g.Retention
			}
			out = append(out, m)
		}
		sort.Slice(out, func(i, j int) bool { return str(out[i].(M)["logGroupName"]) < str(out[j].(M)["logGroupName"]) })
		page, next := pageOf(out, getStr(in, "nextToken"), getInt(in, "limit", 50))
		resp := M{"logGroups": page}
		if next != "" {
			resp["nextToken"] = next
		}
		return resp, nil
	case "CreateLogStream":
		if !a.groupExists(group) {
			return nil, notFound
		}
		if a.db.has("logstream", group+"\x00"+getStr(in, "logStreamName")) {
			return nil, errf(400, "ResourceAlreadyExistsException", "The specified log stream already exists")
		}
		a.ensureStream(group, getStr(in, "logStreamName"))
		return M{}, nil
	case "DeleteLogStream":
		_ = a.db.del("logstream", group+"\x00"+getStr(in, "logStreamName"))
		_ = a.db.exec(`DELETE FROM log_events WHERE grp=? AND stream=?`, group, getStr(in, "logStreamName"))
		return M{}, nil
	case "PutLogEvents":
		stream := getStr(in, "logStreamName")
		if !a.groupExists(group) {
			return nil, notFound
		}
		if !a.db.has("logstream", group+"\x00"+stream) {
			return nil, errf(400, "ResourceNotFoundException", "The specified log stream does not exist.")
		}
		evs := getList(in, "logEvents")
		sort.SliceStable(evs, func(i, j int) bool {
			return getInt(evs[i].(map[string]any), "timestamp", 0) < getInt(evs[j].(map[string]any), "timestamp", 0)
		})
		for _, e := range evs {
			em, _ := e.(map[string]any)
			ts := int64(getInt(em, "timestamp", int(nowMs())))
			if f, ok := em["timestamp"].(float64); ok {
				ts = int64(f)
			}
			_ = a.db.exec(`INSERT INTO log_events(grp, stream, ts_ms, ingest_ms, message) VALUES(?,?,?,?,?)`, group, stream, ts, nowMs(), getStr(em, "message"))
		}
		return M{"nextSequenceToken": strconv.FormatInt(a.db.next("logseq"), 10)}, nil
	case "DescribeLogStreams":
		if !a.groupExists(group) {
			return nil, notFound
		}
		type agg struct{ first, last, ingest int64 }
		stats := map[string]agg{}
		rows, err := a.db.db.Query(`SELECT stream, MIN(ts_ms), MAX(ts_ms), MAX(ingest_ms) FROM log_events WHERE grp=? GROUP BY stream`, group)
		if err == nil {
			for rows.Next() {
				var s string
				var g agg
				_ = rows.Scan(&s, &g.first, &g.last, &g.ingest)
				stats[s] = g
			}
			rows.Close()
		}
		var streams []LogStream
		prefix := getStr(in, "logStreamNamePrefix")
		for _, s := range list[LogStream](a.db, "logstream") {
			if s.Group == group && strings.HasPrefix(s.Name, prefix) {
				streams = append(streams, s)
			}
		}
		desc := getBool(in, "descending")
		byTime := getStr(in, "orderBy") == "LastEventTime"
		sort.Slice(streams, func(i, j int) bool {
			if byTime {
				ki, kj := stats[streams[i].Name].last, stats[streams[j].Name].last
				if ki == 0 {
					ki = streams[i].Created
				}
				if kj == 0 {
					kj = streams[j].Created
				}
				if desc {
					return ki > kj
				}
				return ki < kj
			}
			if desc {
				return streams[i].Name > streams[j].Name
			}
			return streams[i].Name < streams[j].Name
		})
		page, next := pageOf(streams, getStr(in, "nextToken"), getInt(in, "limit", 50))
		out := []any{}
		for _, s := range page {
			m := M{"logStreamName": s.Name, "creationTime": s.Created, "storedBytes": 0,
				"arn": a.arn("logs", "log-group:"+group+":log-stream:"+s.Name)}
			if st, ok := stats[s.Name]; ok {
				m["firstEventTimestamp"], m["lastEventTimestamp"], m["lastIngestionTime"] = st.first, st.last, st.ingest
			}
			out = append(out, m)
		}
		resp := M{"logStreams": out}
		if next != "" {
			resp["nextToken"] = next
		}
		return resp, nil
	case "GetLogEvents":
		stream := getStr(in, "logStreamName")
		if !a.groupExists(group) {
			return nil, notFound
		}
		if !a.db.has("logstream", group+"\x00"+stream) {
			return nil, errf(400, "ResourceNotFoundException", "The specified log stream does not exist.")
		}
		limit := getInt(in, "limit", 10000)
		start, end := int64(getInt(in, "startTime", 0)), int64(getInt(in, "endTime", 0))
		if end == 0 {
			end = 1 << 62
		}
		tok := getStr(in, "nextToken")
		var evs []logEvent
		switch {
		case strings.HasPrefix(tok, "f/"):
			after, _ := strconv.ParseInt(tok[2:], 10, 64)
			evs = a.queryEvents(`SELECT `+evCols+` FROM log_events WHERE grp=? AND stream=? AND id>? AND ts_ms>=? AND ts_ms<=? ORDER BY id LIMIT ?`, group, stream, after, start, end, limit)
		case strings.HasPrefix(tok, "b/") || (tok == "" && !getBool(in, "startFromHead")):
			before := int64(1 << 62)
			if tok != "" {
				before, _ = strconv.ParseInt(tok[2:], 10, 64)
			}
			evs = a.queryEvents(`SELECT `+evCols+` FROM log_events WHERE grp=? AND stream=? AND id<? AND ts_ms>=? AND ts_ms<=? ORDER BY id DESC LIMIT ?`, group, stream, before, start, end, limit)
			for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
				evs[i], evs[j] = evs[j], evs[i]
			}
		default:
			evs = a.queryEvents(`SELECT `+evCols+` FROM log_events WHERE grp=? AND stream=? AND ts_ms>=? AND ts_ms<=? ORDER BY id LIMIT ?`, group, stream, start, end, limit)
		}
		out := []any{}
		fwd, back := tok, tok
		if tok == "" {
			fwd, back = "f/0", "b/0"
		}
		for i, e := range evs {
			out = append(out, M{"timestamp": e.Ts, "message": e.Msg, "ingestionTime": e.Ingest})
			if i == 0 {
				back = "b/" + strconv.FormatInt(e.ID, 10)
			}
			fwd = "f/" + strconv.FormatInt(e.ID, 10)
		}
		if strings.HasPrefix(fwd, "b/") {
			fwd = "f/" + strings.TrimPrefix(fwd, "b/")
		}
		return M{"events": out, "nextForwardToken": fwd, "nextBackwardToken": back}, nil
	case "FilterLogEvents":
		if !a.groupExists(group) {
			return nil, notFound
		}
		after := int64(0)
		if t := getStr(in, "nextToken"); t != "" {
			after, _ = strconv.ParseInt(t, 10, 64)
		}
		start, end := int64(getInt(in, "startTime", 0)), int64(getInt(in, "endTime", 0))
		if end == 0 {
			end = 1 << 62
		}
		names := map[string]bool{}
		for _, s := range strList(getList(in, "logStreamNames")) {
			names[s] = true
		}
		prefix := getStr(in, "logStreamNamePrefix")
		pattern := getStr(in, "filterPattern")
		limit := getInt(in, "limit", 10000)
		out := []any{}
		var last int64
		more := false
		for _, e := range a.queryEvents(`SELECT `+evCols+` FROM log_events WHERE grp=? AND id>? AND ts_ms>=? AND ts_ms<=? ORDER BY ts_ms, id`, group, after, start, end) {
			if (len(names) > 0 && !names[e.Stream]) || !strings.HasPrefix(e.Stream, prefix) || !filterMatch(pattern, e.Msg) {
				continue
			}
			if len(out) >= limit {
				more = true
				break
			}
			out = append(out, M{"logStreamName": e.Stream, "timestamp": e.Ts, "message": e.Msg, "ingestionTime": e.Ingest,
				"eventId": strconv.FormatInt(e.ID, 10)})
			last = e.ID
		}
		resp := M{"events": out, "searchedLogStreams": []any{}}
		if more {
			resp["nextToken"] = strconv.FormatInt(last, 10)
		}
		return resp, nil
	case "TagLogGroup", "TagResource", "UntagResource":
		return M{}, nil
	case "ListTagsLogGroup", "ListTagsForResource":
		return M{"tags": M{}}, nil
	}
	return nil, errf(400, "UnknownOperationException", "localaws logs: %s is not implemented", op)
}
