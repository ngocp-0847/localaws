package main

// How an ECS task actually runs.
//
//   docker  every container of the task as `docker run --rm`. A task with several
//           containers gets a pause container that owns the network namespace and every
//           container joins it (--network container:<pause>) — what ECS does for awsvpc:
//           one localhost for all containers, whichever exits first. Image names can be
//           mapped (ECR → local tag).
//   exec    the first essential container's entryPoint + command as a process on this
//           host (working directory runner.exec.dir); for images you build locally anyway.
//   noop    nothing runs: the container "prints" its command and exits — with the code
//           and after the delay given by LOCALAWS_NOOP_EXIT / LOCALAWS_NOOP_SLEEP in its
//           environment. For exercising orchestration (state machines, retries) alone.
//
// When an essential container exits the task stops and the others are killed, as on ECS.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Runner interface {
	Run(t *Task, started func(), emit func(*Container, string)) (map[string]int, error)
	Stop(t *Task)
}

func (a *App) runner() Runner {
	switch a.cfg.Runner.Mode {
	case "exec":
		return &execRunner{a: a}
	case "noop":
		return &noopRunner{a: a}
	default:
		return &dockerRunner{a: a}
	}
}

func readBlob(a *App, o s3Object) ([]byte, error) { return os.ReadFile(a.db.blobPath(o.Blob)) }

func envMap(kv []KV) map[string]string {
	m := map[string]string{}
	for _, x := range kv {
		m[x.Name] = x.Value
	}
	return m
}

// pump streams a reader line by line to emit (lines up to 8 MiB).
func pump(wg *sync.WaitGroup, r io.Reader, c *Container, emit func(*Container, string)) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		emit(c, sc.Text())
	}
}

// ── process runner core (shared by docker and exec) ───────────────────────
type proc struct {
	cmd  *exec.Cmd
	ctr  *Container
	wg   sync.WaitGroup
	code int
	err  error
}

func startProc(argv, env []string, dir string, c *Container, emit func(*Container, string)) (*proc, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = env, dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, ctr: c}
	p.wg.Add(2)
	go pump(&p.wg, out, c, emit)
	go pump(&p.wg, errp, c, emit)
	return p, nil
}

// wait returns when the process itself exits — not when a background child it left
// behind lets go of the pipes (a container's end takes those with it).
func (p *proc) wait() {
	state, err := p.cmd.Process.Wait()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		killProcessTree(p.cmd)
		<-done
	}
	killProcessTree(p.cmd)
	if err != nil {
		p.err = err
		return
	}
	p.code = state.ExitCode()
}

// ── docker ───────────────────────────────────────────────────────────────
type dockerRunner struct{ a *App }

func containerName(t *Task, c *Container) string {
	return "localaws-" + t.ID[:12] + "-" + sanitize(c.Name)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, s)
}

func pauseName(t *Task) string { return "localaws-" + t.ID[:12] + "-pause" }

func (d *dockerRunner) netArgs() []string {
	cfg := d.a.cfg.Runner
	var args []string
	if cfg.Docker.Network != "" {
		args = append(args, "--network", cfg.Docker.Network)
	}
	if cfg.Docker.HostGateway != nil && *cfg.Docker.HostGateway {
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	return args
}

func (d *dockerRunner) args(t *Task, c *Container, ns string) []string {
	cfg := d.a.cfg.Runner
	args := []string{"run", "--rm", "--name", containerName(t, c), "--label", "localaws.task=" + t.Arn,
		"--label", "localaws.container=" + c.Name}
	if ns != "" {
		args = append(args, "--network", "container:"+ns)
	} else {
		args = append(args, d.netArgs()...)
	}
	for _, kv := range c.Env {
		args = append(args, "-e", kv.Name+"="+kv.Value)
	}
	args = append(args, cfg.Docker.ExtraArgs...)
	if len(c.EntryPoint) > 0 {
		args = append(args, "--entrypoint", c.EntryPoint[0])
	}
	image := c.Image
	if m, ok := cfg.Docker.Images[image]; ok {
		image = m
	}
	args = append(args, image)
	if len(c.EntryPoint) > 1 {
		args = append(args, c.EntryPoint[1:]...)
	}
	return append(args, c.Command...)
}

func (d *dockerRunner) Run(t *Task, started func(), emit func(*Container, string)) (map[string]int, error) {
	ns := ""
	if len(t.Containers) > 1 {
		ns = pauseName(t)
		args := append([]string{"run", "-d", "--rm", "--name", ns, "--label", "localaws.task=" + t.Arn}, d.netArgs()...)
		args = append(args, "--entrypoint", "sleep", d.a.cfg.Runner.Docker.PauseImage, "2147483647")
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("pause container: %v: %s", err, strings.TrimSpace(string(out)))
		}
		defer func() { _ = exec.Command("docker", "kill", ns).Run() }()
	}
	var procs []*proc
	for i := range t.Containers {
		c := &t.Containers[i]
		p, err := startProc(append([]string{"docker"}, d.args(t, c, ns)...), os.Environ(), "", c, emit)
		if err != nil {
			d.Stop(t)
			return nil, err
		}
		procs = append(procs, p)
	}
	started()
	return waitEssential(procs, func() { d.Stop(t) })
}

