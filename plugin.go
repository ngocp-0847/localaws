package main

// e2e-studio plugin protocol (e2e-studio.plugin/v1) — lets a testing studio show this
// emulator's state next to its test runs, while localaws stays a standalone tool with its
// own console. Read-only; two endpoints:
//
//	GET /_localaws/plugin                      manifest: identity, status, summary metrics,
//	                                           sections, the native console's path
//	GET /_localaws/plugin/sections/<id>?limit= one section as a typed table
//
// Tables are { columns: [{key, label, type}], rows: [{key: value}] }; column types are
// text | number | time (epoch ms) | duration (ms) | status | code.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const pluginProtocol = "e2e-studio.plugin/v1"

type pluginColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

type pluginTable struct {
	Columns []pluginColumn `json:"columns"`
	Rows    []M            `json:"rows"`
	Total   int            `json:"total"`
}

var pluginSections = []M{
	{"id": "queues", "title": "SQS queues", "kind": "table", "description": "messages waiting, in flight and dead-lettered"},
	{"id": "mail", "title": "Captured mail (SES)", "kind": "table", "description": "newest first — never delivered"},
	{"id": "executions", "title": "Step Functions executions", "kind": "table"},
	{"id": "tasks", "title": "ECS tasks", "kind": "table"},
	{"id": "buckets", "title": "S3 buckets", "kind": "table"},
	{"id": "calls", "title": "API activity", "kind": "table", "description": "the last calls received, newest first"},
}

func (a *App) pluginQueues() []M {
	var rows []M
	for _, q := range list[Queue](a.db, "queue") {
		at := a.queueAttrs(q)
		dlq, max := redrive(q)
		rows = append(rows, M{"name": q.Name, "visible": atoi(at["ApproximateNumberOfMessages"]),
			"inflight": atoi(at["ApproximateNumberOfMessagesNotVisible"]), "delayed": atoi(at["ApproximateNumberOfMessagesDelayed"]),
			"deadLetter": dlq, "maxReceive": max})
	}
	return rows
}

func (a *App) pluginManifest() M {
	queues := a.pluginQueues()
	waiting, dead := 0, 0
	dlqs := map[string]bool{}
	for _, q := range queues {
		if d := str(q["deadLetter"]); d != "" {
			dlqs[d] = true
		}
	}
	for _, q := range queues {
		if dlqs[str(q["name"])] {
			dead += q["visible"].(int)
		} else {
			waiting += q["visible"].(int) + q["inflight"].(int)
		}
	}
	var mails int
	_ = a.db.db.QueryRow(`SELECT count(*) FROM ses_messages`).Scan(&mails)
	running, failedExec := 0, 0
	for _, e := range list[Execution](a.db, "execution") {
		switch e.Status {
		case "RUNNING":
			running++
		case "FAILED", "TIMED_OUT", "ABORTED":
			failedExec++
		}
	}
	tasks := 0
	for _, t := range list[Task](a.db, "task") {
		if t.Status != "STOPPED" {
			tasks++
		}
	}
	calls := a.consoleCalls()
	errs := 0
	for _, c := range calls {
		if c.Status >= 400 {
			errs++
		}
	}
	tone := func(bad bool) string {
		if bad {
			return "bad"
		}
		return ""
	}
	status := "ok"
	if dead > 0 || failedExec > 0 {
		status = "degraded"
	}
	return M{
		"protocol":    pluginProtocol,
		"name":        "localaws",
		"title":       "localaws",
		"version":     version,
		"description": "AWS emulator — S3, SQS, SES (captured), SNS, EventBridge, Step Functions, ECS, CloudWatch Logs, SSM",
		"status":      status,
		"since":       a.started,
		"details":     M{"account": a.cfg.Account, "region": a.cfg.Region, "runner": a.cfg.Runner.Mode, "endpoint": a.cfg.PublicURL},
		"ui":          M{"path": "/_localaws/", "label": "Open localaws console"},
		"summary": []M{
			{"key": "queues", "label": "SQS queues", "value": len(queues)},
			{"key": "waiting", "label": "Messages waiting / in flight", "value": waiting},
			{"key": "dead", "label": "Dead-lettered messages", "value": dead, "tone": tone(dead > 0)},
			{"key": "mail", "label": "Mails captured", "value": mails},
			{"key": "executions", "label": "Executions running", "value": running},
			{"key": "failedExecutions", "label": "Executions failed", "value": failedExec, "tone": tone(failedExec > 0)},
			{"key": "tasks", "label": "ECS tasks running", "value": tasks},
			{"key": "apiErrors", "label": "API errors (last " + strconv.Itoa(len(calls)) + " calls)", "value": errs, "tone": tone(errs > 0)},
		},
		"sections": pluginSections,
	}
}

