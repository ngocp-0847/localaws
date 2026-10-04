package main

// ECS: clusters, task definitions (families + revisions), RunTask / DescribeTasks /
// ListTasks / StopTask, services kept at their desired count. A task is real work,
// started by the configured runner (runner.go): containers get the task definition's
// environment, `secrets` resolved from this emulator's SSM, `environmentFiles` read
// from its S3, the overrides, and AWS_ENDPOINT_URL pointing back here. awslogs output
// lands in CloudWatch Logs as <stream-prefix>/<container>/<task id>.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Cluster struct {
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

type TaskDef struct {
	Family     string `json:"family"`
	Revision   int    `json:"revision"`
	Status     string `json:"status"`
	Registered int64  `json:"registered"`
	Spec       M      `json:"spec"` // the RegisterTaskDefinition request, as sent
}

type Container struct {
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	Essential  bool     `json:"essential"`
	Command    []string `json:"command"`
	EntryPoint []string `json:"entryPoint"`
	Env        []KV     `json:"env"`
	Status     string   `json:"status"`
	ExitCode   *int     `json:"exitCode,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	LogGroup   string   `json:"logGroup,omitempty"`
	LogStream  string   `json:"logStream,omitempty"`
	RuntimeID  string   `json:"runtimeId"`
}

type Task struct {
	ID            string      `json:"id"`
	Arn           string      `json:"arn"`
	Cluster       string      `json:"cluster"`
	TaskDefArn    string      `json:"taskDefArn"`
	Family        string      `json:"family"`
	Status        string      `json:"status"`
	Desired       string      `json:"desired"`
	StopCode      string      `json:"stopCode,omitempty"`
	StoppedReason string      `json:"stoppedReason,omitempty"`
	StartedBy     string      `json:"startedBy,omitempty"`
	Group         string      `json:"group"`
	LaunchType    string      `json:"launchType"`
	CPU           string      `json:"cpu,omitempty"`
	Memory        string      `json:"memory,omitempty"`
	Created       int64       `json:"created"`
	PullStarted   int64       `json:"pullStarted,omitempty"`
	PullStopped   int64       `json:"pullStopped,omitempty"`
	Started       int64       `json:"started,omitempty"`
	Stopping      int64       `json:"stopping,omitempty"`
	Stopped       int64       `json:"stopped,omitempty"`
	Containers    []Container `json:"containers"`
	Overrides     M           `json:"overrides,omitempty"`
	Version       int         `json:"version"`
	ExecutionArn  string      `json:"executionArn,omitempty"` // the Step Functions execution that started it
}

func (t *Task) allEssentialZero() bool {
	if t.StopCode != "EssentialContainerExited" {
		return false
	}
	for _, c := range t.Containers {
		if c.Essential && (c.ExitCode == nil || *c.ExitCode != 0) {
			return false
		}
	}
	return true
}

type Service struct {
	Name       string `json:"name"`
	Cluster    string `json:"cluster"`
	TaskDef    string `json:"taskDef"`
	Desired    int    `json:"desired"`
	LaunchType string `json:"launchType"`
	Network    M      `json:"network,omitempty"`
	Status     string `json:"status"`
	Created    int64  `json:"created"`
}

func (a *App) clusterArn(n string) string { return a.arn("ecs", "cluster/"+n) }
func (a *App) tdArn(td TaskDef) string {
	return a.arn("ecs", fmt.Sprintf("task-definition/%s:%d", td.Family, td.Revision))
}

// ── state kept in memory for running tasks ───────────────────────────────
type taskRun struct {
	done chan struct{}
	once sync.Once
	kill func(reason string)
}

func (a *App) taskRunOf(id string) *taskRun {
	a.tasksMu.Lock()
	defer a.tasksMu.Unlock()
	tr := a.taskRuns[id]
	if tr == nil {
		tr = &taskRun{done: make(chan struct{})}
		a.taskRuns[id] = tr
	}
	return tr
}

func (a *App) taskDone(id string) <-chan struct{} { return a.taskRunOf(id).done }

func (a *App) task(id string) *Task {
	var t Task
	if a.db.get("task", arnTail(id), &t) {
		return &t
	}
	return nil
}

func (a *App) saveTask(t *Task) {
	a.tasksMu.Lock()
	t.Version++
	a.tasksMu.Unlock()
	_ = a.db.put("task", t.ID, t)
}

func (a *App) tasksOf(execArn string) []*Task {
	var out []*Task
	for _, t := range list[Task](a.db, "task") {
		if t.ExecutionArn == execArn {
			t := t
			out = append(out, &t)
		}
	}
	return out
}

func (a *App) stopTask(id, reason string) {
	t := a.task(id)
	if t == nil || t.Status == "STOPPED" {
		return
	}
	tr := a.taskRunOf(t.ID)
	if tr.kill != nil {
		tr.kill(reason)
	}
}

// ── task definitions ─────────────────────────────────────────────────────
func (a *App) registerTaskDef(spec M) (TaskDef, *apiError) {
	fam := getStr(spec, "family")
	if fam == "" {
		return TaskDef{}, errf(400, "ClientException", "Family must be specified.")
	}
	cds := getList(spec, "containerDefinitions")
	if len(cds) == 0 {
		return TaskDef{}, errf(400, "ClientException", "Container definitions must be specified.")
	}
	for _, c := range cds {
		cm, _ := c.(map[string]any)
		if getStr(cm, "name") == "" || getStr(cm, "image") == "" {
			return TaskDef{}, errf(400, "ClientException", "Container.name and Container.image should not be null or empty.")
		}
	}
	rev := 1
	for _, td := range list[TaskDef](a.db, "taskdef") {
		if td.Family == fam && td.Revision >= rev {
			rev = td.Revision + 1
		}
	}
	if r := getInt(spec, "revision", 0); r >= rev { // seed config may pin a revision
		rev = r
	}
	delete(spec, "revision")
	td := TaskDef{Family: fam, Revision: rev, Status: "ACTIVE", Registered: nowMs(), Spec: spec}
	_ = a.db.put("taskdef", fmt.Sprintf("%s:%d", fam, rev), td)
	a.logf("ecs task definition %s:%d registered", fam, rev)
	return td, nil
}

func (a *App) findTaskDef(ref string) (TaskDef, bool) {
	ref = strings.TrimPrefix(ref, a.arn("ecs", "task-definition/"))
	if i := strings.Index(ref, "task-definition/"); i >= 0 {
		ref = ref[i+len("task-definition/"):]
	}
	fam, rev, hasRev := strings.Cut(ref, ":")
	var best TaskDef
	found := false
	for _, td := range list[TaskDef](a.db, "taskdef") {
		if td.Family != fam {
			continue
		}
		if hasRev {
			if strconv.Itoa(td.Revision) == rev {
				return td, true
			}
			continue
		}
		if td.Status == "ACTIVE" && td.Revision > best.Revision {
			best, found = td, true
		}
	}
	return best, found
}

func (a *App) taskDefOut(td TaskDef) M {
	out := M{}
	for k, v := range td.Spec {
		out[k] = v
	}
	out["taskDefinitionArn"], out["family"], out["revision"], out["status"] = a.tdArn(td), td.Family, td.Revision, td.Status
	out["registeredAt"] = epoch(td.Registered)
	if _, ok := out["networkMode"]; !ok {
		out["networkMode"] = "bridge"
	}
	if _, ok := out["requiresCompatibilities"]; ok {
		out["compatibilities"] = []any{"EC2", "FARGATE"}
	}
	return out
}

// ── RunTask ──────────────────────────────────────────────────────────────
func pick(m M, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func pickMap(m M, keys ...string) M {
	v, _ := pick(m, keys...).(map[string]any)
	if v == nil {
		return M{}
	}
	return v
}

func pickList(m M, keys ...string) []any {
	l, _ := pick(m, keys...).([]any)
	return l
}

// runTask accepts the API's camelCase request and the PascalCase parameters a Step
// Functions ecs:runTask state resolves.
func (a *App) runTask(in M, defaultStartedBy, execArn string) (*Task, *apiError) {
	td, ok := a.findTaskDef(str(pick(in, "taskDefinition", "TaskDefinition")))
	if !ok || td.Status != "ACTIVE" {
		return nil, errf(400, "ClientException", "TaskDefinition not found.")
	}
	cluster := arnTail(str(pick(in, "cluster", "Cluster")))
	if cluster == "" {
		cluster = "default"
	}
	if !a.db.has("cluster", cluster) {
		return nil, errf(400, "ClusterNotFoundException", "Cluster not found.")
	}
	net := pickMap(pickMap(in, "networkConfiguration", "NetworkConfiguration"), "awsvpcConfiguration", "AwsvpcConfiguration")
	if getStr(td.Spec, "networkMode") == "awsvpc" {
		subnets := strList(pickList(net, "subnets", "Subnets"))
		if len(subnets) == 0 {
			return nil, errf(400, "InvalidParameterException", "Network Configuration must be provided when networkMode 'awsvpc' is specified.")
		}
		if known := a.cfg.Subnets(); len(known) > 0 {
			for _, s := range subnets {
				if !known[s] {
					return nil, errf(400, "InvalidParameterException", "Error retrieving subnet information for [%s]: The subnet ID '%s' does not exist", s, s)
				}
			}
		}
	}
	ov := pickMap(in, "overrides", "Overrides")
	t := &Task{ID: randHex(16), Cluster: cluster, TaskDefArn: a.tdArn(td), Family: td.Family, Status: "PROVISIONING", Desired: "RUNNING",
		StartedBy: str(pick(in, "startedBy", "StartedBy")), Group: str(pick(in, "group", "Group")),
		LaunchType: str(pick(in, "launchType", "LaunchType")), CPU: getStr(td.Spec, "cpu"), Memory: getStr(td.Spec, "memory"),
		Created: nowMs(), Overrides: camelize(ov).(map[string]any), ExecutionArn: execArn}
	if t.StartedBy == "" {
		t.StartedBy = defaultStartedBy
	}
	if t.Group == "" {
		t.Group = "family:" + td.Family
	}
	if t.LaunchType == "" {
		t.LaunchType = "FARGATE"
	}
	if v := str(pick(ov, "cpu", "Cpu")); v != "" {
		t.CPU = v
	}
	if v := str(pick(ov, "memory", "Memory")); v != "" {
		t.Memory = v
	}
	t.Arn = a.arn("ecs", "task/"+cluster+"/"+t.ID)
	cov := map[string]M{}
	for _, c := range pickList(ov, "containerOverrides", "ContainerOverrides") {
		cm, _ := c.(map[string]any)
		cov[str(pick(cm, "name", "Name"))] = cm
	}
	for _, c := range getList(td.Spec, "containerDefinitions") {
		cm, _ := c.(map[string]any)
		ctr := Container{Name: getStr(cm, "name"), Image: getStr(cm, "image"), Essential: true, Status: "PENDING",
			Command: strList(getList(cm, "command")), EntryPoint: strList(getList(cm, "entryPoint")), RuntimeID: t.ID + "-" + randHex(4)}
		if e, ok := cm["essential"].(bool); ok {
			ctr.Essential = e
		}
		if o, ok := cov[ctr.Name]; ok {
			if cmd := pickList(o, "command", "Command"); cmd != nil {
				ctr.Command = strList(cmd)
			}
		}
		if lc := getMap(cm, "logConfiguration"); getStr(lc, "logDriver") == "awslogs" {
			opts := getMap(lc, "options")
			ctr.LogGroup = getStr(opts, "awslogs-group")
			prefix := getStr(opts, "awslogs-stream-prefix")
			ctr.LogStream = t.ID
			if prefix != "" {
				ctr.LogStream = prefix + "/" + ctr.Name + "/" + t.ID
			}
		}
		t.Containers = append(t.Containers, ctr)
	}
	a.taskRunOf(t.ID)
	a.saveTask(t)
	a.logf("ecs run-task %s %s (started by %q)", t.ID, td.Family, t.StartedBy)
	go a.launch(t, td, cov)
	return t, nil
}

// launch resolves each container's environment and hands the task to the runner.
func (a *App) launch(t *Task, td TaskDef, cov map[string]M) {
	tr := a.taskRunOf(t.ID)
	finish := func(code, reason string) {
		now := nowMs()
		if t.Stopping == 0 {
			t.Stopping = now
		}
		t.Status, t.Desired, t.Stopped, t.StopCode, t.StoppedReason = "STOPPED", "STOPPED", now, code, reason
		for i := range t.Containers {
			if t.Containers[i].Status != "STOPPED" {
				t.Containers[i].Status = "STOPPED"
			}
		}
		a.saveTask(t)
		a.logf("ecs task %s stopped: %s %s", t.ID, code, reason)
		tr.once.Do(func() { close(tr.done) })
	}
	t.Status, t.PullStarted = "PENDING", nowMs()
	a.saveTask(t)
	defs := map[string]M{}
	for _, c := range getList(td.Spec, "containerDefinitions") {
		cm, _ := c.(map[string]any)
		defs[getStr(cm, "name")] = cm
	}
	for i := range t.Containers {
		c := &t.Containers[i]
		env, err := a.containerEnv(defs[c.Name], cov[c.Name])
		if err != nil {
			c.Reason = err.Error()
			finish("TaskFailedToStart", "ResourceInitializationError: "+err.Error())
			return
		}
		c.Env = env
		if c.LogGroup != "" {
			opts := getMap(getMap(defs[c.Name], "logConfiguration"), "options")
			if !a.groupExists(c.LogGroup) {
				if getStr(opts, "awslogs-create-group") != "true" {
					finish("TaskFailedToStart", fmt.Sprintf("ResourceInitializationError: failed to validate logger args: create stream has been retried 1 times: failed to create Cloudwatch log stream: ResourceNotFoundException: The specified log group does not exist. : exit status 1 (group %s)", c.LogGroup))
					return
				}
				a.ensureGroup(c.LogGroup)
			}
			a.ensureStream(c.LogGroup, c.LogStream)
		}
	}
	runner := a.runner()
	stopReason := ""
	var mu sync.Mutex
	tr.kill = func(reason string) {
		mu.Lock()
		stopReason = reason
		mu.Unlock()
		runner.Stop(t)
	}
	started := func() {
		t.PullStopped, t.Started, t.Status = nowMs(), nowMs(), "RUNNING"
		for i := range t.Containers {
			t.Containers[i].Status = "RUNNING"
		}
		a.saveTask(t)
	}
	exits, err := runner.Run(t, started, func(ctr *Container, line string) {
		if ctr.LogGroup != "" {
			a.appendLog(ctr.LogGroup, ctr.LogStream, line)
		}
	})
	mu.Lock()
	reason := stopReason
	mu.Unlock()
	for i := range t.Containers {
		if code, ok := exits[t.Containers[i].Name]; ok {
			code := code
			t.Containers[i].ExitCode = &code
		}
	}
	switch {
	case reason != "":
		t.Stopping = nowMs()
		finish("UserInitiated", reason)
	case err != nil:
		finish("TaskFailedToStart", "CannotStartContainerError: "+err.Error())
	default:
		finish("EssentialContainerExited", "Essential container in task exited")
	}
}

// containerEnv: definition environment, environmentFiles (from this S3), secrets (from
// this SSM), overrides, then the AWS variables a task sees on Fargate.
func (a *App) containerEnv(def, ov M) ([]KV, error) {
	env := map[string]string{}
	order := []string{}
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, f := range append(getList(def, "environmentFiles"), pickList(ov, "environmentFiles", "EnvironmentFiles")...) {
		fm, _ := f.(map[string]any)
		ref := strings.TrimPrefix(str(pick(fm, "value", "Value")), "arn:aws:s3:::")
		bucket, key, _ := strings.Cut(ref, "/")
		o, ok := a.s3Latest(bucket, key)
		if !ok || o.DeleteMarker {
			return nil, fmt.Errorf("failed to download env files: file %s not found", ref)
		}
		body, err := readBlob(a, o)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				set(k, v)
			}
		}
	}
	for _, e := range getList(def, "environment") {
		em, _ := e.(map[string]any)
		set(getStr(em, "name"), getStr(em, "value"))
	}
	for _, s := range getList(def, "secrets") {
		sm, _ := s.(map[string]any)
		from := getStr(sm, "valueFrom")
		name := from
		if i := strings.Index(from, ":parameter"); strings.HasPrefix(from, "arn:") && i >= 0 {
			name = from[i+len(":parameter"):]
		} else if strings.HasPrefix(from, "arn:aws:secretsmanager") {
			return nil, fmt.Errorf("unable to pull secrets or registry auth: localaws does not emulate Secrets Manager (%s)", from)
		}
		p, ok := a.param(name)
		if !ok {
			return nil, fmt.Errorf("unable to pull secrets or registry auth: execution resource retrieval failed: unable to retrieve secrets from ssm: service call has been retried 1 time(s): ParameterNotFound: %s", name)
		}
		set(getStr(sm, "name"), p.Value)
	}
	for _, e := range pickList(ov, "environment", "Environment") {
		em, _ := e.(map[string]any)
		set(str(pick(em, "name", "Name")), str(pick(em, "value", "Value")))
	}
	for k, v := range a.cfg.TaskEnv() {
		if _, ok := env[k]; !ok {
			set(k, v)
		}
	}
	out := make([]KV, 0, len(order))
	for _, k := range order {
		out = append(out, KV{k, env[k]})
	}
	return out, nil
}

// ── response shapes ──────────────────────────────────────────────────────
func (a *App) taskOut(t *Task) M {
	cs := []any{}
	for _, c := range t.Containers {
		m := M{"containerArn": a.arn("ecs", "container/"+t.Cluster+"/"+t.ID+"/"+c.RuntimeID), "taskArn": t.Arn, "name": c.Name,
			"image": c.Image, "lastStatus": c.Status, "runtimeId": c.RuntimeID, "cpu": "0", "healthStatus": "UNKNOWN",
			"networkBindings": []any{}, "networkInterfaces": []any{}, "managedAgents": []any{}}
		if c.ExitCode != nil {
			m["exitCode"] = *c.ExitCode
		}
		if c.Reason != "" {
			m["reason"] = c.Reason
		}
		cs = append(cs, m)
	}
	m := M{"taskArn": t.Arn, "clusterArn": a.clusterArn(t.Cluster), "taskDefinitionArn": t.TaskDefArn, "containers": cs,
		"lastStatus": t.Status, "desiredStatus": t.Desired, "cpu": t.CPU, "memory": t.Memory, "createdAt": epoch(t.Created),
		"startedBy": t.StartedBy, "group": t.Group, "launchType": t.LaunchType, "version": t.Version, "platformVersion": "1.4.0",
		"platformFamily": "Linux", "availabilityZone": a.cfg.Region + "a", "connectivity": "CONNECTED", "connectivityAt": epoch(t.Created),
		"enableExecuteCommand": false, "ephemeralStorage": M{"sizeInGiB": 20}, "attachments": []any{}, "tags": []any{},
		"attributes": []any{M{"name": "ecs.cpu-architecture", "value": "x86_64"}}, "overrides": withDefaultOverrides(t.Overrides)}
	for k, v := range map[string]int64{"pullStartedAt": t.PullStarted, "pullStoppedAt": t.PullStopped, "startedAt": t.Started,
		"stoppingAt": t.Stopping, "stoppedAt": t.Stopped, "executionStoppedAt": t.Stopped} {
		if v > 0 {
			m[k] = epoch(v)
		}
	}
	if t.StopCode != "" {
		m["stopCode"], m["stoppedReason"] = t.StopCode, t.StoppedReason
	}
	return m
}

// camelize: the API always answers in camelCase, whichever spelling the caller used
// (Step Functions passes PascalCase parameters through).
func camelize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := M{}
		for k, val := range x {
			if k != "" {
				k = strings.ToLower(k[:1]) + k[1:]
			}
			out[k] = camelize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = camelize(e)
		}
		return out
	}
	return v
}

func withDefaultOverrides(ov M) M {
	out := M{"containerOverrides": []any{}, "inferenceAcceleratorOverrides": []any{}}
	for k, v := range ov {
		out[k] = v
	}
	return out
}

// taskPascal: the shape Step Functions embeds in TaskSubmitted / TaskSucceeded /
// TaskFailed (the Java SDK's: PascalCase keys, epoch-millisecond timestamps).
func (a *App) taskPascal(t *Task) M {
	return pascalize(a.taskOut(t)).(M)
}

func pascalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := M{}
		for k, val := range x {
			pk := k
			if k != "" {
				pk = strings.ToUpper(k[:1]) + k[1:]
			}
			if strings.HasSuffix(k, "At") {
				if f, ok := val.(float64); ok {
					out[pk] = int64(f * 1000)
					continue
				}
			}
			out[pk] = pascalize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pascalize(e)
		}
		return out
	}
	return v
}

// ── services (kept at desiredCount) ──────────────────────────────────────
func (a *App) reconcile(stop <-chan struct{}) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		tasks := list[Task](a.db, "task")
		for _, s := range list[Service](a.db, "service") {
			var live []Task
			for _, t := range tasks {
				if t.Group == "service:"+s.Name && t.Cluster == s.Cluster && t.Desired == "RUNNING" && t.Status != "STOPPED" {
					live = append(live, t)
				}
			}
			want := s.Desired
			if s.Status != "ACTIVE" {
				want = 0
			}
			for i := len(live); i < want; i++ {
				req := M{"cluster": s.Cluster, "taskDefinition": s.TaskDef, "group": "service:" + s.Name, "startedBy": "ecs-svc/" + s.Name,
					"launchType": s.LaunchType}
				if len(s.Network) > 0 {
					req["networkConfiguration"] = s.Network
				}
				if _, e := a.runTask(req, "", ""); e != nil {
					a.logf("ecs service %s: cannot start a task: %s", s.Name, e.Msg)
					break
				}
			}
			for i := want; i < len(live); i++ {
				a.stopTask(live[i].ID, "Scaling activity initiated by (deployment ecs-svc/"+s.Name+")")
			}
			if s.Status == "DRAINING" && len(live) == 0 {
				_ = a.db.del("service", s.Cluster+"/"+s.Name)
			}
		}
	}
}

func (a *App) serviceOut(s Service) M {
	running, pending := 0, 0
	for _, t := range list[Task](a.db, "task") {
		if t.Group == "service:"+s.Name && t.Cluster == s.Cluster {
			switch t.Status {
			case "RUNNING":
				running++
			case "PENDING", "PROVISIONING":
				pending++
			}
		}
	}
	m := M{"serviceName": s.Name, "serviceArn": a.arn("ecs", "service/"+s.Cluster+"/"+s.Name), "clusterArn": a.clusterArn(s.Cluster),
		"status": s.Status, "desiredCount": s.Desired, "runningCount": running, "pendingCount": pending, "launchType": s.LaunchType,
		"taskDefinition": s.TaskDef, "createdAt": epoch(s.Created), "deployments": []any{}, "events": []any{}}
	if len(s.Network) > 0 {
		m["networkConfiguration"] = s.Network
	}
	return m
}

// ── API ──────────────────────────────────────────────────────────────────
func (a *App) ecsAPI(op string, in M) (any, *apiError) {
	clusterOf := func() string {
		c := arnTail(getStr(in, "cluster"))
		if c == "" {
			c = "default"
		}
		return c
	}
	switch op {
	case "CreateCluster":
		name := getStr(in, "clusterName")
		if name == "" {
			name = "default"
		}
		if !a.db.has("cluster", name) {
			_ = a.db.put("cluster", name, Cluster{Name: name, Created: nowMs()})
		}
		return M{"cluster": a.clusterOut(name)}, nil
	case "DeleteCluster":
		name := clusterOf()
		if !a.db.has("cluster", name) {
			return nil, errf(400, "ClusterNotFoundException", "Cluster not found.")
		}
		_ = a.db.del("cluster", name)
		out := a.clusterOut(name)
		out["status"] = "INACTIVE"
		return M{"cluster": out}, nil
	case "DescribeClusters":
		names := strList(getList(in, "clusters"))
		if len(names) == 0 {
			names = []string{"default"}
		}
		out, fails := []any{}, []any{}
		for _, n := range names {
			if a.db.has("cluster", arnTail(n)) {
				out = append(out, a.clusterOut(arnTail(n)))
			} else {
				fails = append(fails, M{"arn": n, "reason": "MISSING"})
			}
		}
		return M{"clusters": out, "failures": fails}, nil
	case "ListClusters":
		out := []any{}
		for _, c := range list[Cluster](a.db, "cluster") {
			out = append(out, a.clusterArn(c.Name))
		}
		return M{"clusterArns": out}, nil
	case "RegisterTaskDefinition":
		td, e := a.registerTaskDef(in)
		if e != nil {
			return nil, e
		}
		return M{"taskDefinition": a.taskDefOut(td), "tags": []any{}}, nil
	case "DescribeTaskDefinition":
		td, ok := a.findTaskDef(getStr(in, "taskDefinition"))
		if !ok {
			return nil, errf(400, "ClientException", "Unable to describe task definition.")
		}
		return M{"taskDefinition": a.taskDefOut(td), "tags": []any{}}, nil
	case "DeregisterTaskDefinition":
		td, ok := a.findTaskDef(getStr(in, "taskDefinition"))
		if !ok {
			return nil, errf(400, "ClientException", "The specified task definition does not exist.")
		}
		td.Status = "INACTIVE"
		_ = a.db.put("taskdef", fmt.Sprintf("%s:%d", td.Family, td.Revision), td)
		return M{"taskDefinition": a.taskDefOut(td)}, nil
	case "ListTaskDefinitions":
		tds := list[TaskDef](a.db, "taskdef")
		sort.Slice(tds, func(i, j int) bool {
			if tds[i].Family != tds[j].Family {
				return tds[i].Family < tds[j].Family
			}
			return tds[i].Revision < tds[j].Revision
		})
		out := []any{}
		for _, td := range tds {
			if p := getStr(in, "familyPrefix"); p != "" && !strings.HasPrefix(td.Family, p) {
				continue
			}
			if st := getStr(in, "status"); (st == "" && td.Status != "ACTIVE") || (st != "" && st != "ALL" && st != td.Status) {
				continue
			}
			out = append(out, a.tdArn(td))
		}
		return M{"taskDefinitionArns": out}, nil
	case "ListTaskDefinitionFamilies":
		seen := map[string]bool{}
		out := []any{}
		for _, td := range list[TaskDef](a.db, "taskdef") {
			if !seen[td.Family] {
				seen[td.Family] = true
				out = append(out, td.Family)
			}
		}
		return M{"families": out}, nil
	case "RunTask":
		n := getInt(in, "count", 1)
		if n < 1 || n > 10 {
			return nil, errf(400, "InvalidParameterException", "count must be between 1 and 10.")
		}
		tasks := []any{}
		for i := 0; i < n; i++ {
			t, e := a.runTask(in, "", "")
			if e != nil {
				return nil, e
			}
			tasks = append(tasks, a.taskOut(t))
		}
		return M{"tasks": tasks, "failures": []any{}}, nil
	case "DescribeTasks":
		out, fails := []any{}, []any{}
		for _, ref := range strList(getList(in, "tasks")) {
			if t := a.task(ref); t != nil {
				out = append(out, a.taskOut(t))
			} else {
				fails = append(fails, M{"arn": ref, "reason": "MISSING"})
			}
		}
		return M{"tasks": out, "failures": fails}, nil
	case "StopTask":
		t := a.task(getStr(in, "task"))
		if t == nil {
			return nil, errf(400, "InvalidParameterException", "The referenced task was not found.")
		}
		reason := getStr(in, "reason")
		if reason == "" {
			reason = "Task stopped by user"
		}
		a.stopTask(t.ID, reason)
		return M{"task": a.taskOut(t)}, nil
	case "ListTasks":
		cluster := clusterOf()
		tasks := list[Task](a.db, "task")
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created > tasks[j].Created })
		out := []any{}
		for _, t := range tasks {
			desired := getStr(in, "desiredStatus")
			if desired == "" {
				desired = "RUNNING"
			}
			if t.Cluster != cluster || t.Desired != desired {
				continue
			}
			if f := getStr(in, "family"); f != "" && t.Family != f {
				continue
			}
			if sb := getStr(in, "startedBy"); sb != "" && t.StartedBy != sb {
				continue
			}
			if sn := getStr(in, "serviceName"); sn != "" && t.Group != "service:"+sn {
				continue
			}
			out = append(out, t.Arn)
		}
		page, next := pageOf(out, getStr(in, "nextToken"), getInt(in, "maxResults", 100))
		resp := M{"taskArns": page}
		if next != "" {
			resp["nextToken"] = next
		}
		return resp, nil
	case "CreateService":
		s := Service{Name: getStr(in, "serviceName"), Cluster: clusterOf(), Desired: getInt(in, "desiredCount", 0),
			LaunchType: getStr(in, "launchType"), Network: getMap(in, "networkConfiguration"), Status: "ACTIVE", Created: nowMs()}
		td, ok := a.findTaskDef(getStr(in, "taskDefinition"))
		if !ok {
			return nil, errf(400, "ClientException", "TaskDefinition not found.")
		}
		if !a.db.has("cluster", s.Cluster) {
			return nil, errf(400, "ClusterNotFoundException", "Cluster not found.")
		}
		if a.db.has("service", s.Cluster+"/"+s.Name) {
			return nil, errf(400, "InvalidParameterException", "Creation of service was not idempotent.")
		}
		s.TaskDef = a.tdArn(td)
		_ = a.db.put("service", s.Cluster+"/"+s.Name, s)
		return M{"service": a.serviceOut(s)}, nil
	case "UpdateService":
		var s Service
		if !a.db.get("service", clusterOf()+"/"+arnTail(getStr(in, "service")), &s) {
			return nil, errf(400, "ServiceNotFoundException", "Service not found.")
		}
		if _, ok := in["desiredCount"]; ok {
			s.Desired = getInt(in, "desiredCount", s.Desired)
		}
		if ref := getStr(in, "taskDefinition"); ref != "" {
			td, ok := a.findTaskDef(ref)
			if !ok {
				return nil, errf(400, "ClientException", "TaskDefinition not found.")
			}
			s.TaskDef = a.tdArn(td)
		}
		_ = a.db.put("service", s.Cluster+"/"+s.Name, s)
		return M{"service": a.serviceOut(s)}, nil
	case "DeleteService":
		var s Service
		if !a.db.get("service", clusterOf()+"/"+arnTail(getStr(in, "service")), &s) {
			return nil, errf(400, "ServiceNotFoundException", "Service not found.")
		}
		if s.Desired > 0 && !getBool(in, "force") {
			return nil, errf(400, "InvalidParameterException", "The service cannot be stopped while it is scaled above 0.")
		}
		s.Status, s.Desired = "DRAINING", 0
		_ = a.db.put("service", s.Cluster+"/"+s.Name, s)
		return M{"service": a.serviceOut(s)}, nil
	case "DescribeServices":
		out, fails := []any{}, []any{}
		for _, ref := range strList(getList(in, "services")) {
			var s Service
			if a.db.get("service", clusterOf()+"/"+arnTail(ref), &s) {
				out = append(out, a.serviceOut(s))
			} else {
				fails = append(fails, M{"arn": ref, "reason": "MISSING"})
			}
		}
		return M{"services": out, "failures": fails}, nil
	case "ListServices":
		out := []any{}
		for _, s := range list[Service](a.db, "service") {
			if s.Cluster == clusterOf() {
				out = append(out, a.arn("ecs", "service/"+s.Cluster+"/"+s.Name))
			}
		}
		return M{"serviceArns": out}, nil
	case "TagResource", "UntagResource":
		return M{}, nil
	case "ListTagsForResource":
		return M{"tags": []any{}}, nil
	}
	return nil, errf(400, "UnknownOperationException", "localaws ecs: %s is not implemented", op)
}

func (a *App) clusterOut(name string) M {
	running, pending := 0, 0
	for _, t := range list[Task](a.db, "task") {
		if t.Cluster == name {
			switch t.Status {
			case "RUNNING":
				running++
			case "PENDING", "PROVISIONING":
				pending++
			}
		}
	}
	return M{"clusterArn": a.clusterArn(name), "clusterName": name, "status": "ACTIVE", "runningTasksCount": running,
		"pendingTasksCount": pending, "activeServicesCount": len(list[Service](a.db, "service")), "registeredContainerInstancesCount": 0,
		"capacityProviders": []any{"FARGATE", "FARGATE_SPOT"}, "settings": []any{}, "tags": []any{}}
}

var _ = json.Marshal
