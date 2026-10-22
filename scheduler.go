package main

// EventBridge Scheduler (REST JSON, signing name "scheduler"): schedules and schedule
// groups, tags. Schedules fire for real: rate(), cron() (the six-field AWS syntax, in
// ScheduleExpressionTimezone) and at() one-time schedules, honouring State, StartDate,
// EndDate and ActionAfterCompletion=DELETE. The target receives its Input as is (or
// "{}"): state machines, queues, topics, ECS tasks (EcsParameters), log groups — the
// same deliveries EventBridge rules make. cron() works for EventBridge rules too.

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // ScheduleExpressionTimezone needs the zone database everywhere
)

type Schedule struct {
	Name, Group, Expression, Timezone, State, Description, KmsKey, AfterCompletion string
	Window, Target                                                                 M
	Start, End, Created, Modified                                                  int64 // epoch ms, 0 = unset
}

type ScheduleGroup struct {
	Name    string
	Created int64
}

func (a *App) scheduleArn(s Schedule) string {
	return a.arn("scheduler", "schedule/"+s.Group+"/"+s.Name)
}
func (a *App) groupArn(n string) string { return a.arn("scheduler", "schedule-group/"+n) }

// restAPI claims requests of the REST JSON services emulated here.
func (a *App) restAPI(svc string, r *http.Request) (func(http.ResponseWriter, *http.Request), bool) {
	switch svc {
	case "scheduler":
		return a.schedulerREST, true
	}
	return nil, false
}

func tsMs(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x * 1000)
	case string:
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func (a *App) scheduleOut(s Schedule) M {
	m := M{"Arn": a.scheduleArn(s), "Name": s.Name, "GroupName": s.Group, "ScheduleExpression": s.Expression, "State": s.State,
		"FlexibleTimeWindow": s.Window, "Target": s.Target, "CreationDate": epoch(s.Created), "LastModificationDate": epoch(s.Modified),
		"ScheduleExpressionTimezone": s.Timezone, "ActionAfterCompletion": s.AfterCompletion}
	if s.Description != "" {
		m["Description"] = s.Description
	}
	if s.KmsKey != "" {
		m["KmsKeyArn"] = s.KmsKey
	}
	if s.Start > 0 {
		m["StartDate"] = epoch(s.Start)
	}
	if s.End > 0 {
		m["EndDate"] = epoch(s.End)
	}
	return m
}

func validScheduleExpr(expr, tz string) error {
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("Invalid ScheduleExpressionTimezone %s", tz)
	}
	switch {
	case strings.HasPrefix(expr, "rate("):
		if _, ok := parseRate(expr); !ok {
			return fmt.Errorf("Invalid Schedule Expression %s.", expr)
		}
	case strings.HasPrefix(expr, "cron("):
		if _, err := parseCron(expr); err != nil {
			return fmt.Errorf("Invalid Schedule Expression %s: %v", expr, err)
		}
	case strings.HasPrefix(expr, "at("):
		if _, err := time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(strings.TrimPrefix(expr, "at("), ")")); err != nil {
			return fmt.Errorf("Invalid Schedule Expression %s.", expr)
		}
	default:
		return fmt.Errorf("Invalid Schedule Expression %s.", expr)
	}
	return nil
}