func (a *App) pluginSection(id string, limit int) (pluginTable, bool) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var t pluginTable
	cut := func() {
		t.Total = len(t.Rows)
		if len(t.Rows) > limit {
			t.Rows = t.Rows[:limit]
		}
		if t.Rows == nil {
			t.Rows = []M{}
		}
	}
	switch id {
	case "queues":
		t.Columns = []pluginColumn{{"name", "Queue", "code"}, {"visible", "Waiting", "number"}, {"inflight", "In flight", "number"},
			{"delayed", "Delayed", "number"}, {"deadLetter", "Dead-letter queue", "code"}, {"maxReceive", "Max receives", "number"}}
		t.Rows = a.pluginQueues()
	case "mail":
		t.Columns = []pluginColumn{{"timestamp", "Received", "time"}, {"to", "To", "text"}, {"subject", "Subject", "text"}, {"source", "From", "text"}, {"api", "API", "code"}}
		rows, err := a.db.db.Query(`SELECT ts_ms, to_addrs, subject, source, api FROM ses_messages ORDER BY seq DESC LIMIT ?`, limit)
		if err == nil {
			for rows.Next() {
				var ts int64
				var to, subj, src, api string
				_ = rows.Scan(&ts, &to, &subj, &src, &api)
				var tl []string
				_ = jsonUnmarshal(to, &tl)
				t.Rows = append(t.Rows, M{"timestamp": ts, "to": strings.Join(tl, ", "), "subject": subj, "source": src, "api": api})
			}
			rows.Close()
		}
		_ = a.db.db.QueryRow(`SELECT count(*) FROM ses_messages`).Scan(&t.Total)
		if t.Rows == nil {
			t.Rows = []M{}
		}
		return t, true
	case "executions":
		t.Columns = []pluginColumn{{"start", "Started", "time"}, {"stateMachine", "State machine", "code"}, {"name", "Execution", "code"},
			{"status", "Status", "status"}, {"duration", "Duration", "duration"}, {"error", "Error", "text"}}
		ex := list[Execution](a.db, "execution")
		sort.Slice(ex, func(i, j int) bool { return ex[i].Start > ex[j].Start })
		for _, e := range ex {
			var d any
			if e.Stop > 0 {
				d = e.Stop - e.Start
			}
			t.Rows = append(t.Rows, M{"start": e.Start, "stateMachine": e.SmName, "name": e.Name, "status": e.Status, "duration": d, "error": e.Error})
		}
	case "tasks":
		t.Columns = []pluginColumn{{"created", "Created", "time"}, {"family", "Task definition", "code"}, {"cluster", "Cluster", "code"},
			{"status", "Status", "status"}, {"exit", "Exit", "text"}, {"reason", "Stop reason", "text"}}
		ts := list[Task](a.db, "task")
		sort.Slice(ts, func(i, j int) bool { return ts[i].Created > ts[j].Created })
		for _, tk := range ts {
			var exits []string
			for _, c := range tk.Containers {
				if c.ExitCode != nil {
					exits = append(exits, c.Name+"="+strconv.Itoa(*c.ExitCode))
				}
			}
			t.Rows = append(t.Rows, M{"created": tk.Created, "family": tk.Family, "cluster": arnTail(tk.Cluster), "status": tk.Status,
				"exit": strings.Join(exits, " "), "reason": tk.StoppedReason})
		}
	case "buckets":
		t.Columns = []pluginColumn{{"name", "Bucket", "code"}, {"objects", "Objects", "number"}, {"bytes", "Size (bytes)", "number"}, {"created", "Created", "time"}}
		for _, b := range list[Bucket](a.db, "bucket") {
			var n, size int64
			_ = a.db.db.QueryRow(`SELECT count(*), coalesce(sum(size),0) FROM s3_objects o WHERE bucket=? AND delete_marker=0
				AND seq = (SELECT max(seq) FROM s3_objects WHERE bucket=o.bucket AND key=o.key)`, b.Name).Scan(&n, &size)
			t.Rows = append(t.Rows, M{"name": b.Name, "objects": n, "bytes": size, "created": b.Created})
		}
	case "calls":
		t.Columns = []pluginColumn{{"at", "Time", "time"}, {"what", "Operation", "code"}, {"status", "HTTP", "status"}, {"ms", "Duration", "duration"}}
		calls := a.consoleCalls()
		for i := len(calls) - 1; i >= 0; i-- {
			c := calls[i]
			t.Rows = append(t.Rows, M{"at": c.At, "what": c.What, "status": c.Status, "ms": c.Ms})
		}
	default:
		return t, false
	}
	cut()
	return t, true
}

func (a *App) pluginHandler(w http.ResponseWriter, r *http.Request, p string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if p == "/plugin" || p == "/plugin/" {
		writeJSON(w, 200, a.pluginManifest())
		return
	}
	id := strings.TrimPrefix(p, "/plugin/sections/")
	t, ok := a.pluginSection(id, atoi(r.URL.Query().Get("limit")))
	if !ok {
		writeJSONErr(w, errf(404, "NotFound", "no plugin section %q", id))
		return
	}
	writeJSON(w, 200, t)
}
