package main

// Configuration: flags > LOCALAWS_* environment > config file > defaults.
//
// The optional config file (JSON) declares resources to exist at start-up — the same
// shapes the AWS APIs take — so a whole environment comes up with one command and is
// re-applied idempotently on every start. Inside the file:
//   ${ACCOUNT} ${REGION}              this emulator's account id / region
//   ${env:NAME}  ${env:NAME:-default} environment variables
// "definitionFile" / "specFile" paths are relative to the config file.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Subnet struct {
	ID    string            `json:"id"`
	VpcID string            `json:"vpcId,omitempty"`
	Cidr  string            `json:"cidr,omitempty"`
	AZ    string            `json:"az,omitempty"`
	Tags  map[string]string `json:"tags,omitempty"`
}

type SecurityGroup struct {
	ID    string            `json:"id"`
	Name  string            `json:"name,omitempty"`
	VpcID string            `json:"vpcId,omitempty"`
	Tags  map[string]string `json:"tags,omitempty"`
}

type Resources struct {
	S3 struct {
		Buckets []struct {
			Name        string `json:"name"`
			Versioning  bool   `json:"versioning"`
			EventBridge bool   `json:"eventBridge"`
		} `json:"buckets"`
	} `json:"s3"`
	EC2 struct {
		Subnets        []Subnet        `json:"subnets"`
		SecurityGroups []SecurityGroup `json:"securityGroups"`
	} `json:"ec2"`
	ECS struct {
		Clusters        []string `json:"clusters"`
		TaskDefinitions []M      `json:"taskDefinitions"`
		Services        []M      `json:"services"`
	} `json:"ecs"`
	StepFunctions struct {
		StateMachines []struct {
			Name           string `json:"name"`
			Type           string `json:"type"`
			RoleArn        string `json:"roleArn"`
			Definition     any    `json:"definition"`
			DefinitionFile string `json:"definitionFile"`
		} `json:"stateMachines"`
	} `json:"stepFunctions"`
	Events struct {
		Rules []struct {
			Name               string   `json:"name"`
			State              string   `json:"state"`
			Description        string   `json:"description"`
			EventPattern       any      `json:"eventPattern"`
			ScheduleExpression string   `json:"scheduleExpression"`
			Targets            []Target `json:"targets"`
		} `json:"rules"`
	} `json:"events"`
	SSM struct {
		Parameters []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"parameters"`
	} `json:"ssm"`
	Logs struct {
		Groups []string `json:"groups"`
	} `json:"logs"`
	SNS struct {
		Topics []string `json:"topics"`
	} `json:"sns"`
}

type RunnerCfg struct {
	Mode   string `json:"mode"` // docker | exec | noop
	Docker struct {
		Network     string            `json:"network"`
		HostGateway *bool             `json:"hostGateway"`
		Images      map[string]string `json:"images"`
		ExtraArgs   []string          `json:"extraArgs"`
		PauseImage  string            `json:"pauseImage"` // owns a multi-container task's network namespace
	} `json:"docker"`
	Exec struct {
		Dir string `json:"dir"`
	} `json:"exec"`
	Endpoint  string            `json:"endpoint"`  // AWS_ENDPOINT_URL given to tasks
	InjectAWS *bool             `json:"injectAws"` // AWS_ENDPOINT_URL / region / dummy keys (default on)
	Env       map[string]string `json:"env"`       // extra environment for every task
}

type Config struct {
	Port      int       `json:"port"`
	DataDir   string    `json:"dataDir"`
	Account   string    `json:"account"`
	Region    string    `json:"region"`
	Identity  string    `json:"identity"`
	PublicURL string    `json:"publicUrl"`
	TimeScale float64   `json:"timeScale"`
	LogAPI    bool      `json:"logApi"`
	Runner    RunnerCfg `json:"runner"`
	Resources Resources `json:"resources"`
	file      string
}

func (c *Config) IdentityArn() string {
	if strings.HasPrefix(c.Identity, "arn:") {
		return c.Identity
	}
	return fmt.Sprintf("arn:aws:iam::%s:user/%s", c.Account, c.Identity)
}

func (c *Config) Subnets() map[string]bool {
	m := map[string]bool{}
	for _, s := range c.Resources.EC2.Subnets {
		m[s.ID] = true
	}
	return m
}

// TaskEnv is added to every container (definition values win).
func (c *Config) TaskEnv() map[string]string {
	env := map[string]string{}
	for k, v := range c.Runner.Env {
		env[k] = v
	}
	if c.Runner.InjectAWS == nil || *c.Runner.InjectAWS {
		ep := c.Runner.Endpoint
		if ep == "" {
			if c.Runner.Mode == "docker" || c.Runner.Mode == "" {
				ep = fmt.Sprintf("http://host.docker.internal:%d", c.Port)
			} else {
				ep = fmt.Sprintf("http://127.0.0.1:%d", c.Port)
			}
		}
		for k, v := range map[string]string{"AWS_ENDPOINT_URL": ep, "AWS_REGION": c.Region, "AWS_DEFAULT_REGION": c.Region,
			"AWS_ACCESS_KEY_ID": "localaws", "AWS_SECRET_ACCESS_KEY": "localaws", "AWS_EXECUTION_ENV": "AWS_ECS_FARGATE"} {
			if _, ok := env[k]; !ok {
				env[k] = v
			}
		}
	}
	return env
}

var subst = regexp.MustCompile(`\$\{(ACCOUNT|REGION|env:([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?)\}`)

func expand(text string, c *Config) string {
	return subst.ReplaceAllStringFunc(text, func(m string) string {
		g := subst.FindStringSubmatch(m)
		switch g[1] {
		case "ACCOUNT":
			if c != nil {
				return c.Account
			}
			return m
		case "REGION":
			if c != nil {
				return c.Region
			}
			return m
		}
		if v, ok := os.LookupEnv(g[2]); ok {
			return v
		}
		return g[4]
	})
}

func loadConfig(args []string) (*Config, error) {
	fs := flag.NewFlagSet("localaws", flag.ContinueOnError)
	env := func(k, d string) string {
		if v := os.Getenv("LOCALAWS_" + k); v != "" {
			return v
		}
		return d
	}
	file := fs.String("config", env("CONFIG", ""), "config file (JSON) declaring settings and resources")
	port := fs.Int("port", 0, "listen port for every service and the UI (default 4566)")
	data := fs.String("data", "", "data directory: SQLite database + object blobs (default ./data)")
	account := fs.String("account", "", "account id in every ARN (default 000000000000)")
	region := fs.String("region", "", "region (default us-east-1)")
	identity := fs.String("identity", "", "IAM user name or ARN returned by sts:GetCallerIdentity (default localaws)")
	public := fs.String("public-url", "", "base URL the UI and console-style links use")
	runner := fs.String("runner", "", "how ECS tasks run: docker | exec | noop (default docker)")
	network := fs.String("docker-network", "", "runner=docker: network for task containers")
	execDir := fs.String("exec-dir", "", "runner=exec: working directory of task processes")
	endpoint := fs.String("task-endpoint", "", "AWS_ENDPOINT_URL given to tasks")
	scale := fs.Float64("time-scale", 0, "multiplier for Wait states, retries and timeouts (0.1 = ten times faster)")
	logAPI := fs.Bool("log-api", false, "log every API call")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c := &Config{}
	if *file != "" {
		raw, err := os.ReadFile(*file)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(expand(string(raw), nil)), c); err != nil {
			return nil, fmt.Errorf("%s: %w", *file, err)
		}
		c.file = *file
	}
	setS := func(dst *string, flagV, envK, def string) {
		switch {
		case flagV != "":
			*dst = flagV
		case os.Getenv("LOCALAWS_"+envK) != "":
			*dst = os.Getenv("LOCALAWS_" + envK)
		case *dst == "":
			*dst = def
		}
	}
	setS(&c.DataDir, *data, "DATA", "data")
	setS(&c.Account, *account, "ACCOUNT", "000000000000")
	setS(&c.Region, *region, "REGION", "us-east-1")
	setS(&c.Identity, *identity, "IDENTITY", "localaws")
	setS(&c.Runner.Mode, *runner, "RUNNER", "docker")
	setS(&c.Runner.Docker.Network, *network, "DOCKER_NETWORK", "")
	setS(&c.Runner.Exec.Dir, *execDir, "EXEC_DIR", "")
	setS(&c.Runner.Endpoint, *endpoint, "TASK_ENDPOINT", "")
	if *port != 0 {
		c.Port = *port
	} else if p := atoi(os.Getenv("LOCALAWS_PORT")); p != 0 {
		c.Port = p
	} else if c.Port == 0 {
		c.Port = 4566
	}
	setS(&c.PublicURL, *public, "PUBLIC_URL", fmt.Sprintf("http://localhost:%d", c.Port))
	if *scale > 0 {
		c.TimeScale = *scale
	} else if f, err := strconv.ParseFloat(os.Getenv("LOCALAWS_TIME_SCALE"), 64); err == nil && f > 0 {
		c.TimeScale = f
	} else if c.TimeScale <= 0 {
		c.TimeScale = 1
	}
	c.LogAPI = c.LogAPI || *logAPI || os.Getenv("LOCALAWS_LOG_API") == "1"
	if c.Runner.Docker.PauseImage == "" {
		c.Runner.Docker.PauseImage = "alpine:3"
	}
	if c.Runner.Docker.HostGateway == nil {
		t := true
		c.Runner.Docker.HostGateway = &t
	}
	switch c.Runner.Mode {
	case "docker", "exec", "noop":
	default:
		return nil, fmt.Errorf("runner must be docker, exec or noop (got %q)", c.Runner.Mode)
	}
	if c.file != "" { // second pass: ${ACCOUNT} / ${REGION} now known
		raw, _ := os.ReadFile(c.file)
		var res struct {
			Resources Resources `json:"resources"`
		}
		if err := json.Unmarshal([]byte(expand(string(raw), c)), &res); err != nil {
			return nil, err
		}
		c.Resources = res.Resources
	}
	return c, nil
}

// ── seeding ──────────────────────────────────────────────────────────────
func (a *App) seed() error {
	r := a.cfg.Resources
	rel := func(p string) string {
		if filepath.IsAbs(p) || a.cfg.file == "" {
			return p
		}
		return filepath.Join(filepath.Dir(a.cfg.file), p)
	}
	for _, b := range r.S3.Buckets {
		var cur Bucket
		if !a.db.get("bucket", b.Name, &cur) {
			cur = Bucket{Name: b.Name}
		}
		if b.Versioning {
			cur.Versioning = "Enabled"
		}
		cur.EventBridge = cur.EventBridge || b.EventBridge
		if err := a.createBucket(cur); err != nil {
			return err
		}
	}
	for _, c := range r.ECS.Clusters {
		if !a.db.has("cluster", c) {
			_ = a.db.put("cluster", c, Cluster{Name: c, Created: nowMs()})
		}
	}
	if !a.db.has("cluster", "default") {
		_ = a.db.put("cluster", "default", Cluster{Name: "default", Created: nowMs()})
	}
	for _, spec := range r.ECS.TaskDefinitions {
		if f := getStr(spec, "specFile"); f != "" {
			raw, err := os.ReadFile(rel(f))
			if err != nil {
				return err
			}
			spec = M{}
			if err := json.Unmarshal([]byte(expand(string(raw), &a.cfg)), &spec); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		}
		cur, ok := a.findTaskDef(getStr(spec, "family"))
		want := M{}
		for k, v := range spec {
			if k != "revision" {
				want[k] = v
			}
		}
		if ok && jsonStr(cur.Spec) == jsonStr(want) {
			continue
		}
		if _, e := a.registerTaskDef(spec); e != nil {
			return fmt.Errorf("task definition %s: %s", getStr(spec, "family"), e.Msg)
		}
	}
	for _, s := range r.ECS.Services {
		if _, e := a.ecsAPI("CreateService", s); e != nil && !strings.Contains(e.Msg, "idempotent") {
			return fmt.Errorf("service %s: %s", getStr(s, "serviceName"), e.Msg)
		}
	}
	for _, sm := range r.StepFunctions.StateMachines {
		def := sm.Definition
		if sm.DefinitionFile != "" {
			raw, err := os.ReadFile(rel(sm.DefinitionFile))
			if err != nil {
				return err
			}
			def = expand(string(raw), &a.cfg)
		}
		text, isStr := def.(string)
		if !isStr {
			text = jsonStr(def)
		}
		var cur StateMachine
		if a.db.get("statemachine", sm.Name, &cur) && cur.Definition == text && cur.RoleArn == sm.RoleArn {
			continue
		}
		role := sm.RoleArn
		if role == "" {
			role = a.arn("iam", "role/localaws-states")
			role = strings.Replace(role, ":"+a.cfg.Region+":", "::", 1)
		}
		if e := a.createStateMachine(StateMachine{Name: sm.Name, Type: sm.Type, RoleArn: role, Definition: text, Created: cur.Created}); e != nil {
			return fmt.Errorf("state machine %s: %s", sm.Name, e.Msg)
		}
	}
	for _, rl := range r.Events.Rules {
		pat := ""
		if rl.EventPattern != nil {
			if s, ok := rl.EventPattern.(string); ok {
				pat = s
			} else {
				pat = jsonStr(rl.EventPattern)
			}
		}
		if err := a.putRule(Rule{Name: rl.Name, EventPattern: pat, ScheduleExpression: rl.ScheduleExpression, State: rl.State, Description: rl.Description}); err != nil {
			return fmt.Errorf("rule %s: %v", rl.Name, err)
		}
		var cur Rule
		a.db.get("rule", rl.Name, &cur)
		cur.Targets = rl.Targets
		_ = a.db.put("rule", rl.Name, cur)
	}
	for _, p := range r.SSM.Parameters {
		typ := p.Type
		if typ == "" {
			typ = "String"
		}
		if cur, ok := a.param(p.Name); ok && cur.Value == p.Value && cur.Type == typ {
			continue
		}
		if _, e := a.putParam(Param{Name: p.Name, Type: typ, Value: p.Value}, true); e != nil {
			return fmt.Errorf("parameter %s: %s", p.Name, e.Msg)
		}
	}
	for _, g := range r.Logs.Groups {
		a.ensureGroup(g)
	}
	for _, t := range r.SNS.Topics {
		if !a.db.has("topic", t) {
			_ = a.db.put("topic", t, Topic{Name: t, Created: nowMs()})
		}
	}
	return nil
}

// scaled applies the time scale to waits, retries and timeouts.
func (a *App) scaled(d time.Duration) time.Duration {
	return time.Duration(float64(d) * a.cfg.TimeScale)
}

var flagHelp = flag.ErrHelp