func (a *App) schedulerREST(w http.ResponseWriter, r *http.Request) {
	in := readJSON(r)
	q := r.URL.Query()
	path := strings.Trim(r.URL.Path, "/")
	fail := func(e *apiError) { writeJSONErr(w, e) }
	switch {
	case path == "schedules" && r.Method == http.MethodGet:
		out := []any{}
		for _, s := range list[Schedule](a.db, "schedule") {
			if (q.Get("NamePrefix") != "" && !strings.HasPrefix(s.Name, q.Get("NamePrefix"))) || (q.Get("ScheduleGroup") != "" && s.Group != q.Get("ScheduleGroup")) ||
				(q.Get("State") != "" && s.State != q.Get("State")) {
				continue
			}
			out = append(out, M{"Arn": a.scheduleArn(s), "Name": s.Name, "GroupName": s.Group, "State": s.State, "CreationDate": epoch(s.Created),
				"LastModificationDate": epoch(s.Modified), "Target": M{"Arn": getStr(s.Target, "Arn")}})
		}
		writeJSON(w, 200, M{"Schedules": out})
	case strings.HasPrefix(path, "schedules/"):
		name := strings.TrimPrefix(path, "schedules/")
		group := q.Get("groupName")
		if g := getStr(in, "GroupName"); g != "" {
			group = g
		}
		if group == "" {
			group = "default"
		}
		key := group + "/" + name
		var cur Schedule
		exists := a.db.get("schedule", key, &cur)
		switch r.Method {
		case http.MethodGet:
			if !exists {
				fail(errf(404, "ResourceNotFoundException", "Schedule %s does not exist.", name))
				return
			}
			writeJSON(w, 200, a.scheduleOut(cur))
		case http.MethodDelete:
			if !exists {
				fail(errf(404, "ResourceNotFoundException", "Schedule %s does not exist.", name))
				return
			}
			_ = a.db.del("schedule", key)
			writeJSON(w, 200, M{})
		case http.MethodPost, http.MethodPut:
			if r.Method == http.MethodPost && exists {
				fail(errf(409, "ConflictException", "Schedule %s already exists.", name))
				return
			}
			if r.Method == http.MethodPut && !exists {
				fail(errf(404, "ResourceNotFoundException", "Schedule %s does not exist.", name))
				return
			}
			if group != "default" && !a.db.has("schedulegroup", group) {
				fail(errf(404, "ResourceNotFoundException", "Schedule group %s does not exist.", group))
				return
			}
			s := Schedule{Name: name, Group: group, Expression: getStr(in, "ScheduleExpression"), Timezone: getStr(in, "ScheduleExpressionTimezone"),
				State: getStr(in, "State"), Description: getStr(in, "Description"), KmsKey: getStr(in, "KmsKeyArn"), AfterCompletion: getStr(in, "ActionAfterCompletion"),
				Window: getMap(in, "FlexibleTimeWindow"), Target: getMap(in, "Target"), Start: tsMs(in["StartDate"]), End: tsMs(in["EndDate"]),
				Created: nowMs(), Modified: nowMs()}
			if exists {
				s.Created = cur.Created
			}
			if s.Timezone == "" {
				s.Timezone = "UTC"
			}
			if s.State == "" {
				s.State = "ENABLED"
			}
			if s.AfterCompletion == "" {
				s.AfterCompletion = "NONE"
			}
			if err := validScheduleExpr(s.Expression, s.Timezone); err != nil {
				fail(errf(400, "ValidationException", "%v", err))
				return
			}
			if getStr(s.Target, "Arn") == "" || getStr(s.Target, "RoleArn") == "" {
				fail(errf(400, "ValidationException", "Target Arn and RoleArn are required."))
				return
			}
			_ = a.db.put("schedule", key, s)
			writeJSON(w, 200, M{"ScheduleArn": a.scheduleArn(s)})
		}
	case path == "schedule-groups" && r.Method == http.MethodGet:
		out := []any{M{"Arn": a.groupArn("default"), "Name": "default", "State": "ACTIVE", "CreationDate": epoch(a.started), "LastModificationDate": epoch(a.started)}}
		for _, g := range list[ScheduleGroup](a.db, "schedulegroup") {
			out = append(out, M{"Arn": a.groupArn(g.Name), "Name": g.Name, "State": "ACTIVE", "CreationDate": epoch(g.Created), "LastModificationDate": epoch(g.Created)})
		}
		writeJSON(w, 200, M{"ScheduleGroups": out})
	case strings.HasPrefix(path, "schedule-groups/"):
		name := strings.TrimPrefix(path, "schedule-groups/")
		var g ScheduleGroup
		exists := a.db.get("schedulegroup", name, &g) || name == "default"
		if name == "default" {
			g = ScheduleGroup{Name: "default", Created: a.started}
		}
		switch r.Method {
		case http.MethodPost:
			if exists {
				fail(errf(409, "ConflictException", "Schedule group %s already exists.", name))
				return
			}
			g = ScheduleGroup{Name: name, Created: nowMs()}
			_ = a.db.put("schedulegroup", name, g)
			a.tagResource(a.groupArn(name), tagsFromList(getList(in, "Tags"), "Key", "Value"))
			writeJSON(w, 200, M{"ScheduleGroupArn": a.groupArn(name)})
		case http.MethodGet:
			if !exists {
				fail(errf(404, "ResourceNotFoundException", "Schedule group %s does not exist.", name))
				return
			}
			writeJSON(w, 200, M{"Arn": a.groupArn(name), "Name": name, "State": "ACTIVE", "CreationDate": epoch(g.Created), "LastModificationDate": epoch(g.Created)})
		case http.MethodDelete:
			if !exists || name == "default" {
				fail(errf(404, "ResourceNotFoundException", "Schedule group %s does not exist.", name))
				return
			}
			for _, s := range list[Schedule](a.db, "schedule") {
				if s.Group == name {
					_ = a.db.del("schedule", s.Group+"/"+s.Name)
				}
			}
			_ = a.db.del("schedulegroup", name)
			a.untagAll(a.groupArn(name))
			writeJSON(w, 200, M{})
		}
	case strings.HasPrefix(path, "tags/"):
		arn := strings.TrimPrefix(path, "tags/")
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, M{"Tags": tagList(a.tagsOf(arn), "Key", "Value")})
		case http.MethodPost:
			a.tagResource(arn, tagsFromList(getList(in, "Tags"), "Key", "Value"))
			writeJSON(w, 200, M{})
		case http.MethodDelete:
			a.untagResource(arn, q["TagKeys"])
			writeJSON(w, 200, M{})
		}
	default:
		fail(errf(404, "ResourceNotFoundException", "localaws scheduler: %s %s is not implemented", r.Method, r.URL.Path))
	}
}

