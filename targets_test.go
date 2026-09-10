package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An install from before this change has a name-only targets table and rounds keyed
// by its ids. Importing the old list must land on the same ids, i.e. keep history.
func TestMigrationKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite3", filepath.Join(dir, "fogping.db"))
	db.Exec(`CREATE TABLE targets (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE)`)
	db.Exec(`INSERT INTO targets(name) VALUES('HK CN2')`)
	db.Close()

	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ts, _ := s.ActiveTargets(); len(ts) != 0 {
		t.Fatalf("legacy rows must come out inactive, got %+v", ts)
	}
	tdir := t.TempDir()
	os.WriteFile(filepath.Join(tdir, "ping.list"), []byte("59.43.247.1 HK CN2 pace=fast\n"), 0o644)
	if n, err := ingestLists(tdir, s); err != nil || n != 1 {
		t.Fatalf("ingest: n=%d err=%v", n, err)
	}
	ts, _ := s.ActiveTargets()
	if len(ts) != 1 || ts[0].ID != 1 || ts[0].Host != "59.43.247.1" || ts[0].Pace != "fast" {
		t.Fatalf("import should reactivate id 1 with list config, got %+v", ts)
	}
	if _, err := os.Stat(filepath.Join(tdir, "ping.list")); !os.IsNotExist(err) {
		t.Fatal("ping.list should be moved aside after import")
	}
	if b, _ := os.ReadFile(filepath.Join(tdir, "ping.list.imported")); !strings.Contains(string(b), "HK CN2") {
		t.Fatalf("archive missing content: %q", b)
	}
}

func TestIngestRejectsWholeFile(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	tdir := t.TempDir()
	os.WriteFile(filepath.Join(tdir, "tcp.list"), []byte("10.0.0.5:443 ok\nnoport broken\n"), 0o644)
	if _, err := ingestLists(tdir, s); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want line-2 error, got %v", err)
	}
	if ts, _ := s.ActiveTargets(); len(ts) != 0 {
		t.Fatalf("a bad line must reject the whole file, got %+v", ts)
	}
	if _, err := os.Stat(filepath.Join(tdir, "tcp.list.rejected")); err != nil {
		t.Fatal("bad file should be parked as .rejected")
	}
}

func TestTargetCRUDSemantics(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	a, err := s.CreateTarget(TargetCfg{Name: "a", Type: "icmp", Host: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTarget(TargetCfg{Name: "a", Type: "icmp", Host: "192.0.2.9"}); err != errNameTaken {
		t.Fatalf("create over an active name must conflict, got %v", err)
	}
	b, _ := s.CreateTarget(TargetCfg{Name: "b", Type: "icmp", Host: "192.0.2.2"})
	b.Name = "a"
	if _, err := s.UpdateTarget(b); err != errNameTaken {
		t.Fatalf("rename onto an existing name must conflict, got %v", err)
	}
	a.Name = "a-renamed"
	if _, err := s.UpdateTarget(a); err != nil {
		t.Fatal(err)
	}
	if err := s.DeactivateTarget(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeactivateTarget(a.ID); err != errNotFound {
		t.Fatalf("double delete: %v", err)
	}
	again, err := s.CreateTarget(TargetCfg{Name: "a-renamed", Type: "tcp", Host: "192.0.2.3", Port: 22})
	if err != nil || again.ID != a.ID {
		t.Fatalf("re-adding a deleted name should reactivate id %d, got %+v %v", a.ID, again, err)
	}
}

func TestRunnerRenameKeepsRing(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	tg, _ := s.CreateTarget(TargetCfg{Name: "old", Type: "tcp", Host: "127.0.0.1", Port: 1, IntervalSec: 3600})
	r := NewRunner(ProbeCfg{Packets: 1, TimeoutMs: 50}, s, NewDetector(s))
	r.Reload()
	time.Sleep(200 * time.Millisecond) // first round runs immediately
	tg.Name = "new"
	s.UpdateTarget(tg)
	r.Reload()
	if len(s.Recent("new", 0)) == 0 {
		t.Fatal("rename dropped the in-memory ring")
	}
	if got := r.Targets(); len(got) != 1 || got[0].Name != "new" {
		t.Fatalf("runner state: %+v", got)
	}
}

func TestWebWriteGates(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	run := NewRunner(ProbeCfg{Packets: 1, TimeoutMs: 50}, s, NewDetector(s))
	body := `{"type":"tcp","host":"127.0.0.1","port":1,"interval_sec":3600}`
	do := func(h http.Handler, ct, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://fp.local/api/targets", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	ro := newMux(&Config{}, s, run, nil)
	if w := do(ro, "application/json", ""); w.Code != http.StatusForbidden {
		t.Fatalf("read-only instance must refuse writes, got %d", w.Code)
	}
	rw := newMux(&Config{Editable: true}, s, run, nil)
	if w := do(rw, "text/plain", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON body must be refused (CSRF), got %d", w.Code)
	}
	if w := do(rw, "application/json", "http://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin must be refused, got %d", w.Code)
	}
	if w := do(rw, "application/json", "http://fp.local"); w.Code != http.StatusOK {
		t.Fatalf("same-origin create failed: %d %s", w.Code, w.Body)
	}
	if got := run.Targets(); len(got) != 1 || got[0].Name != "127.0.0.1:1" {
		t.Fatalf("created target not running: %+v", got)
	}
	if w := do(rw, "application/json", ""); w.Code != http.StatusConflict {
		t.Fatalf("duplicate create should 409, got %d", w.Code)
	}
}
