package main

// EventBridge (default bus): PutRule / DescribeRule / ListRules / DeleteRule / Enable /
// Disable, PutTargets / RemoveTargets / ListTargetsByRule, PutEvents, rate() schedules.
// Every event — from PutEvents, from S3 buckets with EventBridge notifications, from a
// schedule, from a state machine's events:putEvents — is matched against every ENABLED
// rule; matching targets receive it (Input / InputPath / InputTransformer applied):
//   arn:aws:states:…:stateMachine:<name>   StartExecution (name <event id>_<uuid>)
//   arn:aws:logs:…:log-group:<name>        one log event per delivery
// Other target types are accepted and reported as undeliverable in the server log.

import (
	"encoding/json"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Event struct {
	ID         string
	Source     string
	DetailType string
	Detail     any
	Resources  []string
	Time       int64
}

func (a *App) eventDoc(e Event) M {
	res := []any{}
	for _, r := range e.Resources {
		res = append(res, r)
	}
	return M{"version": "0", "id": e.ID, "detail-type": e.DetailType, "source": e.Source, "account": a.cfg.Account,
		"time": time.UnixMilli(e.Time).UTC().Format("2006-01-02T15:04:05Z"), "region": a.cfg.Region, "resources": res, "detail": e.Detail}
}

type InputTransformer struct {
	InputPathsMap map[string]string `json:"InputPathsMap,omitempty"`
	InputTemplate string            `json:"InputTemplate"`
}

type Target struct {
	Id               string            `json:"Id"`
	Arn              string            `json:"Arn"`
	RoleArn          string            `json:"RoleArn,omitempty"`
	Input            string            `json:"Input,omitempty"`
	InputPath        string            `json:"InputPath,omitempty"`
	InputTransformer *InputTransformer `json:"InputTransformer,omitempty"`
}

type Rule struct {
	Name               string   `json:"Name"`
	EventPattern       string   `json:"EventPattern,omitempty"`
	ScheduleExpression string   `json:"ScheduleExpression,omitempty"`
	State              string   `json:"State"`
	Description        string   `json:"Description,omitempty"`
	Targets            []Target `json:"Targets"`
}

func (a *App) ruleArn(name string) string { return a.arn("events", "rule/"+name) }

func (a *App) putRule(r Rule) error {
	if r.EventPattern == "" && r.ScheduleExpression == "" {
		return errf(400, "ValidationException", "Parameter(s) EventPattern or ScheduleExpression must be specified.")
	}
	if r.EventPattern != "" {
		var p map[string]any
		if err := json.Unmarshal([]byte(r.EventPattern), &p); err != nil {
			return errf(400, "InvalidEventPatternException", "Event pattern is not valid. Reason: %v", err)
		}
	}
	if r.ScheduleExpression != "" {
		if _, ok := parseRate(r.ScheduleExpression); !ok {
			return errf(400, "ValidationException", "Parameter ScheduleExpression is not valid (localaws supports rate() expressions).")
		}
	}
	if r.State == "" {
		r.State = "ENABLED"
	}
	var old Rule
	if a.db.get("rule", r.Name, &old) {
		r.Targets = old.Targets
	}
	return a.db.put("rule", r.Name, r)
}

// ── delivery ─────────────────────────────────────────────────────────────
func (a *App) publish(e Event) {
	if e.ID == "" {
		e.ID = uuid4()
	}
	if e.Time == 0 {
		e.Time = nowMs()
	}
	doc := a.eventDoc(e)
	for _, r := range list[Rule](a.db, "rule") {
		if r.State != "ENABLED" || r.EventPattern == "" {
			continue
		}
		var pat map[string]any
		if json.Unmarshal([]byte(r.EventPattern), &pat) != nil || !matchPattern(pat, doc) {
			continue
		}
		a.deliver(r, e.ID, doc)
	}
}

func (a *App) deliver(r Rule, eventID string, doc M) {
	for _, t := range r.Targets {
		input, err := transformInput(t, doc)
		if err != nil {
			a.logf("events %s → %s: input transform failed: %v", r.Name, t.Id, err)
			continue
		}
		svc := strings.Split(t.Arn, ":")
		switch {
		case len(svc) > 5 && svc[2] == "states":
			name := eventID + "_" + uuid4()
			if _, e := a.startExecution(arnTail(t.Arn), name, input); e != nil {
				a.logf("events %s → %s: %v", r.Name, t.Arn, e)
			} else {
				a.logf("events %s → state machine %s (execution %s)", r.Name, arnTail(t.Arn), name)
			}
		case len(svc) > 5 && svc[2] == "logs":
			group := strings.TrimSuffix(strings.TrimPrefix(strings.Join(svc[5:], ":"), "log-group:"), ":*")
			a.ensureGroup(group)
			a.ensureStream(group, eventID)
			a.appendLog(group, eventID, input)
		default:
			a.logf("events %s → %s: target type not emulated — event dropped", r.Name, t.Arn)
		}
	}
}

func transformInput(t Target, doc M) (string, error) {
	switch {
	case t.Input != "":
		return t.Input, nil
	case t.InputPath != "":
		v, err := jsonPath(t.InputPath, doc)
		if err != nil {
			return "", err
		}
		return jsonStr(v), nil
	case t.InputTransformer != nil:
		vals := map[string]any{}
		for k, p := range t.InputTransformer.InputPathsMap {
			v, err := jsonPath(p, doc)
			if err != nil {
				v = nil
			}
			vals[k] = v
		}
		tpl := t.InputTransformer.InputTemplate
		out := regexp.MustCompile(`<([A-Za-z0-9_.-]+)>`).ReplaceAllStringFunc(tpl, func(m string) string {
			k := m[1 : len(m)-1]
			v, ok := vals[k]
			if !ok {
				if k == "aws.events.event.json" {
					return jsonStr(doc)
				}
				return m
			}
			if s, isStr := v.(string); isStr {
				// inside quotes in the template the raw string goes in; elsewhere the JSON value
				return strings.Trim(jsonStr(s), `"`)
			}
			return jsonStr(v)
		})
		return out, nil
	default:
		return jsonStr(doc), nil
	}
}

// ── schedules ────────────────────────────────────────────────────────────
var rateRe = regexp.MustCompile(`^rate\((\d+)\s+(minute|minutes|hour|hours|day|days)\)$`)

func parseRate(expr string) (time.Duration, bool) {
	m := rateRe.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"minute": time.Minute, "minutes": time.Minute, "hour": time.Hour, "hours": time.Hour,
		"day": 24 * time.Hour, "days": 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, n > 0
}

func (a *App) scheduler(stop <-chan struct{}) {
	last := map[string]time.Time{}
	var mu sync.Mutex
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			for _, r := range list[Rule](a.db, "rule") {
				if r.State != "ENABLED" || r.ScheduleExpression == "" {
					continue
				}
				every, ok := parseRate(r.ScheduleExpression)
				if !ok {
					continue
				}
				mu.Lock()
				prev, seen := last[r.Name]
				if !seen {
					last[r.Name] = now
					mu.Unlock()
					continue
				}
				due := now.Sub(prev) >= every
				if due {
					last[r.Name] = now
				}
				mu.Unlock()
				if due {
					e := Event{ID: uuid4(), Source: "aws.events", DetailType: "Scheduled Event", Detail: M{},
						Resources: []string{a.ruleArn(r.Name)}, Time: now.UnixMilli()}
					a.deliver(r, e.ID, a.eventDoc(e))
				}
			}
		}
	}
}