// ── firing ───────────────────────────────────────────────────────────────

// firing remembers when each schedule last fired.
type firing struct {
	mu   sync.Mutex
	last map[string]time.Time // key → last fire (rate) or last minute fired (cron)
}

func (a *App) fireSchedules(f *firing, now time.Time) {
	for _, s := range list[Schedule](a.db, "schedule") {
		if s.State != "ENABLED" || (s.Start > 0 && now.UnixMilli() < s.Start) || (s.End > 0 && now.UnixMilli() > s.End) {
			continue
		}
		key := "schedule:" + s.Group + "/" + s.Name
		loc, err := time.LoadLocation(s.Timezone)
		if err != nil {
			loc = time.UTC
		}
		if !f.due(key, s.Expression, now, loc, s.Modified) {
			continue
		}
		input := getStr(s.Target, "Input")
		if input == "" {
			input = "{}"
		}
		t := Target{Id: s.Name, Arn: getStr(s.Target, "Arn"), RoleArn: getStr(s.Target, "RoleArn"), Input: input, Extra: M{}}
		t.SqsParameters.MessageGroupId = getStr(getMap(s.Target, "SqsParameters"), "MessageGroupId")
		if ep := getMap(s.Target, "EcsParameters"); len(ep) > 0 {
			t.Extra["EcsParameters"] = ep
		}
		a.logf("scheduler %s/%s fired (%s)", s.Group, s.Name, s.Expression)
		a.deliverTarget("schedule/"+s.Group+"/"+s.Name, t, uuid4(), input)
		if strings.HasPrefix(s.Expression, "at(") && s.AfterCompletion == "DELETE" {
			_ = a.db.del("schedule", s.Group+"/"+s.Name)
		}
	}
}

// due reports whether a schedule expression fires at now (and records it).
func (f *firing) due(key, expr string, now time.Time, loc *time.Location, since int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev, seen := f.last[key]
	switch {
	case strings.HasPrefix(expr, "rate("):
		every, ok := parseRate(expr)
		if !ok {
			return false
		}
		if !seen {
			f.last[key] = now
			return false
		}
		if now.Sub(prev) >= every {
			f.last[key] = now
			return true
		}
	case strings.HasPrefix(expr, "cron("):
		c, err := parseCron(expr)
		minute := now.In(loc).Truncate(time.Minute)
		if err != nil || (seen && !minute.After(prev)) || !c.match(minute) {
			return false
		}
		f.last[key] = minute
		return true
	case strings.HasPrefix(expr, "at("):
		at, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimSuffix(strings.TrimPrefix(expr, "at("), ")"), loc)
		if err != nil || seen && !prev.Before(time.UnixMilli(since)) || now.Before(at) {
			return false
		}
		f.last[key] = now
		return true
	}
	return false
}

// ── cron(min hour dom month dow year) ────────────────────────────────────
type cronField struct {
	any  bool
	vals map[int]bool
	last bool // L (day of month)
	lastWeekday,
	nearestWeekday int // dom W
	lastDow int // dow nL
	nthDow  [2]int
}

type cronExpr struct{ min, hour, dom, month, dow, year cronField }

var cronNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
	"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7}

var cronRe = regexp.MustCompile(`^cron\((.*)\)$`)

func parseCron(expr string) (cronExpr, error) {
	var c cronExpr
	m := cronRe.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return c, fmt.Errorf("not a cron() expression")
	}
	parts := strings.Fields(m[1])
	if len(parts) != 6 {
		return c, fmt.Errorf("cron() takes 6 fields (minutes hours day-of-month month day-of-week year), got %d", len(parts))
	}
	if (parts[2] == "?") == (parts[4] == "?") {
		return c, fmt.Errorf("exactly one of day-of-month and day-of-week must be ?")
	}
	bounds := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {1, 7}, {1970, 2199}}
	fields := []*cronField{&c.min, &c.hour, &c.dom, &c.month, &c.dow, &c.year}
	for i, p := range parts {
		f, err := parseCronField(strings.ToUpper(p), bounds[i][0], bounds[i][1], i)
		if err != nil {
			return c, fmt.Errorf("field %d (%s): %v", i+1, p, err)
		}
		*fields[i] = f
	}
	return c, nil
}

