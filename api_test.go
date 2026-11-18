package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// These tests enforce the rules in docs/API.md. Each failure names the rule.

type apiRoute struct{ method, path, auth string }

// documentedRoutes reads the endpoint table in docs/API.md.
func documentedRoutes(t *testing.T) []apiRoute {
	t.Helper()
	b, err := os.ReadFile("docs/API.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`(?m)^\| (GET|POST|PUT|DELETE|PATCH) \| (/api/[^ |]*) \| ([^|]*) \|`)
	var out []apiRoute
	for _, m := range row.FindAllStringSubmatch(string(b), -1) {
		out = append(out, apiRoute{m[1], m[2], strings.TrimSpace(m[3])})
	}
	if len(out) == 0 {
		t.Fatal("no endpoint rows found in docs/API.md")
	}
	return out
}

// Rules 2 and 7: every /api route the code registers names its method, and the
// documentation lists exactly the routes the code registers.
func TestAPIRoutesDocumentedAndMethodBound(t *testing.T) {
	re := regexp.MustCompile(`HandleFunc\("(?:([A-Z]+) )?(/api/[^"]*)"`)
	files, _ := os.ReadDir(".")
	code := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f.Name())
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if m[1] == "" {
				t.Errorf("rule 2: %s registers %s without a method", f.Name(), m[2])
				continue
			}
			code[m[1]+" "+m[2]] = true
		}
	}
	doc := map[string]bool{}
	for _, r := range documentedRoutes(t) {
		doc[r.method+" "+r.path] = true
	}
	for k := range code {
		if !doc[k] {
			t.Errorf("rule 7: %s is registered but not in docs/API.md", k)
		}
	}
	for k := range doc {
		if !code[k] {
			t.Errorf("rule 7: %s is in docs/API.md but not registered", k)
		}
	}
}

// apiHarness is an instance with a login, --edit, --zabbix and one target with data.
func apiHarness(t *testing.T) (http.Handler, string, int64) {
	t.Helper()
	old := loginFailDelay
	loginFailDelay = 0
	t.Cleanup(func() { loginFailDelay = old })
	tg := TargetCfg{Name: "api-target", Type: "tcp", Host: "127.0.0.1", Port: 9}
	s, err := NewStore(t.TempDir(), []TargetCfg{tg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now().Unix()
	for i := 3; i >= 0; i-- {
		s.Append("api-target", Round{T: now - int64(i)*60, S: 10, R: 9, MS: []float64{1, 2, 3, 4, 5, 6, 7, 8, 9}})
	}
	run := NewRunner(ProbeCfg{Packets: 1, TimeoutMs: 50}, s, NewDetector(s))
	cfg := &Config{Editable: true, StatsToken: testStatsToken, RetentionDays: 93, HotDays: 2}
	h := newMux(cfg, s, run, map[string]string{"admin": "pw"})
	w := apiCall(h, "POST", "/api/login", `{"user":"admin","pass":"pw"}`, "", "")
	if w.Code != 200 {
		t.Fatalf("rule 5: login with snake_case keys: %d %s", w.Code, w.Body)
	}
	var tok string
	for _, c := range w.Result().Cookies() {
		if c.Name == "fogping_session" {
			tok = c.Value
		}
	}
	id, _ := s.targetID("api-target")
	return h, tok, id
}

func apiCall(h http.Handler, method, path, body, session, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://fp.local"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if session != "" {
		req.Header.Set("Cookie", "fogping_session="+session)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// checkAPIResponse applies rules 1 and 3 to any /api response, and rule 4 to an
// error.
func checkAPIResponse(t *testing.T, label string, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Get("API-Version"); got != apiVersion {
		t.Errorf("rule 1: %s: API-Version = %q", label, got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("rule 3: %s: Content-Type = %q", label, ct)
	}
	if w.Code < 400 {
		return
	}
	var e map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Errorf("rule 4: %s: error body is not JSON: %q", label, w.Body)
		return
	}
	msg, _ := e["error"].(string)
	code, _ := e["code"].(string)
	if len(e) != 2 || msg == "" || code != errorCode(w.Code) {
		t.Errorf("rule 4: %s: error body %s, want {error, code:%q}", label, w.Body, errorCode(w.Code))
	}
}

// concrete turns a documented path into one a request can use.
func concrete(path string, id int64) string {
	return strings.ReplaceAll(path, "{id}", itoa64(id))
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

// allowedFor lists the documented methods whose pattern matches a concrete path.
func allowedFor(routes []apiRoute, path string) map[string]bool {
	out := map[string]bool{}
	for _, r := range routes {
		pat := "^" + regexp.MustCompile(`\\\{[a-z_]+\\\}`).ReplaceAllString(regexp.QuoteMeta(r.path), `[^/]+`) + "$"
		if regexp.MustCompile(pat).MatchString(path) {
			out[r.method] = true
		}
	}
	return out
}

// Rules 1–4: wrong methods, missing credentials and unknown paths all answer in
// the one error shape, with the version header.
func TestAPIErrorsFollowTheRules(t *testing.T) {
	h, session, id := apiHarness(t)
	routes := documentedRoutes(t)
	for _, r := range routes {
		path := concrete(r.path, id)
		allowed := allowedFor(routes, path)
		for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
			if allowed[m] || (m == "HEAD" && allowed["GET"]) {
				continue
			}
			w := apiCall(h, m, path, "", session, testStatsToken)
			label := m + " " + path
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("rule 2: %s = %d, want 405", label, w.Code)
				continue
			}
			if w.Header().Get("Allow") == "" {
				t.Errorf("rule 2: %s: 405 without Allow", label)
			}
			checkAPIResponse(t, label, w)
		}
		if strings.HasPrefix(r.auth, "session") {
			w := apiCall(h, r.method, path, "", "", "")
			if w.Code != http.StatusUnauthorized {
				t.Errorf("rule 6: %s %s without a session = %d, want 401", r.method, path, w.Code)
			}
			checkAPIResponse(t, r.method+" "+path+" (no session)", w)
		}
		if r.auth == "bearer token" {
			w := apiCall(h, r.method, path, "", "", "")
			if w.Code != http.StatusUnauthorized {
				t.Errorf("rule 6: %s %s without a token = %d, want 401", r.method, path, w.Code)
			}
			checkAPIResponse(t, r.method+" "+path+" (no token)", w)
		}
	}
	for _, p := range []string{"/api/nope", "/api/targets/1/nope", "/api/"} {
		w := apiCall(h, "GET", p, "", session, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("rule 4: GET %s = %d, want 404", p, w.Code)
		}
		checkAPIResponse(t, "GET "+p, w)
	}
	w := apiCall(h, "POST", "/api/login", `{"user":"admin","pass":"wrong"}`, "", "")
	checkAPIResponse(t, "bad login", w)
	w = apiCall(h, "POST", "/api/targets", "name=x", session, "")
	checkAPIResponse(t, "form body", w)
}

var snakeKey = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

func checkKeys(t *testing.T, label string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !snakeKey.MatchString(k) {
				t.Errorf("rule 5: %s: key %q is not snake_case", label, k)
			}
			checkKeys(t, label+"."+k, x[k])
		}
	case []any:
		for _, e := range x {
			checkKeys(t, label+"[]", e)
		}
	}
}