// ── event patterns ───────────────────────────────────────────────────────
func matchPattern(pat map[string]any, ev map[string]any) bool {
	for k, pv := range pat {
		if k == "$or" {
			alts, _ := pv.([]any)
			ok := false
			for _, alt := range alts {
				if am, isMap := alt.(map[string]any); isMap && matchPattern(am, ev) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
			continue
		}
		val, present := ev[k]
		switch p := pv.(type) {
		case map[string]any:
			sub, isMap := val.(map[string]any)
			if !present || !isMap || !matchPattern(p, sub) {
				return false
			}
		case []any:
			if !matchField(p, val, present) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// matchField: an array value in the event matches when any element matches.
func matchField(matchers []any, v any, present bool) bool {
	if arr, ok := v.([]any); ok && present {
		for _, x := range arr {
			if matchValue(matchers, x, true) {
				return true
			}
		}
		return len(arr) == 0 && matchValue(matchers, nil, false)
	}
	return matchValue(matchers, v, present)
}

func matchValue(matchers []any, v any, present bool) bool {
	for _, m := range matchers {
		switch x := m.(type) {
		case nil:
			if present && v == nil {
				return true
			}
		case string:
			if s, ok := v.(string); ok && present && s == x {
				return true
			}
		case float64:
			if n, ok := v.(float64); ok && present && n == x {
				return true
			}
		case bool:
			if b, ok := v.(bool); ok && present && b == x {
				return true
			}
		case map[string]any:
			if matchOp(x, v, present) {
				return true
			}
		}
	}
	return false
}

func matchOp(op map[string]any, v any, present bool) bool {
	s, isStr := v.(string)
	for name, arg := range op {
		switch name {
		case "exists":
			if b, _ := arg.(bool); b == present {
				return true
			}
		case "prefix":
			if am, ok := arg.(map[string]any); ok {
				if p := str(am["equals-ignore-case"]); isStr && strings.HasPrefix(strings.ToLower(s), strings.ToLower(p)) {
					return true
				}
			} else if isStr && strings.HasPrefix(s, str(arg)) {
				return true
			}
		case "suffix":
			if am, ok := arg.(map[string]any); ok {
				if p := str(am["equals-ignore-case"]); isStr && strings.HasSuffix(strings.ToLower(s), strings.ToLower(p)) {
					return true
				}
			} else if isStr && strings.HasSuffix(s, str(arg)) {
				return true
			}
		case "equals-ignore-case":
			if isStr && strings.EqualFold(s, str(arg)) {
				return true
			}
		case "wildcard":
			if isStr && wildcard(str(arg), s) {
				return true
			}
		case "anything-but":
			if !present {
				continue
			}
			switch x := arg.(type) {
			case []any:
				if !matchValue(x, v, true) {
					return true
				}
			case map[string]any:
				if !matchOp(x, v, true) {
					return true
				}
			default:
				if !matchValue([]any{x}, v, true) {
					return true
				}
			}
		case "numeric":
			n, ok := v.(float64)
			conds, _ := arg.([]any)
			if !ok || len(conds)%2 != 0 {
				continue
			}
			all := true
			for i := 0; i < len(conds); i += 2 {
				t, _ := conds[i+1].(float64)
				switch str(conds[i]) {
				case "=":
					all = all && n == t
				case "<":
					all = all && n < t
				case "<=":
					all = all && n <= t
				case ">":
					all = all && n > t
				case ">=":
					all = all && n >= t
				default:
					all = false
				}
			}
			if all {
				return true
			}
		case "cidr":
			if _, block, err := net.ParseCIDR(str(arg)); err == nil && isStr {
				if ip := net.ParseIP(s); ip != nil && block.Contains(ip) {
					return true
				}
			}
		}
	}
	return false
}

func wildcard(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if pattern[0] == '*' {
		for i := 0; i <= len(s); i++ {
			if wildcard(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	}
	return s != "" && s[0] == pattern[0] && wildcard(pattern[1:], s[1:])
}

// ── API ──────────────────────────────────────────────────────────────────
func (a *App) ruleOut(r Rule) M {
	m := M{"Name": r.Name, "Arn": a.ruleArn(r.Name), "State": r.State, "EventBusName": "default"}
	if r.EventPattern != "" {
		m["EventPattern"] = r.EventPattern
	}
	if r.ScheduleExpression != "" {
		m["ScheduleExpression"] = r.ScheduleExpression
	}
	if r.Description != "" {
		m["Description"] = r.Description
	}
	return m
}

func (a *App) eventsAPI(op string, in M) (any, *apiError) {
	getRule := func(name string) (Rule, *apiError) {
		var r Rule
		if !a.db.get("rule", name, &r) {
			return r, errf(400, "ResourceNotFoundException", "Rule %s does not exist on EventBus default.", name)
		}
		return r, nil
	}
	switch op {
	case "PutRule":
		r := Rule{Name: getStr(in, "Name"), EventPattern: getStr(in, "EventPattern"), ScheduleExpression: getStr(in, "ScheduleExpression"),
			State: getStr(in, "State"), Description: getStr(in, "Description")}
		if err := a.putRule(r); err != nil {
			if ae, ok := err.(*apiError); ok {
				return nil, ae
			}
			return nil, errf(500, "InternalException", "%v", err)
		}
		return M{"RuleArn": a.ruleArn(r.Name)}, nil
	case "DescribeRule":
		r, e := getRule(getStr(in, "Name"))
		if e != nil {
			return nil, e
		}
		return a.ruleOut(r), nil
	case "ListRules":
		out := []any{}
		for _, r := range list[Rule](a.db, "rule") {
			if p := getStr(in, "NamePrefix"); p == "" || strings.HasPrefix(r.Name, p) {
				out = append(out, a.ruleOut(r))
			}
		}
		page, next := pageOf(out, getStr(in, "NextToken"), getInt(in, "Limit", 100))
		resp := M{"Rules": page}
		if next != "" {
			resp["NextToken"] = next
		}
		return resp, nil
	case "ListRuleNamesByTarget":
		out := []any{}
		for _, r := range list[Rule](a.db, "rule") {
			for _, t := range r.Targets {
				if t.Arn == getStr(in, "TargetArn") {
					out = append(out, r.Name)
					break
				}
			}
		}
		return M{"RuleNames": out}, nil
	case "DeleteRule":
		r, e := getRule(getStr(in, "Name"))
		if e != nil {
			return M{}, nil
		}
		if len(r.Targets) > 0 && !getBool(in, "Force") {
			return nil, errf(400, "ValidationException", "Rule can't be deleted since it has targets.")
		}
		_ = a.db.del("rule", r.Name)
		return M{}, nil
	case "EnableRule", "DisableRule":
		r, e := getRule(getStr(in, "Name"))
		if e != nil {
			return nil, e
		}
		r.State = map[string]string{"EnableRule": "ENABLED", "DisableRule": "DISABLED"}[op]
		_ = a.db.put("rule", r.Name, r)
		return M{}, nil
	case "PutTargets":
		r, e := getRule(getStr(in, "Rule"))
		if e != nil {
			return nil, e
		}
		for _, x := range getList(in, "Targets") {
			b, _ := json.Marshal(x)
			var t Target
			_ = json.Unmarshal(b, &t)
			replaced := false
			for i := range r.Targets {
				if r.Targets[i].Id == t.Id {
					r.Targets[i], replaced = t, true
				}
			}
			if !replaced {
				r.Targets = append(r.Targets, t)
			}
		}
		_ = a.db.put("rule", r.Name, r)
		return M{"FailedEntryCount": 0, "FailedEntries": []any{}}, nil
	case "RemoveTargets":
		r, e := getRule(getStr(in, "Rule"))
		if e != nil {
			return nil, e
		}
		drop := map[string]bool{}
		for _, id := range strList(getList(in, "Ids")) {
			drop[id] = true
		}
		kept := r.Targets[:0]
		for _, t := range r.Targets {
			if !drop[t.Id] {
				kept = append(kept, t)
			}
		}
		r.Targets = kept
		_ = a.db.put("rule", r.Name, r)
		return M{"FailedEntryCount": 0, "FailedEntries": []any{}}, nil
	case "ListTargetsByRule":
		r, e := getRule(getStr(in, "Rule"))
		if e != nil {
			return nil, e
		}
		out := []any{}
		for _, t := range r.Targets {
			out = append(out, t)
		}
		return M{"Targets": out}, nil
	case "PutEvents":
		entries := []any{}
		for _, x := range getList(in, "Entries") {
			em, _ := x.(map[string]any)
			var detail any = M{}
			if d := getStr(em, "Detail"); d != "" {
				if err := json.Unmarshal([]byte(d), &detail); err != nil {
					entries = append(entries, M{"ErrorCode": "MalformedDetail", "ErrorMessage": "Detail is malformed."})
					continue
				}
			}
			ev := Event{ID: uuid4(), Source: getStr(em, "Source"), DetailType: getStr(em, "DetailType"), Detail: detail,
				Resources: strList(getList(em, "Resources"))}
			go a.publish(ev)
			entries = append(entries, M{"EventId": ev.ID})
		}
		return M{"Entries": entries, "FailedEntryCount": 0}, nil
	case "TestEventPattern":
		var pat map[string]any
		var ev map[string]any
		if err := json.Unmarshal([]byte(getStr(in, "EventPattern")), &pat); err != nil {
			return nil, errf(400, "InvalidEventPatternException", "Event pattern is not valid. Reason: %v", err)
		}
		if err := json.Unmarshal([]byte(getStr(in, "Event")), &ev); err != nil {
			return nil, errf(400, "ValidationException", "Parameter Event is not valid. Reason: %v", err)
		}
		return M{"Result": matchPattern(pat, ev)}, nil
	case "ListEventBuses":
		return M{"EventBuses": []any{M{"Name": "default", "Arn": a.arn("events", "event-bus/default")}}}, nil
	case "DescribeEventBus":
		return M{"Name": "default", "Arn": a.arn("events", "event-bus/default")}, nil
	}
	return nil, errf(400, "UnknownOperationException", "localaws events: %s is not implemented", op)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