func cronNum(s string) (int, error) {
	if n, ok := cronNames[s]; ok {
		return n, nil
	}
	return strconv.Atoi(s)
}

func parseCronField(p string, lo, hi, idx int) (cronField, error) {
	f := cronField{vals: map[int]bool{}}
	if p == "*" || p == "?" {
		f.any = true
		return f, nil
	}
	for _, item := range strings.Split(p, ",") {
		switch {
		case idx == 2 && item == "L":
			f.last = true
			continue
		case idx == 2 && item == "LW":
			f.lastWeekday = 1
			continue
		case idx == 2 && strings.HasSuffix(item, "W"):
			n, err := strconv.Atoi(strings.TrimSuffix(item, "W"))
			if err != nil {
				return f, err
			}
			f.nearestWeekday = n
			continue
		case idx == 4 && strings.HasSuffix(item, "L") && len(item) > 1:
			n, err := cronNum(strings.TrimSuffix(item, "L"))
			if err != nil {
				return f, err
			}
			f.lastDow = n
			continue
		case idx == 4 && strings.Contains(item, "#"):
			d, k, _ := strings.Cut(item, "#")
			n, err1 := cronNum(d)
			nth, err2 := strconv.Atoi(k)
			if err1 != nil || err2 != nil {
				return f, fmt.Errorf("bad %s", item)
			}
			f.nthDow = [2]int{n, nth}
			continue
		}
		rng, step := item, 1
		if r, s, ok := strings.Cut(item, "/"); ok {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return f, fmt.Errorf("bad step %s", s)
			}
			rng, step = r, n
		}
		from, to := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err1, err2 error
			from, err1 = cronNum(a)
			to, err2 = cronNum(b)
			if err1 != nil || err2 != nil {
				return f, fmt.Errorf("bad range %s", rng)
			}
		default:
			n, err := cronNum(rng)
			if err != nil {
				return f, err
			}
			from = n
			if !strings.Contains(item, "/") {
				to = n
			}
		}
		if from < lo || to > hi || from > to {
			return f, fmt.Errorf("%s out of range %d-%d", rng, lo, hi)
		}
		for v := from; v <= to; v += step {
			f.vals[v] = true
		}
	}
	return f, nil
}

func (f cronField) has(v int) bool { return f.any || f.vals[v] }

func (c cronExpr) match(t time.Time) bool {
	if !c.min.has(t.Minute()) || !c.hour.has(t.Hour()) || !c.month.has(int(t.Month())) || !c.year.has(t.Year()) {
		return false
	}
	lastDay := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, t.Location()).Day()
	dow := int(t.Weekday()) + 1 // AWS: 1 = Sunday
	if !c.dom.any {
		d := t.Day()
		ok := c.dom.vals[d] || (c.dom.last && d == lastDay)
		if c.dom.lastWeekday > 0 {
			ld := time.Date(t.Year(), t.Month(), lastDay, 0, 0, 0, 0, t.Location())
			for ld.Weekday() == time.Saturday || ld.Weekday() == time.Sunday {
				ld = ld.AddDate(0, 0, -1)
			}
			ok = ok || d == ld.Day()
		}
		if n := c.dom.nearestWeekday; n > 0 && n <= lastDay {
			target := time.Date(t.Year(), t.Month(), n, 0, 0, 0, 0, t.Location())
			switch target.Weekday() {
			case time.Saturday:
				if n == 1 {
					target = target.AddDate(0, 0, 2)
				} else {
					target = target.AddDate(0, 0, -1)
				}
			case time.Sunday:
				if n == lastDay {
					target = target.AddDate(0, 0, -2)
				} else {
					target = target.AddDate(0, 0, 1)
				}
			}
			ok = ok || d == target.Day()
		}
		if !ok {
			return false
		}
	}
	if !c.dow.any {
		ok := c.dow.vals[dow]
		if c.dow.lastDow > 0 && dow == c.dow.lastDow && t.Day()+7 > lastDay {
			ok = true
		}
		if c.dow.nthDow[0] > 0 && dow == c.dow.nthDow[0] && (t.Day()-1)/7+1 == c.dow.nthDow[1] {
			ok = true
		}
		if !ok {
			return false
		}
	}
	return true
}