// Rules 1, 3 and 5 on every successful read.
func TestAPIReadsFollowTheRules(t *testing.T) {
	h, session, _ := apiHarness(t)
	for _, p := range []string{"/api/version", "/api/targets", "/api/series?target=api-target&minutes=60", "/api/targets/export", "/api/stats"} {
		w := apiCall(h, "GET", p, "", session, testStatsToken)
		if w.Code != 200 {
			t.Errorf("GET %s = %d %s", p, w.Code, w.Body)
			continue
		}
		checkAPIResponse(t, "GET "+p, w)
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Errorf("GET %s: not JSON: %v", p, err)
			continue
		}
		checkKeys(t, p, v)
	}
}

// Rule 3 does not touch the pages: they stay HTML, without the API header.
func TestAPIRulesLeavePagesAlone(t *testing.T) {
	h, session, _ := apiHarness(t)
	w := apiCall(h, "GET", "/", "", session, "")
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("API-Version") != "" {
		t.Errorf("page: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Header().Get("API-Version"))
	}
	w = apiCall(h, "GET", "/nope", "", session, "")
	if w.Code != 404 || w.Header().Get("API-Version") != "" {
		t.Errorf("non-API 404 changed: %d %v", w.Code, w.Header())
	}
}

// Writes refuse fields they do not know, naming the bad one and the good ones.
func TestAPIRefusesUnknownFields(t *testing.T) {
	h, session, _ := apiHarness(t)
	w := apiCall(h, "POST", "/api/targets", `{"type":"tcp","host":"127.0.0.1","port":1,"interval":30}`, session, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `unknown field \"interval\"`) || !strings.Contains(w.Body.String(), "interval_sec") {
		t.Fatalf("unknown field: %d %s", w.Code, w.Body)
	}
	checkAPIResponse(t, "unknown field", w)
	if w := apiCall(h, "POST", "/api/targets", `{"type":"tcp","host":"127.0.0.1","port":1,"interval_sec":30}`, session, ""); w.Code != 200 {
		t.Fatalf("known fields refused: %d %s", w.Code, w.Body)
	}
}
