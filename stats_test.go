package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testStatsToken = "zabbix-test-token-0123456789"

func statsStore(t *testing.T) (*Store, []TargetCfg) {
	t.Helper()
	targets := []TargetCfg{
		{ID: 1, Name: "steady", Type: "tcp", Host: "10.9.8.7", Port: 443},
		{ID: 2, Name: "dead", Type: "icmp", Host: "192.0.2.99"},
		{ID: 3, Name: "slow", Type: "icmp", Host: "192.0.2.50"},
	}
	s, err := NewStore(t.TempDir(), targets)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now().Unix()
	// Oldest first, as the probes write them.
	s.Append("steady", Round{T: now - 3600, S: 10, R: 10, MS: []float64{999}}) // outside: ignored
	s.Append("slow", Round{T: now - 3600, S: 10, R: 10, MS: []float64{5}})
	for i := 3; i >= 0; i-- { // inside the 5-minute window
		s.Append("steady", Round{T: now - int64(i)*60, S: 10, R: 9, MS: []float64{10, 20, 30, 40, 50, 60, 70, 80, 90}, B: i == 0})
		s.Append("dead", Round{T: now - int64(i)*60, S: 10, R: 0})
	}
	return s, targets
}

func TestBuildStats(t *testing.T) {
	s, targets := statsStore(t)
	rep := buildStats(s, targets, time.Now())
	if rep.WindowSec != 300 || len(rep.Targets) != 3 {
		t.Fatalf("report: %+v", rep)
	}
	st, dead, slow := rep.Targets[0], rep.Targets[1], rep.Targets[2]
	if st.ID != 1 || st.Rounds != 4 || st.P50 == nil || *st.P50 != 50 || *st.P99 != 90 || *st.LossPct != 10 || st.Bursts != 1 || *st.Up != 1 {
		t.Errorf("steady: %+v p50=%v p99=%v", st, deref(st.P50), deref(st.P99))
	}
	if dead.Rounds != 4 || dead.P50 != nil || *dead.LossPct != 100 || *dead.Up != 0 {
		t.Errorf("dead: %+v", dead)
	}
	if slow.Rounds != 0 || slow.P50 != nil || slow.LossPct != nil || slow.Up != nil {
		t.Errorf("slow (no rounds in window) must carry no numbers, not zeros: %+v", slow)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Off by default: with no token the route does not exist, exactly as before.
func TestStatsOffByDefault(t *testing.T) {
	s, _ := statsStore(t)
	h := newMux(&Config{}, s, NewRunner(ProbeCfg{}, s, NewDetector(s)), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/stats", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("stats without a token: %d, want 404", w.Code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	s, _ := statsStore(t)
	h := newMux(&Config{StatsToken: testStatsToken}, s, NewRunner(ProbeCfg{}, s, NewDetector(s)), map[string]string{"admin": "pw"})
	do := func(method, auth, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/stats", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if remote != "" {
			req.RemoteAddr = remote
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	for _, auth := range []string{"", "Bearer wrong-token-0123456789", testStatsToken, "Basic " + testStatsToken} {
		w := do("GET", auth, "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: %d, want 401", auth, w.Code)
		}
		if body := w.Body.String(); strings.Contains(body, "steady") || strings.Contains(body, version) && version != "" {
			t.Errorf("401 body leaks: %s", body)
		}
	}

	w := do("GET", "Bearer "+testStatsToken, "198.51.100.1:1")
	if w.Code != 200 {
		t.Fatalf("good token: %d %s", w.Code, w.Body)
	}
	checkSecurityHeaders(t, "/api/stats", w)
	body := w.Body.String()
	for _, leak := range []string{"10.9.8.7", "443", "192.0.2.99", `"host"`, `"port"`, `"type"`, `"version"`} {
		if strings.Contains(body, leak) {
			t.Errorf("stats response exposes %s: %s", leak, body)
		}
	}
	var rep statsReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || len(rep.Targets) != 0 {
		// The runner in this test has no live targets; buildStats is covered above.
		t.Fatalf("decode: %v %s", err, body)
	}

	// Read-only: every write method is refused and changes nothing.
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if w := do(m, "Bearer "+testStatsToken, ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, w.Code)
		}
	}
	if len(s.Recent("steady", 0)) != 5 {
		t.Error("a refused write changed the store")
	}

	// Wrong tokens are throttled like wrong passwords.
	old := loginFailDelay
	loginFailDelay = 0
	defer func() { loginFailDelay = old }()
	for i := 0; i <= loginFreeFails; i++ {
		do("GET", "Bearer nope-nope-nope-nope", "203.0.113.9:1")
	}
	if w := do("GET", "Bearer "+testStatsToken, "203.0.113.9:1"); w.Code != http.StatusTooManyRequests {
		t.Errorf("right token from a locked address: %d, want 429", w.Code)
	}
}

func TestResolveZabbixToken(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	os.WriteFile(f, []byte(testStatsToken+"\n"), 0600)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	withTok := env(map[string]string{"FOGPING_ZABBIX_TOKEN": testStatsToken})

	// Default: off, login args untouched.
	if tok, rest, ign, err := resolveZabbixToken(false, []string{"user=a", "passwd=b"}, env(nil)); err != nil || tok != "" || ign || len(rest) != 2 {
		t.Errorf("default must be off: %q %v %v %v", tok, rest, ign, err)
	}
	// A token without --zabbix is ignored (and reported), never an error.
	if tok, rest, ign, err := resolveZabbixToken(false, []string{"user=a", "zabbix-token-file=" + f}, withTok); err != nil || tok != "" || !ign || len(rest) != 1 {
		t.Errorf("token without --zabbix: %q %v %v %v", tok, rest, ign, err)
	}
	// --zabbix without a token refuses to start.
	if _, _, _, err := resolveZabbixToken(true, nil, env(nil)); err == nil || !strings.Contains(err.Error(), "--zabbix needs a token") {
		t.Errorf("--zabbix without token: %v", err)
	}
	if tok, _, _, err := resolveZabbixToken(true, nil, withTok); err != nil || tok != testStatsToken {
		t.Errorf("env: %q %v", tok, err)
	}
	if tok, rest, _, err := resolveZabbixToken(true, []string{"user=a", "zabbix-token-file=" + f}, env(nil)); err != nil || tok != testStatsToken || len(rest) != 1 {
		t.Errorf("file arg: %q %v %v", tok, rest, err)
	}
	if tok, _, _, err := resolveZabbixToken(true, nil, env(map[string]string{"FOGPING_ZABBIX_TOKEN_FILE": f})); err != nil || tok != testStatsToken {
		t.Errorf("env file: %q %v", tok, err)
	}
	if _, _, _, err := resolveZabbixToken(true, nil, env(map[string]string{"FOGPING_ZABBIX_TOKEN": "short"})); err == nil {
		t.Error("a short token was accepted")
	}
	if _, _, _, err := resolveZabbixToken(true, nil, env(map[string]string{"FOGPING_ZABBIX_TOKEN": testStatsToken, "FOGPING_ZABBIX_TOKEN_FILE": f})); err == nil {
		t.Error("both env sources accepted")
	}
}
