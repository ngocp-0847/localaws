// localaws — a small AWS emulator that runs things for real.
//
// One static binary, one port, one SQLite file. S3, EventBridge, Step Functions, ECS,
// CloudWatch Logs, SSM Parameter Store, STS, EC2 (networks) and SNS answer the AWS
// SDKs and CLI unchanged (point AWS_ENDPOINT_URL at it); state machines are interpreted,
// ECS tasks run as real containers or processes, and everything they do lands in the
// same places it would on AWS: execution histories, task states, log streams, objects.
//
//	localaws                                   serve on :4566 (UI at /_localaws/)
//	localaws -config env.json -runner docker   bring up a declared environment
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.1.0"

type apiCall struct {
	At     int64
	What   string
	Status int
	Ms     int64
}

type App struct {
	cfg      Config
	db       *Store
	started  int64
	runs     map[string]*run
	runsMu   sync.Mutex
	taskRuns map[string]*taskRun
	tasksMu  sync.Mutex
	execs    sync.Map // task id → *exec.Cmd (runner=exec)
	noops    sync.Map // task id → cancel (runner=noop)
	apiMu    sync.Mutex
	apiLog   []apiCall
	stop     chan struct{}
}

func (a *App) logf(format string, args ...any) { log.Printf(format, args...) }

