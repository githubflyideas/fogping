package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebTargetsExportImport(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	run := NewRunner(ProbeCfg{Packets: 1, TimeoutMs: 50}, s, NewDetector(s))
	file := `{"format":"netping-targets","version":1,"targets":[
		{"name":"a","type":"tcp","host":"127.0.0.1","port":1,"interval":3600},
		{"name":"b","type":"ping","host":"127.0.0.1","interval":3600},
		{"name":"bad","type":"tcp","host":"127.0.0.1"}]}`
	do := func(h http.Handler, method, path, ct, origin, body, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://fp.local"+path, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if cookie != "" {
			req.Header.Set("Cookie", "fogping_session="+cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	plan := func(w *httptest.ResponseRecorder) importPlan {
		var p importPlan
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("bad plan JSON %q: %v", w.Body, err)
		}
		return p
	}

	// Read-only instance: export works, import is refused before the body is read.
	ro := newMux(&Config{}, s, run, nil)
	if w := do(ro, "GET", "/api/targets/export", "", "", "", ""); w.Code != 200 {
		t.Fatalf("export on a read-only instance: %d", w.Code)
	}
	if w := do(ro, "POST", "/api/targets/import?dry_run=1", "application/json", "", file, ""); w.Code != http.StatusForbidden {
		t.Fatalf("read-only import must be 403, got %d", w.Code)
	}

	rw := newMux(&Config{Editable: true}, s, run, nil)
	if w := do(rw, "POST", "/api/targets/import", "text/plain", "", file, ""); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON import must be 415, got %d", w.Code)
	}
	if w := do(rw, "POST", "/api/targets/import", "application/json", "http://evil.example", file, ""); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin import must be 403, got %d", w.Code)
	}
	big := `{"format":"netping-targets","version":1,"targets":[],"pad":"` + strings.Repeat("x", maxImportBytes) + `"}`
	if w := do(rw, "POST", "/api/targets/import", "application/json", "", big, ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize import must be 413, got %d", w.Code)
	}
	if w := do(rw, "POST", "/api/targets/import", "application/json", "", `{"format":"netping-targets","version":9,"targets":[]}`, ""); w.Code != 400 {
		t.Fatalf("future version must be 400, got %d", w.Code)
	}

	w := do(rw, "POST", "/api/targets/import?dry_run=1", "application/json", "http://fp.local", file, "")
	if p := plan(w); w.Code != 200 || !p.DryRun || len(p.Add) != 2 || len(p.Invalid) != 1 || p.Imported != 0 {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	if len(run.Targets()) != 0 {
		t.Fatal("dry run changed the target list")
	}
	w = do(rw, "POST", "/api/targets/import", "application/json", "http://fp.local", file, "")
	if p := plan(w); w.Code != 200 || p.Imported != 2 {
		t.Fatalf("import: %d %s", w.Code, w.Body)
	}
	if got := run.Targets(); len(got) != 2 {
		t.Fatalf("runner not reloaded: %+v", got)
	}
	w = do(rw, "POST", "/api/targets/import", "application/json", "", file, "")
	if p := plan(w); w.Code != 200 || p.Imported != 0 || len(p.Skip) != 2 {
		t.Fatalf("second import should skip both: %d %s", w.Code, w.Body)
	}

	w = do(rw, "GET", "/api/targets/export", "", "", "", "")
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="fogping-targets-`) {
		t.Fatalf("Content-Disposition %q", cd)
	}
	var f targetsFile
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil || f.Format != targetsFormat || len(f.Targets) != 2 ||
		f.Targets[0].Name != "a" || f.Targets[1].Type != "ping" {
		t.Fatalf("export body: %v %s", err, w.Body)
	}

	// With accounts configured, both need a session.
	authed := newMux(&Config{Editable: true}, s, run, map[string]string{"u": "p"})
	if w := do(authed, "GET", "/api/targets/export", "", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("export without a session must be 401, got %d", w.Code)
	}
	if w := do(authed, "POST", "/api/targets/import", "application/json", "", file, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("import without a session must be 401, got %d", w.Code)
	}
	lw := do(authed, "POST", "/api/login", "application/json", "", `{"User":"u","Pass":"p"}`, "")
	var tok string
	for _, c := range lw.Result().Cookies() {
		if c.Name == "fogping_session" {
			tok = c.Value
		}
	}
	if w := do(authed, "GET", "/api/targets/export", "", "", "", tok); w.Code != 200 {
		t.Fatalf("export with a session: %d", w.Code)
	}
	if w := do(authed, "POST", "/api/targets/import?dry_run=1", "application/json", "", file, tok); w.Code != 200 {
		t.Fatalf("import with a session: %d %s", w.Code, w.Body)
	}
}
