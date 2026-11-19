package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Findings from an outside review: the login endpoint did not enforce the same
// content-type rule as every other write, an oversize body was reported as 400
// instead of 413, and a mistyped address such as 999.999.999.999 was stored as a
// host name and showed up as 100% loss.

func TestLoginRequiresJSON(t *testing.T) {
	h, _, _ := apiHarness(t)
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		req := strings.NewReader(`{"user":"admin","pass":"pw"}`)
		w := rawCall(h, "POST", "/api/login", req, ct)
		if w.Code != 415 || !strings.Contains(w.Body.String(), `"unsupported_media_type"`) {
			t.Errorf("content type %q: %d %s", ct, w.Code, w.Body)
		}
	}
	if w := apiCall(h, "POST", "/api/login", `{"user":"admin","pass":"pw"}`, "", ""); w.Code != 200 {
		t.Fatalf("JSON login: %d %s", w.Code, w.Body)
	}
}

func TestOversizeBodyIs413(t *testing.T) {
	h, session, _ := apiHarness(t)
	big := `{"type":"icmp","host":"` + strings.Repeat("a", 20<<10) + `"}`
	for _, path := range []string{"/api/targets", "/api/login"} {
		s := session
		if path == "/api/login" {
			s = ""
		}
		w := apiCall(h, "POST", path, big, s, "")
		if w.Code != 413 || !strings.Contains(w.Body.String(), `"too_large"`) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
		checkAPIResponse(t, path, w)
	}
}

func TestNumericButNotIPHostsAreRefused(t *testing.T) {
	for _, bad := range []string{"999.999.999.999", "1.2.3", "300.1.1.1", "123", "1.2.3.4.5"} {
		tg := TargetCfg{Type: typeICMP, Host: bad}
		if err := normalizeTarget(&tg); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"10.0.0.1", "example.org", "1e100.net", "::1", "2001:db8::1", "a1.example"} {
		tg := TargetCfg{Type: typeICMP, Host: ok}
		if err := normalizeTarget(&tg); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

// rawCall posts a body with an explicit Content-Type (empty means none at all).
func rawCall(h http.Handler, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://fp.local"+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