func (a *App) sleep(ctx context.Context, d time.Duration) bool {
	d = a.scaled(d)
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func newApp(cfg *Config) (*App, error) {
	db, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: *cfg, db: db, started: nowMs(), runs: map[string]*run{}, taskRuns: map[string]*taskRun{}, stop: make(chan struct{})}
	a.recover()
	if err := a.seed(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return a, nil
}

// recover settles what a previous process left running: executions are aborted and
// tasks stopped, as AWS would report them after losing their host.
func (a *App) recover() {
	for _, e := range list[Execution](a.db, "execution") {
		if e.Status == "RUNNING" {
			r := &run{a: a, e: e}
			r.event("ExecutionAborted", e.LastID, M{"executionAbortedEventDetails": M{"error": "localaws.Restarted", "cause": "the emulator restarted while the execution was running"}})
			r.e.Status, r.e.Stop, r.e.Error, r.e.Cause = "ABORTED", nowMs(), "localaws.Restarted", "the emulator restarted while the execution was running"
			r.save()
		}
	}
	for _, t := range list[Task](a.db, "task") {
		if t.Status != "STOPPED" {
			t.Status, t.Desired, t.StopCode, t.Stopped = "STOPPED", "STOPPED", "TaskFailedToStart", nowMs()
			t.StoppedReason = "Host stopped: the emulator restarted while the task was running"
			_ = a.db.put("task", t.ID, t)
		}
		tr := a.taskRunOf(t.ID)
		tr.once.Do(func() { close(tr.done) })
	}
}

// Close stops every running execution and task and closes the database.
func (a *App) Close() {
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
	a.runsMu.Lock()
	for _, rn := range a.runs {
		rn.cancel()
	}
	a.runsMu.Unlock()
	for _, t := range list[Task](a.db, "task") {
		if t.Status != "STOPPED" {
			a.stopTask(t.ID, "emulator shutting down")
			select {
			case <-a.taskDone(t.ID):
			case <-time.After(3 * time.Second):
			}
		}
	}
	_ = a.db.db.Close()
}

func (a *App) record(what string, status int, d time.Duration) {
	a.apiMu.Lock()
	a.apiLog = append(a.apiLog, apiCall{nowMs(), what, status, d.Milliseconds()})
	if len(a.apiLog) > 1000 {
		a.apiLog = a.apiLog[len(a.apiLog)-1000:]
	}
	a.apiMu.Unlock()
	if a.cfg.LogAPI {
		a.logf("api %-56s %d %4dms", what, status, d.Milliseconds())
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

var credScope = regexp.MustCompile(`Credential=[^/,]+/\d{8}/[^/]+/([a-z0-9-]+)/aws4_request`)

func signingService(r *http.Request) string {
	if m := credScope.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		return m[1]
	}
	if m := credScope.FindStringSubmatch("Credential=" + r.URL.Query().Get("X-Amz-Credential")); m != nil {
		return m[1]
	}
	return ""
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{w, 200}
	t0 := time.Now()
	what := "s3 " + r.Method + " " + r.URL.Path
	defer func() {
		if rec := recover(); rec != nil {
			a.logf("panic serving %s: %v", what, rec)
			http.Error(sw, fmt.Sprint(rec), 500)
		}
		if !strings.HasPrefix(r.URL.Path, "/_localaws") {
			a.record(what, sw.status, time.Since(t0))
		}
	}()
	if strings.HasPrefix(r.URL.Path, "/_localaws") {
		a.admin(sw, r)
		return
	}
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		svc, op, _ := strings.Cut(target, ".")
		what = svc + "." + op
		in := readJSON(r)
		var out any
		var e *apiError
		switch {
		case strings.HasPrefix(svc, "AWSStepFunctions"):
			out, e = a.sfnAPI(op, in)
		case strings.HasPrefix(svc, "AmazonEC2ContainerService"):
			out, e = a.ecsAPI(op, in)
		case strings.HasPrefix(svc, "Logs_"):
			out, e = a.logsAPI(op, in)
		case strings.HasPrefix(svc, "AmazonSSM"):
			out, e = a.ssmAPI(op, in)
		case strings.HasPrefix(svc, "AWSEvents"):
			out, e = a.eventsAPI(op, in)
		default:
			e = errf(400, "UnknownOperationException", "localaws: service %s is not emulated", svc)
		}
		if e != nil {
			writeJSONErr(sw, e)
			return
		}
		writeJSON(sw, 200, out)
		return
	}
	svc := signingService(r)
	if r.Method == http.MethodPost && svc != "s3" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		if act := vals.Get("Action"); act != "" {
			what = svc + "." + act
			out, e := a.queryAPI(svc, vals)
			if e != nil {
				queryErr(sw, e)
				return
			}
			writeXML(sw, 200, out)
			return
		}
	}
	a.s3(sw, r)
}

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	switch p := strings.TrimPrefix(r.URL.Path, "/_localaws"); {
	case p == "/health":
		writeJSON(w, 200, M{"status": "running", "version": version, "runner": a.cfg.Runner.Mode, "account": a.cfg.Account,
			"region": a.cfg.Region, "services": []string{"s3", "events", "states", "ecs", "logs", "ssm", "sts", "ec2", "sns"}})
	case p == "/reset" && r.Method == http.MethodPost:
		a.runsMu.Lock()
		for _, rn := range a.runs {
			rn.cancel()
		}
		a.runs = map[string]*run{}
		a.runsMu.Unlock()
		for _, t := range list[Task](a.db, "task") {
			a.stopTask(t.ID, "reset")
		}
		if err := a.db.reset(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a.tasksMu.Lock()
		a.taskRuns = map[string]*taskRun{}
		a.tasksMu.Unlock()
		if err := a.seed(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, 200, M{"reset": true})
	case p == "/api/info":
		writeJSON(w, 200, a.consoleInfo())
	case p == "/api/calls":
		writeJSON(w, 200, a.consoleCalls())
	default:
		a.serveConsole(w, r, p)
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	cfg, err := loadConfig(os.Args[1:])
	if errors.Is(err, flagHelp) {
		return
	}
	if err != nil {
		log.Fatalf("localaws: %v", err)
	}
	a, err := newApp(cfg)
	if err != nil {
		log.Fatalf("localaws: %v", err)
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Port), Handler: a, ReadHeaderTimeout: 30 * time.Second}
	go a.scheduler(a.stop)
	go a.reconcile(a.stop)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("localaws: %v", err)
		}
	}()
	log.Printf("localaws %s on :%d — account %s, region %s, runner %s, data %s — UI %s/_localaws/",
		version, cfg.Port, cfg.Account, cfg.Region, cfg.Runner.Mode, cfg.DataDir, cfg.PublicURL)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	a.Close()
}
