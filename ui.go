package main

// A read-only console at /_localaws/: everything the emulator holds — executions with
// their event history, tasks with their containers' log streams, bucket versions, log
// groups, parameters, rules and the API calls received — so a local run is as
// inspectable as the AWS console makes a real one.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"
)

const uiCSS = `<style>
:root{--bg:#f6f7f9;--panel:#fff;--fg:#16191f;--mut:#5f6b7a;--line:#dde1e6;--ok:#1d8102;--ng:#d13212;--run:#0972d3;--acc:#0972d3}
@media (prefers-color-scheme: dark){:root{--bg:#0f1218;--panel:#171b23;--fg:#e8eaee;--mut:#9aa4b2;--line:#2b313c;--ok:#6cc04a;--ng:#ff6b57;--run:#6aaef5;--acc:#6aaef5}}
body{font:13px/1.45 system-ui,sans-serif;margin:0;padding:14px 18px;color:var(--fg);background:var(--bg)}
h1{font-size:18px;margin:6px 0}h2{font-size:14px;margin:18px 0 6px}a{color:var(--acc)}
table{border-collapse:collapse;width:100%;background:var(--panel);font-size:12.5px}
th,td{border-bottom:1px solid var(--line);padding:4px 8px;text-align:left;vertical-align:top}th{color:var(--mut)}
code,pre{font-family:ui-monospace,Consolas,monospace;font-size:12px}
pre{white-space:pre-wrap;overflow-wrap:anywhere;background:var(--panel);border:1px solid var(--line);padding:8px;max-height:60vh;overflow:auto}
.meta{color:var(--mut)}.ok{color:var(--ok);font-weight:600}.ng{color:var(--ng);font-weight:600}.run{color:var(--run);font-weight:600}
nav a{margin-right:14px;font-weight:600}td{overflow-wrap:anywhere}</style>`

func cls(s string) string {
	switch s {
	case "SUCCEEDED", "ACTIVE", "ENABLED":
		return "ok"
	case "FAILED", "ABORTED", "TIMED_OUT":
		return "ng"
	}
	return "run"
}

