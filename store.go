package main

// All state lives in one SQLite file (modernc.org/sqlite: pure Go, no CGO, one static
// binary). Object bodies are files under <data>/blobs/. Resources (buckets, rules,
// state machines, task definitions, parameters, …) are JSON documents in `resources`;
// high-volume records (objects, history events, log events) have their own tables.
// Everything survives a restart and can be inspected with any SQLite client.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	dir string
}

const schema = `
CREATE TABLE IF NOT EXISTS resources(kind TEXT, id TEXT, seq INTEGER, doc TEXT, PRIMARY KEY(kind, id));
CREATE TABLE IF NOT EXISTS s3_objects(
  bucket TEXT, key TEXT, version_id TEXT, seq INTEGER, size INTEGER, etag TEXT, modified_ms INTEGER,
  delete_marker INTEGER DEFAULT 0, blob TEXT, meta TEXT, PRIMARY KEY(bucket, key, version_id));
CREATE INDEX IF NOT EXISTS s3_objects_list ON s3_objects(bucket, key, seq);
CREATE TABLE IF NOT EXISTS s3_parts(upload_id TEXT, part INTEGER, size INTEGER, etag TEXT, blob TEXT, PRIMARY KEY(upload_id, part));
CREATE TABLE IF NOT EXISTS sfn_events(exec_arn TEXT, id INTEGER, ts_ms INTEGER, doc TEXT, PRIMARY KEY(exec_arn, id));
CREATE TABLE IF NOT EXISTS log_events(id INTEGER PRIMARY KEY AUTOINCREMENT, grp TEXT, stream TEXT, ts_ms INTEGER, ingest_ms INTEGER, message TEXT);
CREATE INDEX IF NOT EXISTS log_events_stream ON log_events(grp, stream, id);
CREATE TABLE IF NOT EXISTS counters(name TEXT PRIMARY KEY, val INTEGER);
`

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)",
		filepath.ToSlash(filepath.Join(dir, "localaws.sqlite")))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
}

func (s *Store) exec(q string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(q, args...)
	return err
}

func (s *Store) next(name string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.Exec(`INSERT INTO counters(name, val) VALUES(?, 1) ON CONFLICT(name) DO UPDATE SET val = val + 1`, name)
	var v int64
	_ = s.db.QueryRow(`SELECT val FROM counters WHERE name = ?`, name).Scan(&v)
	return v
}

// ── resource documents ───────────────────────────────────────────────────
func (s *Store) put(kind, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	seq := s.next("res")
	return s.exec(`INSERT INTO resources(kind, id, seq, doc) VALUES(?,?,?,?)
		ON CONFLICT(kind, id) DO UPDATE SET doc = excluded.doc`, kind, id, seq, string(b))
}

func (s *Store) get(kind, id string, v any) bool {
	var doc string
	if err := s.db.QueryRow(`SELECT doc FROM resources WHERE kind=? AND id=?`, kind, id).Scan(&doc); err != nil {
		return false
	}
	return json.Unmarshal([]byte(doc), v) == nil
}

func (s *Store) has(kind, id string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT count(*) FROM resources WHERE kind=? AND id=?`, kind, id).Scan(&n)
	return n > 0
}

func (s *Store) del(kind, id string) error {
	return s.exec(`DELETE FROM resources WHERE kind=? AND id=?`, kind, id)
}

// list returns the documents of one kind in creation order.
func list[T any](s *Store, kind string) []T {
	rows, err := s.db.Query(`SELECT doc FROM resources WHERE kind=? ORDER BY seq`, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var doc string
		_ = rows.Scan(&doc)
		var v T
		if json.Unmarshal([]byte(doc), &v) == nil {
			out = append(out, v)
		}
	}
	return out
}

func (s *Store) blobPath(name string) string { return filepath.Join(s.dir, "blobs", name) }

// reset wipes everything (the admin /reset endpoint); the caller re-applies the seed config.
func (s *Store) reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range []string{"resources", "s3_objects", "s3_parts", "sfn_events", "log_events", "counters"} {
		if _, err := s.db.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	_ = os.RemoveAll(filepath.Join(s.dir, "blobs"))
	return os.MkdirAll(filepath.Join(s.dir, "blobs"), 0o755)
}