func (d *dockerRunner) Stop(t *Task) {
	names := []string{"kill"}
	for i := range t.Containers {
		names = append(names, containerName(t, &t.Containers[i]))
	}
	_ = exec.Command("docker", names...).Run()
}

// waitEssential collects exit codes; the first essential exit stops the rest.
func waitEssential(procs []*proc, stopAll func()) (map[string]int, error) {
	type res struct{ p *proc }
	ch := make(chan res, len(procs))
	for _, p := range procs {
		go func(p *proc) { p.wait(); ch <- res{p} }(p)
	}
	codes := map[string]int{}
	var firstErr error
	stopped := false
	for range procs {
		r := <-ch
		if r.p.err != nil && firstErr == nil {
			firstErr = r.p.err
		}
		codes[r.p.ctr.Name] = r.p.code
		if r.p.code == 125 && firstErr == nil {
			firstErr = fmt.Errorf("docker could not start container %s (exit 125) — see its log stream", r.p.ctr.Name)
		}
		if r.p.ctr.Essential && !stopped {
			stopped = true
			stopAll()
		}
	}
	return codes, firstErr
}

// ── exec ─────────────────────────────────────────────────────────────────
type execRunner struct {
	a    *App
	mu   sync.Mutex
	cmds map[string]*exec.Cmd
}

func (x *execRunner) Run(t *Task, started func(), emit func(*Container, string)) (map[string]int, error) {
	var c *Container
	for i := range t.Containers {
		if t.Containers[i].Essential {
			c = &t.Containers[i]
			break
		}
	}
	if c == nil {
		return nil, errors.New("no essential container")
	}
	argv := append(append([]string{}, c.EntryPoint...), c.Command...)
	if len(argv) == 0 {
		return nil, errors.New("runner exec: the container has neither entryPoint nor command")
	}
	env := os.Environ()
	for _, kv := range c.Env {
		env = append(env, kv.Name+"="+kv.Value)
	}
	p, err := startProc(argv, env, x.a.cfg.Runner.Exec.Dir, c, emit)
	if err != nil {
		return nil, err
	}
	x.a.execs.Store(t.ID, p.cmd)
	defer x.a.execs.Delete(t.ID)
	started()
	p.wait()
	return map[string]int{c.Name: p.code}, p.err
}

func (x *execRunner) Stop(t *Task) {
	if v, ok := x.a.execs.Load(t.ID); ok {
		killProcessTree(v.(*exec.Cmd))
	}
}

// ── noop ─────────────────────────────────────────────────────────────────
type noopRunner struct{ a *App }

func (n *noopRunner) Run(t *Task, started func(), emit func(*Container, string)) (map[string]int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	n.a.noops.Store(t.ID, cancel)
	defer n.a.noops.Delete(t.ID)
	started()
	codes := map[string]int{}
	for i := range t.Containers {
		c := &t.Containers[i]
		env := envMap(c.Env)
		emit(c, fmt.Sprintf("localaws noop runner: %s %s", strings.Join(c.EntryPoint, " "), strings.Join(c.Command, " ")))
		d, _ := time.ParseDuration(env["LOCALAWS_NOOP_SLEEP"])
		if d == 0 {
			d = 200 * time.Millisecond
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
		codes[c.Name] = atoi(env["LOCALAWS_NOOP_EXIT"])
	}
	return codes, nil
}

func (n *noopRunner) Stop(t *Task) {
	if v, ok := n.a.noops.Load(t.ID); ok {
		v.(context.CancelFunc)()
	}
}
