// A fogping prober that times an HTTP(S) request. Written in Go.
//
// # WHAT FOGPING EXPECTS FROM ANY PROBER, IN ANY LANGUAGE
//
// fogping runs this program once per round. The whole conversation is:
//
//  1. fogping writes ONE JSON object to our standard input (the request:
//     which host, how many packets, how long to wait, the user's options).
//  2. We measure.
//  3. We write ONE JSON object to standard output (the answer: how many probes
//     we sent, how many came back, and the round-trip time of each).
//  4. We exit.
//
// Nothing arrives on the command line and the environment is nearly empty, on
// purpose: whatever a user typed into the editor can never become part of a
// shell command line. The full contract is docs/PROBE-PROTOCOL.md.
//
// # WHAT THIS PROBER MEASURES
//
// For each of the `packets` probes: open a NEW connection, send a request, and
// stop the clock when the response HEADERS arrive. That is "time to first byte":
// DNS + TCP connect + TLS handshake + server think time. The body is never read.
// A probe counts as received when the status is acceptable (see expect_status).
//
// Try it by hand, no fogping needed:
//
//	make
//	echo '{"protocol":"fogping-probe/1","type":"http","host":"example.com","port":443,
//	       "packets":3,"gap_ms":200,"timeout_ms":3000,"options":{"path":"/"}}' | ./probe
package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// protocol is the version string both sides must agree on.
const protocol = "fogping-probe/1"

// request is what fogping sends us. Field names are the protocol's, in snake_case.
//
// Options arrive as map[string]any because the protocol lets every prober define
// its own. fogping has already checked them against OUR manifest.json (types,
// ranges, choices, defaults filled in), so reading them is simple. We still use
// small helper functions that fall back safely: a prober must never crash because
// an option was missing.
type request struct {
	Protocol  string         `json:"protocol"`
	Type      string         `json:"type"`
	Host      string         `json:"host"`
	Port      int            `json:"port"`
	Packets   int            `json:"packets"`
	GapMs     int            `json:"gap_ms"`
	TimeoutMs int            `json:"timeout_ms"`
	Options   map[string]any `json:"options"`
}

// There are exactly two valid answers, so there are two types. fogping rejects an
// answer that mixes them (code "ambiguous"), and one type per shape makes that
// mistake impossible to write.

// measurement: we probed. `ms` must ALWAYS be present, as [] when everything was
// lost (a nil slice would encode as null and be rejected). Lost probes are not
// listed: len(ms) must equal `received`.
type measurement struct {
	Protocol string    `json:"protocol"`
	Sent     int       `json:"sent"`
	Received int       `json:"received"`
	MS       []float64 `json:"ms"`
}

// failure: we could not measure at all.
type failure struct {
	Protocol string `json:"protocol"`
	Error    string `json:"error"`
}

func main() {
	// os.Exit would skip deferred calls, so main only wires things together.
	os.Exit(run(os.Stdin, os.Stdout))
}

func run(in io.Reader, out io.Writer) int {
	// Never trust the input, even though it comes from fogping: read a bounded
	// amount, so a bug on the other side cannot make us allocate without limit.
	var req request
	if err := json.NewDecoder(io.LimitReader(in, 64<<10)).Decode(&req); err != nil {
		return fail(out, "bad request: "+err.Error())
	}
	if req.Protocol != protocol {
		return fail(out, "unsupported protocol "+req.Protocol)
	}
	if req.Packets < 1 || req.Packets > 1000 {
		return fail(out, "packets out of range")
	}

	cfg, err := parseOptions(req)
	if err != nil {
		// A prober's own failure is a valid answer. fogping records it as "this
		// round could not be measured": it is NOT counted as packet loss.
		return fail(out, err.Error())
	}

	rtts := probe(req, cfg)

	return reply(out, measurement{Protocol: protocol, Sent: req.Packets, Received: len(rtts), MS: rtts})
}

// config is the request after validation: everything the probing code needs, in
// the types it needs them in.
type config struct {
	url            string // the full URL to fetch
	method         string
	hostHeader     string
	expectStatus   int // 0 = any status below 400
	followRedirect bool
	gap, timeout   time.Duration
	packets        int
}

