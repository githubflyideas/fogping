package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func authTestMux(t *testing.T) http.Handler {
	t.Helper()
	old := loginFailDelay
	loginFailDelay = 0
	t.Cleanup(func() { loginFailDelay = old })
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	run := NewRunner(ProbeCfg{Packets: 1, TimeoutMs: 50}, s, NewDetector(s))
	return newMux(&Config{RetentionDays: 93, HotDays: 3, Editable: true}, s, run,
		map[string]string{"admin": "right"})
}

func authDo(h http.Handler, method, path, body, cookie, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://fp.local"+path, strings.NewReader(body))
	if remote != "" {
		req.RemoteAddr = remote
	}
	if cookie != "" {
		req.Header.Set("Cookie", "fogping_session="+cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func sessionCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == "fogping_session" {
			return c.Value
		}
	}
	return ""
}

// Eight wrong passwords and then the right one: the right one must not get in
// while the address is locked, and another address must be unaffected.
func TestLoginLockout(t *testing.T) {
	h := authTestMux(t)
	const attacker, other = "198.51.100.9:4000", "198.51.100.10:4000"
	wrong := `{"User":"admin","Pass":"nope"}`
	right := `{"User":"admin","Pass":"right"}`

	for i := 1; i <= 8; i++ {
		w := authDo(h, "POST", "/api/login", wrong, "", attacker)
		if i <= loginFreeFails+1 && w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d, want 401", i, w.Code)
		}
		if i > loginFreeFails+1 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("wrong password %d while locked: %d, want 429", i, w.Code)
		}
	}
	w := authDo(h, "POST", "/api/login", right, "", attacker)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("right password while locked: %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("429 without a usable Retry-After: %q", ra)
	}
	if sessionCookie(w) != "" {
		t.Fatal("a locked address was issued a session")
	}
	checkSecurityHeaders(t, "429 /api/login", w)

	if w := authDo(h, "POST", "/api/login", right, "", other); w.Code != 200 {
		t.Fatalf("another address was locked out too: %d", w.Code)
	}
}

func TestLoginLockExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginThrottle()
	l.now = func() time.Time { return now }
	for i := 0; i <= loginFreeFails; i++ {
		l.begin("a")
		l.fail("a")
	}
	if _, ok := l.begin("a"); ok {
		t.Fatal("not locked")
	}
	now = now.Add(loginBaseLock)
	if _, ok := l.begin("a"); !ok {
		t.Fatal("still locked after the lock ran out")
	}
}

// Logout must kill the session on the server, not only ask the browser to forget
// the cookie: a copy of the cookie must stop working too.
func TestLogoutRevokesSession(t *testing.T) {
	h := authTestMux(t)
	w := authDo(h, "POST", "/api/login", `{"User":"admin","Pass":"right"}`, "", "")
	tok := sessionCookie(w)
	if w.Code != 200 || tok == "" {
		t.Fatalf("login: %d %q", w.Code, tok)
	}
	if w := authDo(h, "GET", "/api/series?target=x", "", tok, ""); w.Code != 200 {
		t.Fatalf("series before logout: %d", w.Code)
	}
	if w := authDo(h, "POST", "/api/logout", "", tok, ""); w.Code != 200 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := authDo(h, "GET", "/api/series?target=x", "", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after logout: %d, want 401", w.Code)
	}
	if w := authDo(h, "GET", "/", "", tok, ""); w.Code != http.StatusFound {
		t.Fatalf("old cookie on / after logout: %d, want 302", w.Code)
	}
}

// With a login configured, an anonymous caller learns the version and nothing
// about how the instance is set up.
func TestVersionHidesSettingsUntilLogin(t *testing.T) {
	h := authTestMux(t)
	var anon map[string]any
	w := authDo(h, "GET", "/api/version", "", "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &anon); err != nil || w.Code != 200 {
		t.Fatalf("anonymous /api/version: %d %v", w.Code, err)
	}
	if _, ok := anon["version"]; !ok || len(anon) != 1 {
		t.Fatalf("anonymous /api/version = %v, want the version only", anon)
	}

	tok := sessionCookie(authDo(h, "POST", "/api/login", `{"User":"admin","Pass":"right"}`, "", ""))
	var full map[string]any
	json.Unmarshal(authDo(h, "GET", "/api/version", "", tok, "").Body.Bytes(), &full)
	for _, k := range []string{"version", "retention_days", "hot_days", "editable"} {
		if _, ok := full[k]; !ok {
			t.Errorf("signed-in /api/version lacks %q: %v", k, full)
		}
	}
}
