package main

// Step Functions API + execution runtime. State machines are interpreted state by state
// (see asl.go for the data flow); every step is written to the execution history with
// the same event types, details and previousEventId chain the console shows, so tools
// that read real histories read these unchanged.
//
// Task integrations: ecs:runTask(.sync), states:startExecution(.sync/.sync:2),
// events:putEvents, sns:publish (accepted). Anything else fails the state with
// States.Runtime, saying so.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type StateMachine struct {
	Name       string `json:"name"`
	Arn        string `json:"arn"`
	Type       string `json:"type"`
	RoleArn    string `json:"roleArn"`
	Definition string `json:"definition"`
	Created    int64  `json:"created"`
	Updated    int64  `json:"updated"`
}

type Execution struct {
	Arn     string `json:"arn"`
	SmArn   string `json:"smArn"`
	SmName  string `json:"smName"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Input   string `json:"input"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
	Cause   string `json:"cause,omitempty"`
	Start   int64  `json:"start"`
	Stop    int64  `json:"stop,omitempty"`
	Parent  string `json:"parent,omitempty"`
	LastID  int64  `json:"lastId"`
	Express bool   `json:"express,omitempty"`
}

type stateErr struct {
	Name, Cause string
	fromTask    bool
}

func (e *stateErr) Error() string { return e.Name + ": " + e.Cause }

func (a *App) smArn(name string) string { return a.arn("states", "stateMachine:"+name) }