func clock(msv int64) string {
	if msv == 0 {
		return ""
	}
	return time.UnixMilli(msv).Local().Format("01-02 15:04:05.000")
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func pretty(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

var e = html.EscapeString

func (a *App) page(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>localaws · %s</title>%s</head><body>`+
		`<nav><a href="/_localaws/">overview</a><a href="/_localaws/executions">executions</a><a href="/_localaws/tasks">tasks</a>`+
		`<a href="/_localaws/s3">s3</a><a href="/_localaws/logs">logs</a><a href="/_localaws/params">parameters</a><a href="/_localaws/api">api calls</a></nav>`+
		`<h1>%s</h1>%s</body></html>`, e(title), uiCSS, e(title), body)
}

func (a *App) ui(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var sb strings.Builder
	switch strings.TrimPrefix(r.URL.Path, "/_localaws") {
	case "", "/":
		fmt.Fprintf(&sb, `<div class="meta">localaws %s · account <code>%s</code> · region <code>%s</code> · identity <code>%s</code> · runner <b>%s</b> · data <code>%s</code> · up since %s</div>`,
			version, a.cfg.Account, a.cfg.Region, e(a.cfg.IdentityArn()), a.cfg.Runner.Mode, e(a.cfg.DataDir), clock(a.started))
		sb.WriteString("<h2>Step Functions</h2><table><tr><th>state machine</th><th>type</th><th>states</th><th>executions</th></tr>")
		execs := list[Execution](a.db, "execution")
		for _, sm := range list[StateMachine](a.db, "statemachine") {
			var def map[string]any
			_ = json.Unmarshal([]byte(sm.Definition), &def)
			n := 0
			for _, x := range execs {
				if x.SmName == sm.Name {
					n++
				}
			}
			fmt.Fprintf(&sb, `<tr><td>%s</td><td>%s</td><td>%d</td><td><a href="/_localaws/executions?sm=%s">%d</a></td></tr>`, e(sm.Name), sm.Type, len(getMap(def, "States")), e(sm.Name), n)
		}
		sb.WriteString("</table><h2>EventBridge rules</h2><table><tr><th>rule</th><th>state</th><th>pattern / schedule</th><th>targets</th></tr>")
		for _, rl := range list[Rule](a.db, "rule") {
			ts := []string{}
			for _, t := range rl.Targets {
				ts = append(ts, arnTail(t.Arn))
			}
			fmt.Fprintf(&sb, `<tr><td>%s</td><td class="%s">%s</td><td><code>%s</code></td><td>%s</td></tr>`, e(rl.Name), cls(rl.State), rl.State,
				e(short(rl.EventPattern+rl.ScheduleExpression, 220)), e(strings.Join(ts, ", ")))
		}
		sb.WriteString("</table><h2>ECS</h2><table><tr><th>task definition</th><th>revision</th><th>containers</th><th>status</th></tr>")
		for _, td := range list[TaskDef](a.db, "taskdef") {
			names := []string{}
			for _, c := range getList(td.Spec, "containerDefinitions") {
				cm, _ := c.(map[string]any)
				names = append(names, getStr(cm, "name")+" ("+getStr(cm, "image")+")")
			}
			fmt.Fprintf(&sb, `<tr><td>%s</td><td>%d</td><td>%s</td><td class="%s">%s</td></tr>`, e(td.Family), td.Revision, e(strings.Join(names, ", ")), cls(td.Status), td.Status)
		}
		sb.WriteString("</table><table style='margin-top:6px'><tr><th>cluster / service</th><th>desired</th><th>status</th></tr>")
		for _, c := range list[Cluster](a.db, "cluster") {
			fmt.Fprintf(&sb, `<tr><td>%s</td><td></td><td></td></tr>`, e(c.Name))
			for _, s := range list[Service](a.db, "service") {
				if s.Cluster == c.Name {
					fmt.Fprintf(&sb, `<tr><td>&nbsp;&nbsp;%s</td><td>%d</td><td class="%s">%s</td></tr>`, e(s.Name), s.Desired, cls(s.Status), s.Status)
				}
			}
		}
		sb.WriteString("</table><h2>S3</h2><table><tr><th>bucket</th><th>versioning</th><th>EventBridge</th><th>objects</th></tr>")
		for _, b := range list[Bucket](a.db, "bucket") {
			fmt.Fprintf(&sb, `<tr><td><a href="/_localaws/s3?bucket=%s">%s</a></td><td>%s</td><td>%v</td><td>%d</td></tr>`, e(b.Name), e(b.Name), b.Versioning, b.EventBridge, len(a.s3Objects(b.Name, "", false)))
		}
		sb.WriteString("</table><h2>Recent executions</h2>" + a.execTable(execs, 10) + "<h2>Recent tasks</h2>" + a.taskTable(list[Task](a.db, "task"), 10))
		a.page(w, "localaws", sb.String())
	case "/executions":
		var xs []Execution
		for _, x := range list[Execution](a.db, "execution") {
			if sm := q.Get("sm"); sm == "" || x.SmName == sm {
				xs = append(xs, x)
			}
		}
		a.page(w, "Executions", a.execTable(xs, 1000))
	case "/execution":
		x, ok := a.getExec(q.Get("arn"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(&sb, `<div class="meta">%s · <span class="%s">%s</span> · %s → %s · <code>%s</code></div>`, e(x.SmName), cls(x.Status), x.Status, clock(x.Start), clock(x.Stop), e(x.Arn))
		if x.Error != "" || x.Cause != "" {
			fmt.Fprintf(&sb, `<p class="ng">%s</p><pre>%s</pre>`, e(x.Error), e(pretty(x.Cause)))
		}
		sb.WriteString("<h2>Event history</h2><table><tr><th>#</th><th>prev</th><th>time</th><th>type</th><th>details</th></tr>")
		for _, ev := range a.history(x.Arn) {
			det := ""
			for k, v := range ev {
				if strings.HasSuffix(k, "EventDetails") {
					det = short(jsonStr(v), 400)
				}
			}
			ts, _ := ev["timestamp"].(float64)
			fmt.Fprintf(&sb, "<tr><td>%v</td><td>%v</td><td>%s</td><td>%s</td><td><code>%s</code></td></tr>", ev["id"], ev["previousEventId"], clock(int64(ts*1000)), e(str(ev["type"])), e(det))
		}
		sb.WriteString("</table><h2>Tasks started by this execution</h2>")
		var ts []Task
		for _, t := range list[Task](a.db, "task") {
			if t.ExecutionArn == x.Arn {
				ts = append(ts, t)
			}
		}
		sb.WriteString(a.taskTable(ts, 100))
		fmt.Fprintf(&sb, "<h2>Input</h2><pre>%s</pre><h2>Output</h2><pre>%s</pre>", e(pretty(x.Input)), e(pretty(x.Output)))
		a.page(w, "Execution "+x.Name, sb.String())
	case "/tasks":
		a.page(w, "ECS tasks", a.taskTable(list[Task](a.db, "task"), 1000))
	case "/task":
		t := a.task(q.Get("id"))
		if t == nil {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(&sb, `<div class="meta"><span class="%s">%s</span> · %s · %s · started by %s · created %s · started %s · stopped %s</div>`,
			cls(t.Status), t.Status, e(t.StopCode), e(t.StoppedReason), e(t.StartedBy), clock(t.Created), clock(t.Started), clock(t.Stopped))
		if t.ExecutionArn != "" {
			fmt.Fprintf(&sb, `<p>started by execution <a href="/_localaws/execution?arn=%s">%s</a></p>`, e(t.ExecutionArn), e(arnTail(t.ExecutionArn)))
		}
		for _, c := range t.Containers {
			code := "–"
			if c.ExitCode != nil {
				code = fmt.Sprint(*c.ExitCode)
			}
			fmt.Fprintf(&sb, "<h2>%s · exit %s</h2><div class='meta'>image <code>%s</code> · command <code>%s</code></div>", e(c.Name), code, e(c.Image), e(strings.Join(append(c.EntryPoint, c.Command...), " ")))
			if c.LogGroup != "" {
				fmt.Fprintf(&sb, `<p class="meta">log stream <a href="/_localaws/logs?group=%s&stream=%s">%s</a></p>`, e(c.LogGroup), e(c.LogStream), e(c.LogStream))
				sb.WriteString(a.logTable(c.LogGroup, c.LogStream, 500))
			}
		}
		a.page(w, "Task "+t.ID, sb.String())
	case "/s3":
		b := q.Get("bucket")
		if b == "" {
			sb.WriteString("<table><tr><th>bucket</th></tr>")
			for _, x := range list[Bucket](a.db, "bucket") {
				fmt.Fprintf(&sb, `<tr><td><a href="/_localaws/s3?bucket=%s">%s</a></td></tr>`, e(x.Name), e(x.Name))
			}
			a.page(w, "S3", sb.String()+"</table>")
			return
		}
		sb.WriteString("<table><tr><th>key</th><th>version</th><th>size</th><th>etag</th><th>modified</th><th>latest</th></tr>")
		last := "\x00"
		for _, o := range a.s3Objects(b, q.Get("prefix"), true) {
			latest := o.Key != last
			last = o.Key
			if o.DeleteMarker {
				fmt.Fprintf(&sb, `<tr><td>%s</td><td><code>%s</code></td><td colspan="3" class="meta">delete marker</td><td>%v</td></tr>`, e(o.Key), e(o.VersionID), latest)
				continue
			}
			fmt.Fprintf(&sb, `<tr><td><a href="/%s/%s?versionId=%s">%s</a></td><td><code>%s</code></td><td>%d</td><td><code>%s</code></td><td>%s</td><td>%v</td></tr>`,
				e(b), e(o.Key), e(o.VersionID), e(o.Key), e(o.VersionID), o.Size, o.ETag, clock(o.Modified), latest)
		}
		a.page(w, "s3://"+b+"/"+q.Get("prefix"), sb.String()+"</table>")
	case "/logs":
		g, s := q.Get("group"), q.Get("stream")
		switch {
		case g == "":
			sb.WriteString("<table><tr><th>log group</th></tr>")
			for _, x := range list[LogGroup](a.db, "loggroup") {
				fmt.Fprintf(&sb, `<tr><td><a href="/_localaws/logs?group=%s">%s</a></td></tr>`, e(x.Name), e(x.Name))
			}
			a.page(w, "Log groups", sb.String()+"</table>")
		case s == "":
			sb.WriteString("<table><tr><th>stream</th><th>created</th></tr>")
			streams := list[LogStream](a.db, "logstream")
			sort.Slice(streams, func(i, j int) bool { return streams[i].Created > streams[j].Created })
			for _, x := range streams {
				if x.Group == g {
					fmt.Fprintf(&sb, `<tr><td><a href="/_localaws/logs?group=%s&stream=%s">%s</a></td><td>%s</td></tr>`, e(g), e(x.Name), e(x.Name), clock(x.Created))
				}
			}
			a.page(w, "Log group "+g, sb.String()+"</table>")
		default:
			a.page(w, g+" / "+s, a.logTable(g, s, 10000))
		}
	case "/params":
		sb.WriteString("<table><tr><th>name</th><th>type</th><th>version</th><th>value</th></tr>")
		for _, p := range list[Param](a.db, "param") {
			v := p.Value
			if p.Type == "SecureString" {
				v = fmt.Sprintf("(SecureString, %d chars)", len(p.Value))
			}
			fmt.Fprintf(&sb, "<tr><td>%s</td><td>%s</td><td>%d</td><td><code>%s</code></td></tr>", e(p.Name), p.Type, p.Version, e(short(v, 200)))
		}
		a.page(w, "SSM parameters", sb.String()+"</table>")
	case "/api":
		sb.WriteString("<table><tr><th>time</th><th>operation</th><th>status</th><th>ms</th></tr>")
		a.apiMu.Lock()
		for i := len(a.apiLog) - 1; i >= 0; i-- {
			c := a.apiLog[i]
			fmt.Fprintf(&sb, "<tr><td>%s</td><td><code>%s</code></td><td>%d</td><td>%d</td></tr>", clock(c.At), e(c.What), c.Status, c.Ms)
		}
		a.apiMu.Unlock()
		a.page(w, "API calls (last 1000)", sb.String()+"</table>")
	default:
		http.NotFound(w, r)
	}
}

func (a *App) execTable(xs []Execution, limit int) string {
	sort.Slice(xs, func(i, j int) bool { return xs[i].Start > xs[j].Start })
	var sb strings.Builder
	sb.WriteString("<table><tr><th>started</th><th>state machine</th><th>name</th><th>status</th><th>duration</th><th>error</th></tr>")
	for i, x := range xs {
		if i >= limit {
			break
		}
		dur := ""
		if x.Stop > 0 {
			dur = (time.Duration(x.Stop-x.Start) * time.Millisecond).String()
		}
		fmt.Fprintf(&sb, `<tr><td>%s</td><td>%s</td><td><a href="/_localaws/execution?arn=%s">%s</a></td><td class="%s">%s</td><td>%s</td><td>%s</td></tr>`,
			clock(x.Start), e(x.SmName), e(x.Arn), e(short(x.Name, 60)), cls(x.Status), x.Status, dur, e(x.Error))
	}
	return sb.String() + "</table>"
}

func (a *App) taskTable(ts []Task, limit int) string {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Created > ts[j].Created })
	var sb strings.Builder
	sb.WriteString("<table><tr><th>created</th><th>task</th><th>family</th><th>status</th><th>exit</th><th>stop code</th><th>started by</th><th>command</th></tr>")
	for i, t := range ts {
		if i >= limit {
			break
		}
		code, cmd := "", ""
		if len(t.Containers) > 0 {
			if c := t.Containers[0]; c.ExitCode != nil {
				code = fmt.Sprint(*c.ExitCode)
			}
			cmd = strings.Join(append(t.Containers[0].EntryPoint, t.Containers[0].Command...), " ")
		}
		fmt.Fprintf(&sb, `<tr><td>%s</td><td><a href="/_localaws/task?id=%s">%s</a></td><td>%s</td><td class="%s">%s</td><td>%s</td><td>%s</td><td>%s</td><td><code>%s</code></td></tr>`,
			clock(t.Created), t.ID, t.ID[:12], e(t.Family), cls(t.Status), t.Status, code, e(t.StopCode), e(t.StartedBy), e(short(cmd, 160)))
	}
	return sb.String() + "</table>"
}

func (a *App) logTable(g, s string, limit int) string {
	var sb strings.Builder
	evs := a.streamEvents(g, s, limit)
	sb.WriteString(fmt.Sprintf("<div class='meta'>%d event(s)</div><table><tr><th>time</th><th>message</th></tr>", len(evs)))
	for _, ev := range evs {
		fmt.Fprintf(&sb, "<tr><td>%s</td><td><code>%s</code></td></tr>", clock(ev.Ts), e(short(ev.Msg, 2000)))
	}
	return sb.String() + "</table>"
}