// parseOptions turns the raw request into a config.
//
// Why check things fogping already checked? fogping validates the TYPES and
// RANGES declared in manifest.json. It cannot know that `path` must start with
// a slash. Rules only this prober understands belong in this prober.
func parseOptions(req request) (config, error) {
	c := config{
		method:         optString(req.Options, "method", "GET"),
		hostHeader:     optString(req.Options, "host_header", ""),
		expectStatus:   optInt(req.Options, "expect_status", 0),
		followRedirect: optBool(req.Options, "follow_redirects"),
		gap:            time.Duration(req.GapMs) * time.Millisecond,
		timeout:        time.Duration(req.TimeoutMs) * time.Millisecond,
		packets:        req.Packets,
	}
	path := optString(req.Options, "path", "/")
	if !strings.HasPrefix(path, "/") {
		return c, errors.New("path must start with /")
	}
	if req.Port < 1 || req.Port > 65535 {
		return c, errors.New("port must be 1-65535")
	}

	// The host comes from a user, and fogping only checks it has no spaces. A host
	// such as "good.example/@evil.example" would turn into a URL that talks to a
	// DIFFERENT server. Whenever text becomes part of a structured string (URL,
	// SQL, shell command), check it against what it is allowed to be first:
	// here, a hostname or an IP address and nothing else.
	if !validHost(req.Host) {
		return c, errors.New("host must be a hostname or an IP address")
	}

	// Build the URL with net/url rather than concatenating strings; that handles
	// escaping of the path. net.JoinHostPort adds the brackets IPv6 needs.
	u := url.URL{
		Scheme: optString(req.Options, "scheme", "https"),
		Host:   net.JoinHostPort(req.Host, fmt.Sprint(req.Port)),
	}
	// path may carry a query string ("/ping?src=fogping"): split it ourselves
	// instead of letting url.URL escape the "?".
	if p, q, found := strings.Cut(path, "?"); found {
		u.Path, u.RawQuery = p, q
	} else {
		u.Path = p
	}
	c.url = u.String()
	return c, nil
}

// validHost allows letters, digits, dot, hyphen, underscore and colon (IPv6).
// It is a whitelist: anything not named is refused, which is safer than trying
// to list every dangerous character.
func validHost(h string) bool {
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

// probe sends cfg.packets requests and returns the round-trip time, in
// milliseconds, of each one that succeeded. A request that fails, times out or
// returns an unacceptable status simply has no entry: that is what packet loss
// IS to fogping. The caller counts `packets - len(result)` as lost.
func probe(req request, cfg config) []float64 {
	client := &http.Client{
		Timeout: cfg.timeout,
		// DisableKeepAlives makes every probe open a fresh connection. With
		// connection reuse, probes 2..N would skip DNS, TCP and TLS and measure
		// only the server: a different, much smaller number than probe 1.
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12}, // certificates ARE verified
			Proxy:             nil,                                       // never go through a proxy from the environment
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if !cfg.followRedirect {
				return http.ErrUseLastResponse // the redirect itself is the answer
			}
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return nil
		},
	}

	rtts := []float64{} // not nil: see the note on measurement.MS
	for i := 0; i < cfg.packets; i++ {
		if i > 0 {
			time.Sleep(cfg.gap)
		}
		if ms, ok := once(client, cfg); ok {
			rtts = append(rtts, ms)
		}
	}
	return rtts
}

// once performs one request and reports its time to first byte.
func once(client *http.Client, cfg config) (ms float64, ok bool) {
	r, err := http.NewRequest(cfg.method, cfg.url, nil)
	if err != nil {
		return 0, false
	}
	if cfg.hostHeader != "" {
		r.Host = cfg.hostHeader // the Host header is a field of the request, not a header map entry
	}
	r.Header.Set("User-Agent", "fogping-probe-http/1")

	start := time.Now()
	resp, err := client.Do(r) // returns as soon as the headers are in
	elapsed := time.Since(start)
	if err != nil {
		return 0, false
	}
	// We never read the body, so close it without draining: draining a large
	// download would cost time and bandwidth for nothing.
	resp.Body.Close()

	if !statusOK(resp.StatusCode, cfg.expectStatus) {
		return 0, false
	}
	return float64(elapsed.Microseconds()) / 1000.0, true
}

// statusOK decides whether a status code means "the service answered properly".
func statusOK(got, expect int) bool {
	if expect == 0 {
		return got < 400
	}
	return got == expect
}

// ---- small helpers: read options without ever panicking ----

func optString(o map[string]any, key, def string) string {
	if s, ok := o[key].(string); ok && s != "" {
		return s
	}
	return def
}

// optInt: JSON numbers decode into float64 in Go.
func optInt(o map[string]any, key string, def int) int {
	if f, ok := o[key].(float64); ok {
		return int(f)
	}
	return def
}

func optBool(o map[string]any, key string) bool {
	b, _ := o[key].(bool)
	return b
}

// ---- output ----

func reply(out io.Writer, answer any) int {
	if err := json.NewEncoder(out).Encode(answer); err != nil {
		return 1
	}
	return 0
}

// fail reports a problem that stopped us measuring. Exit status 0 on purpose:
// we DID produce a valid answer, and fogping judges the answer, not the exit
// code. Keep the text short and free of secrets; fogping cuts it at 200 chars.
func fail(out io.Writer, msg string) int {
	return reply(out, failure{Protocol: protocol, Error: msg})
}