func (a *App) createStateMachine(sm StateMachine) *apiError {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`).MatchString(sm.Name) {
		return errf(400, "InvalidName", "Invalid Name: '%s'", sm.Name)
	}
	var def map[string]any
	if err := json.Unmarshal([]byte(sm.Definition), &def); err != nil {
		return errf(400, "InvalidDefinition", "Invalid State Machine Definition: 'INVALID_JSON_DESCRIPTION: %v'", err)
	}
	if errs := validateDefinition(def, "/"); len(errs) > 0 {
		sort.Strings(errs)
		return errf(400, "InvalidDefinition", "Invalid State Machine Definition: '%s'", strings.Join(errs, ", "))
	}
	if sm.Type == "" {
		sm.Type = "STANDARD"
	}
	sm.Arn = a.smArn(sm.Name)
	if sm.Created == 0 {
		sm.Created = nowMs()
	}
	sm.Updated = nowMs()
	a.logf("states state machine %s (%s) ready", sm.Name, sm.Type)
	_ = a.db.put("statemachine", sm.Name, sm)
	return nil
}

func (a *App) getSM(ref string) (StateMachine, *apiError) {
	var sm StateMachine
	if !a.db.get("statemachine", arnTail(ref), &sm) {
		return sm, errf(400, "StateMachineDoesNotExist", "State Machine Does Not Exist: '%s'", ref)
	}
	return sm, nil
}

func (a *App) getExec(arn string) (Execution, bool) {
	var e Execution
	ok := a.db.get("execution", arn, &e)
	return e, ok
}

// ── execution lifecycle ──────────────────────────────────────────────────
type run struct {
	a      *App
	mu     sync.Mutex
	e      Execution
	sm     StateMachine
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *run) save() {
	r.mu.Lock()
	e := r.e
	r.mu.Unlock()
	_ = r.a.db.put("execution", e.Arn, e)
}

func (r *run) event(typ string, prev int64, details M) int64 {
	r.mu.Lock()
	r.e.LastID++
	id := r.e.LastID
	r.mu.Unlock()
	ts := nowMs()
	ev := M{"id": id, "previousEventId": prev, "timestamp": epoch(ts), "type": typ}
	for k, v := range details {
		ev[k] = v
	}
	_ = r.a.db.exec(`INSERT INTO sfn_events(exec_arn, id, ts_ms, doc) VALUES(?,?,?,?)`, r.e.Arn, id, ts, jsonStr(ev))
	return id
}

func (a *App) startExecution(smRef, name, input string) (Execution, *apiError) {
	return a.startExecutionParent(smRef, name, input, "")
}

func (a *App) startExecutionParent(smRef, name, input, parent string) (Execution, *apiError) {
	sm, e := a.getSM(smRef)
	if e != nil {
		return Execution{}, e
	}
	if name == "" {
		name = uuid4()
	}
	if !regexp.MustCompile(`^[^\s<>{}\[\]?*"#%\\^|~` + "`" + `$&,;:/]{1,80}$`).MatchString(name) {
		return Execution{}, errf(400, "InvalidName", "Invalid Name: '%s'", name)
	}
	if input == "" {
		input = "{}"
	}
	var parsed any
	if json.Unmarshal([]byte(input), &parsed) != nil {
		return Execution{}, errf(400, "InvalidExecutionInput", "Invalid execution input: not valid JSON")
	}
	arn := a.arn("states", "execution:"+sm.Name+":"+name)
	if sm.Type == "EXPRESS" {
		arn = a.arn("states", "express:"+sm.Name+":"+name+":"+uuid4())
	}
	if _, exists := a.getExec(arn); exists {
		return Execution{}, errf(400, "ExecutionAlreadyExists", "Execution Already Exists: '%s'", arn)
	}
	ex := Execution{Arn: arn, SmArn: sm.Arn, SmName: sm.Name, Name: name, Status: "RUNNING", Input: input, Start: nowMs(), Parent: parent,
		Express: sm.Type == "EXPRESS"}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{a: a, e: ex, sm: sm, ctx: ctx, cancel: cancel}
	r.save()
	a.runsMu.Lock()
	a.runs[arn] = r
	a.runsMu.Unlock()
	go r.execute(parsed)
	return ex, nil
}

func (r *run) finish(status, output, errName, cause string) {
	r.mu.Lock()
	if r.e.Status != "RUNNING" {
		r.mu.Unlock()
		return
	}
	r.e.Status, r.e.Output, r.e.Error, r.e.Cause, r.e.Stop = status, output, errName, cause, nowMs()
	r.mu.Unlock()
	r.save()
	r.a.runsMu.Lock()
	delete(r.a.runs, r.e.Arn)
	r.a.runsMu.Unlock()
	r.cancel()
	r.a.logf("states %s/%s → %s %s", r.e.SmName, r.e.Name, status, errName)
}

func (r *run) execute(input any) {
	var def map[string]any
	_ = json.Unmarshal([]byte(r.sm.Definition), &def)
	first := r.event("ExecutionStarted", 0, M{"executionStartedEventDetails": M{"input": r.e.Input,
		"inputDetails": M{"truncated": false}, "roleArn": r.sm.RoleArn}})
	if t := getInt(def, "TimeoutSeconds", 0); t > 0 {
		var cancel context.CancelFunc
		r.ctx, cancel = context.WithTimeout(r.ctx, r.a.scaled(time.Duration(t)*time.Second))
		defer cancel()
	}
	ctxObj := M{"Execution": M{"Id": r.e.Arn, "Input": input, "Name": r.e.Name, "RoleArn": r.sm.RoleArn,
		"StartTime": isoMs(r.e.Start)}, "StateMachine": M{"Id": r.sm.Arn, "Name": r.sm.Name}}
	out, serr, last := r.states(def, input, ctxObj, 0)
	_ = first
	switch {
	case r.ctx.Err() == context.DeadlineExceeded:
		r.event("ExecutionTimedOut", last, M{"executionTimedOutEventDetails": M{"error": "States.Timeout", "cause": "The execution timed out."}})
		r.finish("TIMED_OUT", "", "States.Timeout", "The execution timed out.")
	case r.ctx.Err() != nil:
		r.finish("ABORTED", "", r.e.Error, r.e.Cause) // StopExecution wrote the event
	case serr != nil:
		r.event("ExecutionFailed", last, M{"executionFailedEventDetails": M{"error": serr.Name, "cause": serr.Cause}})
		r.finish("FAILED", "", serr.Name, serr.Cause)
	default:
		r.event("ExecutionSucceeded", last, M{"executionSucceededEventDetails": M{"output": jsonStr(out), "outputDetails": M{"truncated": false}}})
		r.finish("SUCCEEDED", jsonStr(out), "", "")
	}
}

// states runs one state graph (the machine, a Parallel branch or a Map iteration).
func (r *run) states(def map[string]any, input any, ctxObj M, prev int64) (any, *stateErr, int64) {
	states, _ := def["States"].(map[string]any)
	cur := str(def["StartAt"])
	data := input
	for {
		if r.ctx.Err() != nil {
			return nil, &stateErr{Name: "States.Aborted", Cause: "execution stopped"}, prev
		}
		st, _ := states[cur].(map[string]any)
		if st == nil {
			return nil, &stateErr{Name: "States.Runtime", Cause: "state " + cur + " does not exist"}, prev
		}
		out, next, serr, last := r.state(cur, st, data, ctxObj, prev)
		prev = last
		if serr != nil {
			return nil, serr, prev
		}
		data = out
		if next == "" {
			return data, nil, prev
		}
		cur = next
	}
}

var enteredType = map[string]string{"Task": "TaskStateEntered", "Pass": "PassStateEntered", "Choice": "ChoiceStateEntered",
	"Wait": "WaitStateEntered", "Succeed": "SucceedStateEntered", "Fail": "FailStateEntered", "Parallel": "ParallelStateEntered", "Map": "MapStateEntered"}

func (r *run) state(name string, st map[string]any, raw any, ctxObj M, prev int64) (out any, next string, serr *stateErr, last int64) {
	typ := str(st["Type"])
	entered := r.event(enteredType[typ], prev, M{"stateEnteredEventDetails": M{"name": name, "input": jsonStr(raw), "inputDetails": M{"truncated": false}}})
	last = entered
	ctxObj = copyCtx(ctxObj)
	ctxObj["State"] = M{"Name": name, "EnteredTime": isoMs(nowMs()), "RetryCount": 0}
	exited := func(output any) {
		last = r.event(strings.TrimSuffix(enteredType[typ], "Entered")+"Exited", last,
			M{"stateExitedEventDetails": M{"name": name, "output": jsonStr(output), "outputDetails": M{"truncated": false}}})
	}
	fail := func(e *stateErr) (any, string, *stateErr, int64) { return nil, "", e, last }
	// InputPath
	eff := raw
	if p, has := st["InputPath"]; has {
		if p == nil {
			eff = map[string]any{}
		} else if v, err := pathIn(str(p), raw, ctxObj); err != nil {
			return fail(&stateErr{"States.Runtime", "An error occurred while executing the state '" + name + "'. Invalid path '" + str(p) + "' : " + err.Error(), false})
		} else {
			eff = v
		}
	}
	nextOf := func() string {
		if e, _ := st["End"].(bool); e {
			return ""
		}
		return str(st["Next"])
	}
	// result → ResultSelector → ResultPath → OutputPath
	finishWith := func(result any) (any, string, *stateErr, int64) {
		if sel, has := st["ResultSelector"]; has {
			v, err := resolveTemplate(sel, result, ctxObj)
			if err != nil {
				return fail(&stateErr{"States.Runtime", err.Error(), false})
			}
			result = v
		}
		merged := result
		if rp, has := st["ResultPath"]; has {
			if rp == nil {
				merged = raw
			} else if v, err := setPath(raw, str(rp), result); err != nil {
				return fail(&stateErr{"States.Runtime", err.Error(), false})
			} else {
				merged = v
			}
		}
		output := merged
		if op, has := st["OutputPath"]; has {
			if op == nil {
				output = map[string]any{}
			} else if v, err := pathIn(str(op), merged, ctxObj); err != nil {
				return fail(&stateErr{"States.Runtime", err.Error(), false})
			} else {
				output = v
			}
		}
		exited(output)
		return output, nextOf(), nil, last
	}
	params := func(in any) (any, *stateErr) {
		if p, has := st["Parameters"]; has {
			v, err := resolveTemplate(p, in, ctxObj)
			if err != nil {
				return nil, &stateErr{"States.Runtime", "An error occurred while executing the state '" + name + "'. " + err.Error(), false}
			}
			return v, nil
		}
		return in, nil
	}
	switch typ {
	case "Pass":
		in, e := params(eff)
		if e != nil {
			return fail(e)
		}
		if res, has := st["Result"]; has {
			in = res
		}
		return finishWith(in)
	case "Succeed":
		output := eff
		if op, has := st["OutputPath"]; has && op != nil {
			if v, err := pathIn(str(op), eff, ctxObj); err == nil {
				output = v
			}
		}
		exited(output)
		return output, "", nil, last
	case "Fail":
		errName, cause := str(st["Error"]), str(st["Cause"])
		if p := str(st["ErrorPath"]); p != "" {
			if v, err := pathIn(p, eff, ctxObj); err == nil {
				errName = str(v)
			}
		}
		if p := str(st["CausePath"]); p != "" {
			if v, err := pathIn(p, eff, ctxObj); err == nil {
				cause = str(v)
			}
		}
		return fail(&stateErr{errName, cause, false})
	case "Wait":
		d := time.Duration(0)
		switch {
		case st["Seconds"] != nil:
			d = time.Duration(getInt(st, "Seconds", 0)) * time.Second
		case st["SecondsPath"] != nil:
			v, err := pathIn(str(st["SecondsPath"]), eff, ctxObj)
			if err != nil {
				return fail(&stateErr{"States.Runtime", err.Error(), false})
			}
			n, _ := v.(float64)
			d = time.Duration(n) * time.Second
		case st["Timestamp"] != nil || st["TimestampPath"] != nil:
			var tv any = st["Timestamp"]
			if p := str(st["TimestampPath"]); p != "" {
				v, err := pathIn(p, eff, ctxObj)
				if err != nil {
					return fail(&stateErr{"States.Runtime", err.Error(), false})
				}
				tv = v
			}
			if t, ok := parseTS(tv); ok {
				d = time.Until(t)
			}
		}
		if !r.a.sleep(r.ctx, d) {
			return fail(&stateErr{"States.Aborted", "execution stopped", false})
		}
		output := eff
		if op, has := st["OutputPath"]; has && op != nil {
			if v, err := pathIn(str(op), eff, ctxObj); err == nil {
				output = v
			}
		}
		exited(output)
		return output, nextOf(), nil, last
	case "Choice":
		for _, c := range getList(st, "Choices") {
			cm, _ := c.(map[string]any)
			ok, err := evalChoice(cm, eff, ctxObj)
			if err != nil {
				return fail(&stateErr{"States.Runtime", "An error occurred while executing the state '" + name + "'. " + err.Error(), false})
			}
			if ok {
				exited(eff)
				return eff, str(cm["Next"]), nil, last
			}
		}
		if d := str(st["Default"]); d != "" {
			exited(eff)
			return eff, d, nil, last
		}
		return fail(&stateErr{"States.NoChoiceMatched", "No Matches!", false})
	case "Task", "Parallel", "Map":
		attempts := map[int]int{}
		for {
			in, e := params(eff)
			var result any
			if e == nil {
				switch typ {
				case "Task":
					result, e, last = r.task(name, st, in, last)
				case "Parallel":
					result, e, last = r.parallel(st, in, ctxObj, last)
				case "Map":
					result, e, last = r.mapState(st, eff, ctxObj, last)
				}
			}
			if e == nil {
				return finishWith(result)
			}
			if e.Name == "States.Aborted" {
				return fail(e)
			}
			if wait, ok := retryDelay(st, e, attempts); ok {
				r.a.logf("states %s/%s: %s failed with %s, retrying in %s", r.e.SmName, r.e.Name, name, e.Name, wait)
				if !r.a.sleep(r.ctx, wait) {
					return fail(&stateErr{"States.Aborted", "execution stopped", false})
				}
				continue
			}
			for _, c := range getList(st, "Catch") {
				cm, _ := c.(map[string]any)
				if !errorMatches(strList(getList(cm, "ErrorEquals")), e) {
					continue
				}
				errOut := M{"Error": e.Name, "Cause": e.Cause}
				output := any(errOut)
				if rp, has := cm["ResultPath"]; has {
					if rp == nil {
						output = raw
					} else if v, err := setPath(raw, str(rp), errOut); err == nil {
						output = v
					}
				}
				exited(output)
				return output, str(cm["Next"]), nil, last
			}
			return fail(e)
		}
	}
	return fail(&stateErr{"States.Runtime", "unsupported state type " + typ, false})
}

func copyCtx(c M) M {
	out := M{}
	for k, v := range c {
		out[k] = v
	}
	return out
}

func errorMatches(list []string, e *stateErr) bool {
	for _, x := range list {
		switch {
		case x == e.Name:
			return true
		case x == "States.ALL" && e.Name != "States.Runtime" && e.Name != "States.DataLimitExceeded":
			return true
		case x == "States.TaskFailed" && e.fromTask && e.Name != "States.Timeout":
			return true
		}
	}
	return false
}

func retryDelay(st map[string]any, e *stateErr, attempts map[int]int) (time.Duration, bool) {
	for i, x := range getList(st, "Retry") {
		rm, _ := x.(map[string]any)
		if !errorMatches(strList(getList(rm, "ErrorEquals")), e) {
			continue
		}
		max := getInt(rm, "MaxAttempts", 3)
		if attempts[i] >= max {
			return 0, false
		}
		iv := float64(getInt(rm, "IntervalSeconds", 1))
		rate := 2.0
		if b, ok := rm["BackoffRate"].(float64); ok {
			rate = b
		}
		d := iv
		for n := 0; n < attempts[i]; n++ {
			d *= rate
		}
		if mx := getInt(rm, "MaxDelaySeconds", 0); mx > 0 && d > float64(mx) {
			d = float64(mx)
		}
		if str(rm["JitterStrategy"]) == "FULL" {
			d = rand.Float64() * d
		}
		attempts[i]++
		return time.Duration(d * float64(time.Second)), true
	}
	return 0, false
}

// ── Parallel / Map ───────────────────────────────────────────────────────
func (r *run) parallel(st map[string]any, in any, ctxObj M, prev int64) (any, *stateErr, int64) {
	started := r.event("ParallelStateStarted", prev, M{})
	branches := getList(st, "Branches")
	out := make([]any, len(branches))
	errs := make([]*stateErr, len(branches))
	var wg sync.WaitGroup
	for i, b := range branches {
		bm, _ := b.(map[string]any)
		wg.Add(1)
		go func(i int, bm map[string]any) {
			defer wg.Done()
			out[i], errs[i], _ = r.states(bm, in, ctxObj, started)
		}(i, bm)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e, r.event("ParallelStateFailed", started, M{})
		}
	}
	return out, nil, r.event("ParallelStateSucceeded", started, M{})
}

func (r *run) mapState(st map[string]any, eff any, ctxObj M, prev int64) (any, *stateErr, int64) {
	items := eff
	if p := str(st["ItemsPath"]); p != "" {
		v, err := pathIn(p, eff, ctxObj)
		if err != nil {
			return nil, &stateErr{"States.Runtime", err.Error(), false}, prev
		}
		items = v
	}
	list, ok := items.([]any)
	if !ok {
		return nil, &stateErr{"States.Runtime", "Map ItemsPath must point to an array", false}, prev
	}
	proc := getMap(st, "ItemProcessor")
	if len(proc) == 0 {
		proc = getMap(st, "Iterator")
	}
	sel, hasSel := st["ItemSelector"]
	if !hasSel {
		sel, hasSel = st["Parameters"]
	}
	started := r.event("MapStateStarted", prev, M{"mapStateStartedEventDetails": M{"length": len(list)}})
	conc := getInt(st, "MaxConcurrency", 0)
	if conc <= 0 || conc > 40 {
		conc = 40
	}
	sem := make(chan struct{}, conc)
	out := make([]any, len(list))
	errs := make([]*stateErr, len(list))
	var wg sync.WaitGroup
	for i, item := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item any) {
			defer wg.Done()
			defer func() { <-sem }()
			ictx := copyCtx(ctxObj)
			ictx["Map"] = M{"Item": M{"Index": i, "Value": item}}
			in := item
			if hasSel {
				v, err := resolveTemplate(sel, eff, ictx)
				if err != nil {
					errs[i] = &stateErr{"States.Runtime", err.Error(), false}
					return
				}
				in = v
			}
			det := M{"mapIterationStartedEventDetails": M{"name": "map", "index": i}}
			it := r.event("MapIterationStarted", started, det)
			out[i], errs[i], _ = r.states(proc, in, ictx, it)
			if errs[i] != nil {
				r.event("MapIterationFailed", it, M{"mapIterationFailedEventDetails": M{"name": "map", "index": i}})
			} else {
				r.event("MapIterationSucceeded", it, M{"mapIterationSucceededEventDetails": M{"name": "map", "index": i}})
			}
		}(i, item)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e, r.event("MapStateFailed", started, M{})
		}
	}
	return out, nil, r.event("MapStateSucceeded", started, M{})
}

// ── Task integrations ────────────────────────────────────────────────────
func (r *run) task(name string, st map[string]any, params any, prev int64) (any, *stateErr, int64) {
	resource := str(st["Resource"])
	short := strings.TrimPrefix(resource, "arn:aws:states:::")
	svc, action, _ := strings.Cut(short, ":")
	det := func(extra M) M {
		m := M{"resourceType": svc, "resource": action}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	ctx := r.ctx
	if t := getInt(st, "TimeoutSeconds", 0); t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.a.scaled(time.Duration(t)*time.Second))
		defer cancel()
	}
	sched := r.event("TaskScheduled", prev, M{"taskScheduledEventDetails": det(M{"region": r.a.cfg.Region, "parameters": jsonStr(params)})})
	startedID := r.event("TaskStarted", sched, M{"taskStartedEventDetails": det(nil)})
	failed := func(e *stateErr, prev int64) (any, *stateErr, int64) {
		e.fromTask = true
		if e.Name == "States.Timeout" {
			return nil, e, r.event("TaskTimedOut", prev, M{"taskTimedOutEventDetails": det(M{"error": e.Name, "cause": e.Cause})})
		}
		return nil, e, r.event("TaskFailed", prev, M{"taskFailedEventDetails": det(M{"error": e.Name, "cause": e.Cause})})
	}
	succeeded := func(out any, prev int64) (any, *stateErr, int64) {
		return out, nil, r.event("TaskSucceeded", prev, M{"taskSucceededEventDetails": det(M{"output": jsonStr(out), "outputDetails": M{"truncated": false}})})
	}
	pm, _ := params.(map[string]any)
	switch {
	case svc == "ecs" && strings.HasPrefix(action, "runTask"):
		t, err := r.a.runTask(pm, "AWS Step Functions", r.e.Arn)
		if err != nil {
			return failed(&stateErr{"ECS.AmazonECSException", err.Msg, true}, startedID)
		}
		if action == "runTask" {
			return succeeded(M{"Tasks": []any{r.a.taskPascal(t)}, "Failures": []any{}}, startedID)
		}
		sub := r.event("TaskSubmitted", startedID, M{"taskSubmittedEventDetails": det(M{"output": jsonStr(M{"Tasks": []any{r.a.taskPascal(t)},
			"Failures": []any{}, "SdkHttpMetadata": M{"HttpStatusCode": 200}}), "outputDetails": M{"truncated": false}})})
		select {
		case <-r.a.taskDone(t.ID):
		case <-ctx.Done():
			r.a.stopTask(t.ID, "Step Functions execution stopped")
			<-r.a.taskDone(t.ID)
			if ctx.Err() == context.DeadlineExceeded && r.ctx.Err() == nil {
				return failed(&stateErr{"States.Timeout", "Task timed out", true}, sub)
			}
			return nil, &stateErr{"States.Aborted", "execution stopped", true}, sub
		}
		t = r.a.task(t.ID)
		out := r.a.taskPascal(t)
		if t.allEssentialZero() {
			return succeeded(out, sub)
		}
		return failed(&stateErr{"States.TaskFailed", jsonStr(out), true}, sub)
	case svc == "states" && strings.HasPrefix(action, "startExecution"):
		child, ae := r.a.startExecutionParent(str(pm["StateMachineArn"]), str(pm["Name"]), inputString(pm["Input"]), r.e.Arn)
		if ae != nil {
			return failed(&stateErr{"StepFunctions." + ae.Type, ae.Msg, true}, startedID)
		}
		if action == "startExecution" {
			return succeeded(M{"ExecutionArn": child.Arn, "StartDate": isoMs(child.Start)}, startedID)
		}
		sub := r.event("TaskSubmitted", startedID, M{"taskSubmittedEventDetails": det(M{"output": jsonStr(M{"ExecutionArn": child.Arn, "StartDate": isoMs(child.Start)})})})
		done := r.a.waitExecution(ctx, child.Arn)
		if done.Status == "RUNNING" {
			r.a.stopExecution(child.Arn, "States.Aborted", "parent stopped")
			return nil, &stateErr{"States.Aborted", "execution stopped", true}, sub
		}
		desc := describeExec(done)
		if action == "startExecution.sync:2" {
			var o any
			_ = json.Unmarshal([]byte(done.Output), &o)
			desc["Output"] = o
		}
		if done.Status == "SUCCEEDED" {
			return succeeded(pascalKeys(desc), sub)
		}
		return failed(&stateErr{"States.TaskFailed", jsonStr(pascalKeys(desc)), true}, sub)
	case svc == "events" && action == "putEvents":
		entries := []any{}
		for _, x := range getList(pm, "Entries") {
			em, _ := x.(map[string]any)
			var detail any = em["Detail"]
			if s, ok := detail.(string); ok {
				_ = json.Unmarshal([]byte(s), &detail)
			}
			ev := Event{ID: uuid4(), Source: str(em["Source"]), DetailType: str(em["DetailType"]), Detail: detail, Resources: strList(getList(em, "Resources"))}
			go r.a.publish(ev)
			entries = append(entries, M{"EventId": ev.ID})
		}
		return succeeded(M{"Entries": entries, "FailedEntryCount": 0}, startedID)
	case svc == "sns" && action == "publish":
		r.a.logf("states %s: sns:publish to %s accepted (not delivered)", r.e.Name, str(pm["TopicArn"]))
		return succeeded(M{"MessageId": uuid4()}, startedID)
	}
	return failed(&stateErr{"States.Runtime", fmt.Sprintf("localaws: the resource %s is not emulated", resource), false}, startedID)
}

func inputString(v any) string {
	switch x := v.(type) {
	case nil:
		return "{}"
	case string:
		return x
	}
	return jsonStr(v)
}

func pascalKeys(m M) M {
	out := M{}
	for k, v := range m {
		out[strings.ToUpper(k[:1])+k[1:]] = v
	}
	return out
}

func (a *App) waitExecution(ctx context.Context, arn string) Execution {
	for {
		e, _ := a.getExec(arn)
		if e.Status != "RUNNING" {
			return e
		}
		select {
		case <-ctx.Done():
			return e
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (a *App) stopExecution(arn, errName, cause string) {
	a.runsMu.Lock()
	r := a.runs[arn]
	a.runsMu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.e.Error, r.e.Cause = errName, cause
	last := r.e.LastID
	r.mu.Unlock()
	r.event("ExecutionAborted", last, M{"executionAbortedEventDetails": M{"error": errName, "cause": cause}})
	r.cancel()
	for _, t := range a.tasksOf(arn) {
		a.stopTask(t.ID, "Step Functions execution stopped")
	}
}

// ── API ──────────────────────────────────────────────────────────────────
func describeExec(e Execution) M {
	m := M{"executionArn": e.Arn, "stateMachineArn": e.SmArn, "name": e.Name, "status": e.Status, "startDate": epoch(e.Start),
		"input": e.Input, "inputDetails": M{"included": true}, "redriveCount": 0, "redriveStatus": "NOT_REDRIVABLE"}
	if e.Stop > 0 {
		m["stopDate"] = epoch(e.Stop)
	}
	if e.Output != "" {
		m["output"], m["outputDetails"] = e.Output, M{"included": true}
	}
	if e.Error != "" || e.Cause != "" {
		m["error"], m["cause"] = e.Error, e.Cause
	}
	return m
}

func (a *App) history(arn string) []M {
	rows, err := a.db.db.Query(`SELECT doc FROM sfn_events WHERE exec_arn=? ORDER BY id`, arn)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []M
	for rows.Next() {
		var doc string
		_ = rows.Scan(&doc)
		var m M
		if json.Unmarshal([]byte(doc), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (a *App) smOut(sm StateMachine) M {
	return M{"stateMachineArn": sm.Arn, "name": sm.Name, "status": "ACTIVE", "definition": sm.Definition, "roleArn": sm.RoleArn,
		"type": sm.Type, "creationDate": epoch(sm.Created), "loggingConfiguration": M{"level": "OFF", "includeExecutionData": false},
		"tracingConfiguration": M{"enabled": false}, "revisionId": fmt.Sprint(sm.Updated)}
}

func (a *App) sfnAPI(op string, in M) (any, *apiError) {
	switch op {
	case "CreateStateMachine":
		def := getStr(in, "definition")
		sm := StateMachine{Name: getStr(in, "name"), Type: getStr(in, "type"), RoleArn: getStr(in, "roleArn"), Definition: def}
		var old StateMachine
		if a.db.get("statemachine", sm.Name, &old) {
			if old.Definition == def && old.RoleArn == sm.RoleArn {
				return M{"stateMachineArn": old.Arn, "creationDate": epoch(old.Created)}, nil
			}
			return nil, errf(400, "StateMachineAlreadyExists", "State Machine Already Exists: '%s'", old.Arn)
		}
		if e := a.createStateMachine(sm); e != nil {
			return nil, e
		}
		sm, _ = a.getSM(sm.Name)
		return M{"stateMachineArn": sm.Arn, "creationDate": epoch(sm.Created)}, nil
	case "UpdateStateMachine":
		sm, e := a.getSM(getStr(in, "stateMachineArn"))
		if e != nil {
			return nil, e
		}
		if d := getStr(in, "definition"); d != "" {
			sm.Definition = d
		}
		if ra := getStr(in, "roleArn"); ra != "" {
			sm.RoleArn = ra
		}
		if e := a.createStateMachine(sm); e != nil {
			return nil, e
		}
		return M{"updateDate": epoch(nowMs())}, nil
	case "DeleteStateMachine":
		_ = a.db.del("statemachine", arnTail(getStr(in, "stateMachineArn")))
		return M{}, nil
	case "DescribeStateMachine":
		sm, e := a.getSM(getStr(in, "stateMachineArn"))
		if e != nil {
			return nil, e
		}
		return a.smOut(sm), nil
	case "DescribeStateMachineForExecution":
		ex, ok := a.getExec(getStr(in, "executionArn"))
		if !ok {
			return nil, errf(400, "ExecutionDoesNotExist", "Execution Does Not Exist: '%s'", getStr(in, "executionArn"))
		}
		sm, e := a.getSM(ex.SmName)
		if e != nil {
			return nil, e
		}
		m := a.smOut(sm)
		m["updateDate"] = epoch(sm.Updated)
		return m, nil
	case "ListStateMachines":
		out := []any{}
		for _, sm := range list[StateMachine](a.db, "statemachine") {
			out = append(out, M{"stateMachineArn": sm.Arn, "name": sm.Name, "type": sm.Type, "creationDate": epoch(sm.Created)})
		}
		page, next := pageOf(out, getStr(in, "nextToken"), getInt(in, "maxResults", 100))
		resp := M{"stateMachines": page}
		if next != "" {
			resp["nextToken"] = next
		}
		return resp, nil
	case "ValidateStateMachineDefinition":
		var def map[string]any
		diags := []any{}
		if err := json.Unmarshal([]byte(getStr(in, "definition")), &def); err != nil {
			diags = append(diags, M{"severity": "ERROR", "code": "INVALID_JSON_DESCRIPTION", "message": err.Error()})
		} else {
			for _, e := range validateDefinition(def, "/") {
				code, msg, _ := strings.Cut(e, ": ")
				diags = append(diags, M{"severity": "ERROR", "code": code, "message": msg})
			}
		}
		res := "OK"
		if len(diags) > 0 {
			res = "FAIL"
		}
		return M{"result": res, "diagnostics": diags, "truncated": false}, nil
	case "StartExecution":
		e, ae := a.startExecution(getStr(in, "stateMachineArn"), getStr(in, "name"), getStr(in, "input"))
		if ae != nil {
			return nil, ae
		}
		return M{"executionArn": e.Arn, "startDate": epoch(e.Start)}, nil
	case "StartSyncExecution":
		e, ae := a.startExecution(getStr(in, "stateMachineArn"), getStr(in, "name"), getStr(in, "input"))
		if ae != nil {
			return nil, ae
		}
		done := a.waitExecution(context.Background(), e.Arn)
		m := describeExec(done)
		delete(m, "redriveCount")
		delete(m, "redriveStatus")
		return m, nil
	case "DescribeExecution":
		e, ok := a.getExec(getStr(in, "executionArn"))
		if !ok {
			return nil, errf(400, "ExecutionDoesNotExist", "Execution Does Not Exist: '%s'", getStr(in, "executionArn"))
		}
		return describeExec(e), nil
	case "ListExecutions":
		smArn, status := getStr(in, "stateMachineArn"), getStr(in, "statusFilter")
		if smArn != "" {
			if _, e := a.getSM(smArn); e != nil {
				return nil, e
			}
		}
		var execs []Execution
		for _, e := range list[Execution](a.db, "execution") {
			if (smArn == "" || e.SmArn == smArn || e.SmName == arnTail(smArn)) && (status == "" || e.Status == status) && !e.Express {
				execs = append(execs, e)
			}
		}
		sort.Slice(execs, func(i, j int) bool { return execs[i].Start > execs[j].Start })
		page, next := pageOf(execs, getStr(in, "nextToken"), getInt(in, "maxResults", 100))
		out := []any{}
		for _, e := range page {
			m := M{"executionArn": e.Arn, "stateMachineArn": e.SmArn, "name": e.Name, "status": e.Status, "startDate": epoch(e.Start)}
			if e.Stop > 0 {
				m["stopDate"] = epoch(e.Stop)
			}
			out = append(out, m)
		}
		resp := M{"executions": out}
		if next != "" {
			resp["nextToken"] = next
		}
		return resp, nil
	case "GetExecutionHistory":
		arn := getStr(in, "executionArn")
		if _, ok := a.getExec(arn); !ok {
			return nil, errf(400, "ExecutionDoesNotExist", "Execution Does Not Exist: '%s'", arn)
		}
		evs := a.history(arn)
		if getBool(in, "reverseOrder") {
			for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
				evs[i], evs[j] = evs[j], evs[i]
			}
		}
		page, next := pageOf(evs, getStr(in, "nextToken"), getInt(in, "maxResults", 1000))
		resp := M{"events": page}
		if next != "" {
			resp["nextToken"] = next
		}
		return resp, nil
	case "StopExecution":
		arn := getStr(in, "executionArn")
		e, ok := a.getExec(arn)
		if !ok {
			return nil, errf(400, "ExecutionDoesNotExist", "Execution Does Not Exist: '%s'", arn)
		}
		if e.Status == "RUNNING" {
			a.stopExecution(arn, getStr(in, "error"), getStr(in, "cause"))
			e = a.waitExecution(context.Background(), arn)
		}
		return M{"stopDate": epoch(e.Stop)}, nil
	case "TagResource", "UntagResource":
		return M{}, nil
	case "ListTagsForResource":
		return M{"tags": []any{}}, nil
	}
	return nil, errf(400, "UnknownOperationException", "localaws states: %s is not implemented", op)
}

var _ = errors.New
