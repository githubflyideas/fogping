package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func httpTarget(t *testing.T, srv *httptest.Server, opts map[string]any) TargetCfg {
	t.Helper()
	host, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	tg := TargetCfg{Type: typeHTTP, Host: host, Port: port, Options: opts}
	if err := normalizeTarget(&tg); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return tg
}

func httpRoundOf(t *testing.T, tg TargetCfg) Round {
	t.Helper()
	r, err := httpRound(context.Background(), tg, 3, time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHTTPMeasuresAndCountsLoss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.WriteHeader(500)
		case "/moved":
			http.Redirect(w, r, "/ok", http.StatusMovedPermanently)
		default:
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()

	if r := httpRoundOf(t, httpTarget(t, srv, map[string]any{"scheme": "http", "path": "/ok"})); r.R != 3 || len(r.MS) != 3 {
		t.Fatalf("healthy: %+v", r)
	}
	// A 500 is "the server answered but is broken": every probe is lost.
	if r := httpRoundOf(t, httpTarget(t, srv, map[string]any{"scheme": "http", "path": "/bad"})); r.R != 0 || r.S != 3 || r.E != "" {
		t.Fatalf("500 must be loss, not an error: %+v", r)
	}
	if r := httpRoundOf(t, httpTarget(t, srv, map[string]any{"scheme": "http", "path": "/bad", "expect_status": 500})); r.R != 3 {
		t.Fatalf("expect_status 500: %+v", r)
	}
	// Redirects: the 301 itself is the answer unless asked to follow.
	if r := httpRoundOf(t, httpTarget(t, srv, map[string]any{"scheme": "http", "path": "/moved", "expect_status": 301})); r.R != 3 {
		t.Fatalf("301 as the answer: %+v", r)
	}
	if r := httpRoundOf(t, httpTarget(t, srv, map[string]any{"scheme": "http", "path": "/moved", "follow_redirects": true})); r.R != 3 {
		t.Fatalf("followed: %+v", r)
	}
}

func TestHTTPCertificatesAreVerified(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// The test server's certificate is not trusted: accepting it would report a
	// site healthy that every browser refuses.
	if r := httpRoundOf(t, httpTarget(t, srv, nil)); r.R != 0 {
		t.Fatalf("an untrusted certificate was accepted: %+v", r)
	}
}

func TestHTTPInputIsJudgedStrictly(t *testing.T) {
	for name, tg := range map[string]TargetCfg{
		"hostile host":    {Type: typeHTTP, Host: "good.example/@evil.example", Port: 443},
		"host with ?":     {Type: typeHTTP, Host: "a?b", Port: 443},
		"no port":         {Type: typeHTTP, Host: "example.org"},
		"path no slash":   {Type: typeHTTP, Host: "example.org", Port: 443, Options: map[string]any{"path": "nope"}},
		"bad scheme":      {Type: typeHTTP, Host: "example.org", Port: 443, Options: map[string]any{"scheme": "ftp"}},
		"bad host_header": {Type: typeHTTP, Host: "example.org", Port: 443, Options: map[string]any{"host_header": "a b"}},
		"path with space": {Type: typeHTTP, Host: "example.org", Port: 443, Options: map[string]any{"path": "/a b"}},
		"path with #":     {Type: typeHTTP, Host: "example.org", Port: 443, Options: map[string]any{"path": "/a#b"}},
		"unknown option":  {Type: typeHTTP, Host: "example.org", Port: 443, Options: map[string]any{"verify": false}},
	} {
		tg := tg
		if err := normalizeTarget(&tg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHTTPURLIsBuiltSafely(t *testing.T) {
	tg := TargetCfg{Type: typeHTTP, Host: "example.org", Port: 8443, Options: map[string]any{"path": "/p?a=b"}}
	if err := normalizeTarget(&tg); err != nil {
		t.Fatal(err)
	}
	if got := httpURL(tg); got != "https://example.org:8443/p?a=b" {
		t.Fatalf("url = %q", got)
	}
	tg = TargetCfg{Type: typeHTTP, Host: "2001:db8::1", Port: 80, Options: map[string]any{"scheme": "http"}}
	normalizeTarget(&tg)
	if got := httpURL(tg); got != "http://[2001:db8::1]:80/" {
		t.Fatalf("ipv6 url = %q", got)
	}
}

func TestHTTPStopsWithTheTarget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	tg := httpTarget(t, slow, map[string]any{"scheme": "http"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	t0 := time.Now()
	if _, err := httpRound(ctx, tg, 3, time.Millisecond, time.Minute); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("not stopped: %v after %v", err, time.Since(t0))
	}
}

// A name that does not resolve is a round that could not be measured, not loss.
func TestHTTPUnresolvableNameIsAProbeErrorNotLoss(t *testing.T) {
	tg := TargetCfg{Type: typeHTTP, Host: "no-such-host.invalid", Port: 80, Options: map[string]any{"scheme": "http"}}
	if err := normalizeTarget(&tg); err != nil {
		t.Fatal(err)
	}
	if _, err := httpRound(context.Background(), tg, 2, time.Millisecond, time.Second); err == nil {
		t.Fatal("an unresolvable name was reported as loss")
	}
}
