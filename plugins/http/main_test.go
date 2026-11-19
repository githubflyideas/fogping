package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// These tests show how to test a prober: feed run() a request, read the answer.
// No fogping is needed, and no network beyond a local test server.

// result is a loose view of an answer, so a test can tell "field absent" (nil)
// from "field is zero".
type result struct {
	Sent     *int            `json:"sent"`
	Received *int            `json:"received"`
	MS       json.RawMessage `json:"ms"`
	Error    string          `json:"error"`
}

func runReq(t *testing.T, body string) result {
	t.Helper()
	var out bytes.Buffer
	run(strings.NewReader(body), &out)
	var a result
	if err := json.Unmarshal(out.Bytes(), &a); err != nil {
		t.Fatalf("answer is not JSON: %v: %s", err, out.String())
	}
	return a
}

func request1(host string, port int, opts string) string {
	return `{"protocol":"fogping-probe/1","type":"http","host":"` + host + `","port":` + strconv.Itoa(port) +
		`,"packets":3,"gap_ms":1,"timeout_ms":2000,"options":` + opts + `}`
}

func TestMeasuresAndCountsLoss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	a := runReq(t, request1(host, port, `{"scheme":"http","path":"/ok"}`))
	if a.Error != "" || *a.Sent != 3 || *a.Received != 3 {
		t.Fatalf("healthy server: %+v", a)
	}

	// A 500 is "the server answered but is broken": every probe counts as lost.
	// The answer must still carry "ms": [] (present and empty), or fogping
	// rejects it as malformed.
	a = runReq(t, request1(host, port, `{"scheme":"http","path":"/bad"}`))
	if a.Error != "" || *a.Received != 0 || string(a.MS) != "[]" {
		t.Fatalf("500 must count as loss with ms=[]: %+v ms=%s", a, a.MS)
	}

	// expect_status makes one specific code the success condition.
	a = runReq(t, request1(host, port, `{"scheme":"http","path":"/bad","expect_status":500}`))
	if *a.Received != 3 {
		t.Fatalf("expect_status 500: %+v", a)
	}
}

func TestOwnFailuresAreErrorsNotLoss(t *testing.T) {
	for name, body := range map[string]string{
		"bad option":     request1("127.0.0.1", 80, `{"path":"nope"}`),
		"wrong protocol": `{"protocol":"fogping-probe/9"}`,
		"garbage":        `not json`,
		"hostile host":   request1("good.example/@evil.example", 443, `{}`),
		"bad port":       request1("127.0.0.1", 0, `{}`),
	} {
		a := runReq(t, body)
		// An error answer has an error and NO measurement fields at all.
		if a.Error == "" || a.Sent != nil || a.Received != nil || a.MS != nil {
			t.Errorf("%s: want an error-only answer, got %+v", name, a)
		}
	}
}

func TestURLIsBuiltSafely(t *testing.T) {
	c, err := parseOptions(request{Host: "example.org", Port: 8443, Packets: 1,
		Options: map[string]any{"path": "/p?a=b"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.url != "https://example.org:8443/p?a=b" {
		t.Fatalf("url = %q", c.url)
	}
	// IPv6 literals get their brackets from net.JoinHostPort.
	c, _ = parseOptions(request{Host: "2001:db8::1", Port: 80, Packets: 1, Options: map[string]any{"scheme": "http"}})
	if c.url != "http://[2001:db8::1]:80/" {
		t.Fatalf("ipv6 url = %q", c.url)
	}
}

func TestValidHost(t *testing.T) {
	for _, ok := range []string{"example.org", "10.0.0.1", "2001:db8::1", "a_b-c.example"} {
		if !validHost(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a/b", "a@b", "a?b", "a#b", "a\\b", "[::1]", "a%b", strings.Repeat("a", 254)} {
		if validHost(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
