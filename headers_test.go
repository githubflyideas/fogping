package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func checkSecurityHeaders(t *testing.T, label string, w *httptest.ResponseRecorder) {
	t.Helper()
	h := w.Header()
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("%s: X-Frame-Options = %q, want DENY", label, got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", label, got)
	}
	if got := h.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("%s: Referrer-Policy = %q, want same-origin", label, got)
	}
	if got := h.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("%s: CSP %q lacks frame-ancestors 'none'", label, got)
	}
}

func checkPageCSP(t *testing.T, label string, w *httptest.ResponseRecorder, page string) {
	t.Helper()
	csp := w.Header().Get("Content-Security-Policy")
	b, _ := staticFS.ReadFile("static/" + page)
	hashes := inlineScriptHashes(b)
	if len(hashes) == 0 {
		t.Fatalf("%s: no inline scripts found in %s — the matcher has stopped working", label, page)
	}
	for _, hs := range hashes {
		if !strings.Contains(csp, hs) {
			t.Errorf("%s: CSP lacks hash %s", label, hs)
		}
	}
	for _, bad := range []string{"'unsafe-eval'", "script-src 'self' 'unsafe-inline'", "*"} {
		if strings.Contains(csp, bad) {
			t.Errorf("%s: CSP contains %s: %q", label, bad, csp)
		}
	}
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "object-src 'none'") {
		t.Errorf("%s: page CSP too weak: %q", label, csp)
	}
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	run := NewRunner(ProbeCfg{Packets: 1, TimeoutMs: 50}, s, NewDetector(s))
	do := func(h http.Handler, method, path, body, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://fp.local"+path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://fp.local")
		}
		if cookie != "" {
			req.Header.Set("Cookie", "fogping_session="+cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// Open, read-only UI (no users): the shareable page itself.
	open := newMux(&Config{}, s, run, nil)
	w := do(open, "GET", "/", "", "")
	if w.Code != 200 {
		t.Fatalf("open GET /: %d", w.Code)
	}
	checkSecurityHeaders(t, "open /", w)
	checkPageCSP(t, "open /", w, "index.html")

	// With a login.
	auth := newMux(&Config{Editable: true}, s, run, map[string]string{"admin": "pw"})
	w = do(auth, "GET", "/login", "", "")
	checkSecurityHeaders(t, "/login", w)
	checkPageCSP(t, "/login", w, "login.html")

	w = do(auth, "GET", "/", "", "")
	if w.Code != http.StatusFound {
		t.Errorf("unauthenticated GET / = %d, want 302", w.Code)
	}
	checkSecurityHeaders(t, "redirect /", w)

	w = do(auth, "POST", "/api/login", `{"User":"admin","Pass":"pw"}`, "")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	checkSecurityHeaders(t, "/api/login", w)
	var tok string
	for _, c := range w.Result().Cookies() {
		if c.Name == "fogping_session" {
			tok = c.Value
		}
	}

	w = do(auth, "GET", "/", "", tok)
	if w.Code != 200 {
		t.Fatalf("authed GET /: %d", w.Code)
	}
	checkSecurityHeaders(t, "authed /", w)
	checkPageCSP(t, "authed /", w, "index.html")

	cases := []struct {
		method, path, body, cookie string
		code                       int
	}{
		{"GET", "/api/targets", "", "", http.StatusUnauthorized},
		{"GET", "/api/targets", "", tok, 200},
		{"GET", "/api/version", "", "", 200},
		{"GET", "/api/targets/export", "", tok, 200},
		{"POST", "/api/targets", `{"name":"t1","type":"tcp","host":"127.0.0.1","port":1,"interval_sec":3600}`, tok, 200},
		{"GET", "/static/echarts.min.js", "", "", 200},
		{"GET", "/static/nope.js", "", "", 404},
		{"GET", "/nope", "", "", 404},
	}
	for _, c := range cases {
		w := do(auth, c.method, c.path, c.body, c.cookie)
		label := c.method + " " + c.path
		if c.code != 0 && w.Code != c.code {
			t.Errorf("%s: status %d, want %d", label, w.Code, c.code)
		}
		checkSecurityHeaders(t, label, w)
	}
}

// Every <script> in a page is either external (same origin) or an attribute-less
// inline block the hash matcher sees, and no markup uses inline event handlers
// or javascript: URLs, which a hash cannot allow.
func TestPageScriptsAllCovered(t *testing.T) {
	anyScript := regexp.MustCompile(`<script\b[^>]*>`)
	external := regexp.MustCompile(`^<script src="/static/[^"]+"></script>`)
	handler := regexp.MustCompile(`(?i)<[a-z][^>]*\son[a-z]+\s*=`)
	for _, page := range []string{"index.html", "login.html"} {
		b, err := staticFS.ReadFile("static/" + page)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		inline := len(inlineScriptRe.FindAllString(src, -1))
		ext := 0
		for _, loc := range anyScript.FindAllStringIndex(src, -1) {
			if external.MatchString(src[loc[0]:]) {
				ext++
			}
		}
		if total := len(anyScript.FindAllString(src, -1)); total != inline+ext {
			t.Errorf("%s: %d <script> tags but only %d inline + %d external are covered", page, total, inline, ext)
		}
		if m := handler.FindString(src); m != "" {
			t.Errorf("%s: inline event handler %q is blocked by the CSP", page, m)
		}
		if strings.Contains(src, "javascript:") {
			t.Errorf("%s: javascript: URL is blocked by the CSP", page)
		}
	}
}
