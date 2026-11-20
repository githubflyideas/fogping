package main

// The built-in HTTP(S) prober: time to first byte of a request.
//
// For each of `packets` probes it opens a NEW connection, sends one request and
// stops the clock when the response HEADERS arrive. That is DNS + TCP connect + TLS
// handshake + the server's own processing: the number a visitor feels. The body is
// never read. A probe counts as received when the status is acceptable.
//
// This file is also the reference for writing a prober in Go: it registers itself
// with the registry like any other and declares its options, so the editor form,
// validation, storage and the targets file all follow with no other change. The
// same code is built into pingping and vrf-ping, so one set of tests describes all
// three.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const typeHTTP = "http"

// registerHTTP is called from prober.go's init, after icmp and tcp: the order of
// registration is the order the UI lists the types in, and Go would otherwise run
// init functions in file-name order.
func registerHTTP() {
	registerBuiltin(&prober{
		name:     typeHTTP,
		label:    "HTTP",
		usesPort: true,
		options: []optionSpec{
			{Key: "scheme", Label: "Scheme", Kind: optChoice, Choices: []string{"https", "http"}, Default: "https",
				Help: "https verifies the certificate; there is deliberately no option to switch that off"},
			{Key: "method", Label: "Method", Kind: optChoice, Choices: []string{"GET", "HEAD"}, Default: "GET"},
			{Key: "path", Label: "Path", Kind: optString, Default: "/", MaxLen: 200,
				Help: "Starts with /, for example /health. A query string is allowed: /ping?src=fogping"},
			{Key: "host_header", Label: "Host header", Kind: optString, MaxLen: 253,
				Help: "Ask for a different site on the same address (virtual hosting). Empty = the target's host."},
			{Key: "expect_status", Label: "Expected status", Kind: optInt, Min: 0, Max: 599, Default: 0,
				Help: "0 = any status below 400 counts as up. Otherwise only exactly this status does."},
			{Key: "follow_redirects", Label: "Follow redirects", Kind: optBool,
				Help: "Off: a 301 is the answer. On: follow up to 5 redirects and time the whole chain."},
		},
		validate: validateHTTP,
		round:    httpRound,
	})
}

// validateHTTP holds the rules only this prober knows. fogping checks the types
// and ranges the option specs declare; it cannot know that a path starts with "/".
func validateHTTP(t *TargetCfg) error {
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("http target needs a port 1-65535")
	}
	if !validURLHost(t.Host) {
		return fmt.Errorf("http host must be a hostname or an IP address")
	}
	if p := t.OptString("path"); !validURLPath(p) {
		return fmt.Errorf("option \"path\" must start with / and hold no spaces, # or control characters")
	}
	if h := t.OptString("host_header"); h != "" && !validURLHost(h) {
		return fmt.Errorf("option \"host_header\" must be a hostname or an IP address")
	}
	return nil
}

// validURLPath: starts with "/", and every character is printable and not a space
// or "#". The path is split at "?" and the query part is placed into the URL as
// written (url.URL does not escape it), so what may appear in it has to be checked
// here: a space would corrupt the request line, and "#" would start a fragment that
// silently drops the rest. Control characters are refused by the options layer and
// again by net/http, but this prober does not rely on either.
func validURLPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f || r == '#' {
			return false
		}
	}
	return true
}

// validURLHost is a WHITELIST: letters, digits, dot, hyphen, underscore, colon
// (IPv6). The host comes from a user, and the general host check only refuses
// spaces. A host such as "good.example/@evil.example" would, glued into a URL,
// make the probe talk to a DIFFERENT server. Whenever text becomes part of a
// structured string (URL, SQL, command line), check it against what it may be
// first; listing what is allowed is safer than listing what is dangerous.
func validURLHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == ':':
		default:
			return false
		}
	}
	return true
}

// httpURL builds the URL with net/url rather than by concatenation, so the path is
// escaped properly. net.JoinHostPort adds the brackets an IPv6 address needs.
func httpURL(t TargetCfg) string {
	u := url.URL{Scheme: t.OptString("scheme"), Host: net.JoinHostPort(t.Host, fmt.Sprint(t.Port))}
	// The path may carry a query string: split it ourselves instead of letting
	// url.URL escape the "?".
	if p, q, found := strings.Cut(t.OptString("path"), "?"); found {
		u.Path, u.RawQuery = p, q
	} else {
		u.Path = p
	}
	return u.String()
}

// httpStatusOK decides whether a status means "the service answered properly".
func httpStatusOK(got, expect int) bool {
	if expect == 0 {
		return got < 400
	}
	return got == expect
}

// httpRound sends `packets` requests, `gap` apart. A request that fails, times out
// or returns an unacceptable status has no sample: that is what packet loss IS to
// fogping. A 500 counts as loss because the service is not working, although the
// network is fine.
func httpRound(ctx context.Context, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error) {
	r := Round{T: time.Now().Unix(), S: packets}
	follow := t.OptBool("follow_redirects")
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Every probe opens a fresh connection. With reuse, probes 2..N would
			// skip DNS, TCP and TLS and measure only the server: a much smaller
			// number than probe 1.
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12}, // certificates ARE verified
			Proxy:             nil,                                       // never a proxy taken from the environment
			// A server chooses how much it sends before the body. Bound it: the default is
			// 1 MB per probe, from a machine we do not control.
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if !follow {
				return http.ErrUseLastResponse // the redirect itself is the answer
			}
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return nil
		},
	}
	target, expect := httpURL(t), t.OptInt("expect_status")
	for i := 0; i < packets; i++ {
		if i > 0 {
			select {
			case <-time.After(gap):
			case <-ctx.Done():
				return r, ctx.Err()
			}
		}
		ms, ok, err := httpOnce(ctx, client, t, target, expect)
		var dns *net.DNSError
		switch {
		case ok:
			r.R++
			r.MS = append(r.MS, ms)
		case ctx.Err() != nil:
			return r, ctx.Err() // stopped mid-round: the caller drops the round
		case errors.As(err, &dns):
			// A name that does not resolve says nothing about the service: it is a
			// round that could not be measured (like a tcp target's lookup failure),
			// not 100% loss.
			return r, fmt.Errorf("cannot resolve %s: %v", t.Host, dns.Err)
		}
	}
	return r, nil
}

// httpOnce performs one request. ok means the probe was answered acceptably; err is
// the transport error when it was not, so the caller can tell "the network did not
// answer" (loss) from "we could not even try" (a probe error).
func httpOnce(ctx context.Context, client *http.Client, t TargetCfg, target string, expect int) (ms float64, ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, t.OptString("method"), target, nil)
	if err != nil {
		return 0, false, nil
	}
	if h := t.OptString("host_header"); h != "" {
		req.Host = h // the Host header is a field of the request, not a header map entry
	}
	req.Header.Set("User-Agent", "fogping-http/1")
	start := time.Now()
	resp, err := client.Do(req) // returns as soon as the headers are in
	elapsed := time.Since(start)
	if err != nil {
		return 0, false, err
	}
	// The body is never read: draining a large download would cost time and
	// bandwidth for nothing.
	resp.Body.Close()
	if !httpStatusOK(resp.StatusCode, expect) {
		return 0, false, nil
	}
	return float64(elapsed.Microseconds()) / 1000.0, true, nil
}
