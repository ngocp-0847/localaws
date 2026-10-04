package main

// The web console (console/, React) is built into console/dist and embedded in the binary,
// so `go build` needs no Node and the emulator stays one file. Served at /_localaws/;
// it talks to this same origin with the real AWS protocols, plus two small admin endpoints:
//
//	GET /_localaws/api/info    account, region, identity, runner, resource counts
//	GET /_localaws/api/calls   the last API calls received

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
)

//go:embed all:console/dist
var consoleFS embed.FS

type asset struct {
	body, gz []byte
	ctype    string
}

var (
	assets     map[string]asset
	assetsOnce sync.Once
)

func loadAssets() {
	assets = map[string]asset{}
	sub, err := fs.Sub(consoleFS, "console/dist")
	if err != nil {
		return
	}
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return nil
		}
		a := asset{body: b, ctype: mime.TypeByExtension(path.Ext(p))}
		if a.ctype == "" {
			a.ctype = "application/octet-stream"
		}
		if len(b) > 1024 && (strings.HasPrefix(a.ctype, "text/") || strings.Contains(a.ctype, "javascript") || strings.Contains(a.ctype, "json") || strings.Contains(a.ctype, "svg")) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = zw.Write(b)
			_ = zw.Close()
			a.gz = buf.Bytes()
		}
		assets[p] = a
		return nil
	})
}

// serveConsole answers /_localaws/<path>: a built file, else index.html (hash routing).
func (a *App) serveConsole(w http.ResponseWriter, r *http.Request, p string) {
	assetsOnce.Do(loadAssets)
	p = strings.TrimPrefix(p, "/")
	f, ok := assets[p]
	if !ok {
		p = "index.html"
		f, ok = assets[p]
	}
	if !ok {
		http.Error(w, "the console was not built into this binary (cd console && npm ci && npm run build)", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", f.ctype)
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if f.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		_, _ = w.Write(f.gz)
		return
	}
	_, _ = w.Write(f.body)
}

func (a *App) consoleInfo() M {
	running := 0
	for _, e := range list[Execution](a.db, "execution") {
		if e.Status == "RUNNING" {
			running++
		}
	}
	tasks := 0
	for _, t := range list[Task](a.db, "task") {
		if t.Status != "STOPPED" {
			tasks++
		}
	}
	return M{"version": version, "account": a.cfg.Account, "region": a.cfg.Region, "identity": a.cfg.IdentityArn(),
		"runner": a.cfg.Runner.Mode, "started": a.started, "endpoint": a.cfg.PublicURL, "dataDir": a.cfg.DataDir,
		"counts": M{"buckets": len(list[Bucket](a.db, "bucket")), "stateMachines": len(list[StateMachine](a.db, "statemachine")),
			"executionsRunning": running, "tasksRunning": tasks, "rules": len(list[Rule](a.db, "rule")),
			"parameters": len(list[Param](a.db, "param")), "logGroups": len(list[LogGroup](a.db, "loggroup"))}}
}

func (a *App) consoleCalls() []apiCall {
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	out := make([]apiCall, len(a.apiLog))
	copy(out, a.apiLog)
	return out
}
