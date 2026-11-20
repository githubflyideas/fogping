// A fogping prober that times an LDAP directory server. Written in Go.
//
// # WHAT FOGPING EXPECTS FROM ANY PROBER, IN ANY LANGUAGE
//
// fogping runs this program once per round. The whole conversation is:
//
//  1. fogping writes ONE JSON object to our standard input (the request: which
//     host, how many probes, how long to wait, the user's options).
//  2. We measure.
//  3. We write ONE JSON object to standard output (the answer: how many probes
//     we sent, how many were answered, and the round-trip time of each).
//  4. We exit.
//
// Nothing arrives on the command line and the environment is nearly empty, on
// purpose: whatever a user typed into the editor can never become part of a shell
// command line. The full contract is docs/PROBE-PROTOCOL.md.
//
// # WHAT THIS PROBER MEASURES
//
// An LDAP directory (OpenLDAP, Active Directory, 389) is usually the thing every
// login depends on, so "is it answering, and how fast?" is worth a chart. Each
// probe opens a new connection and goes as far as the option `stage` says:
//
//	connect  TCP, plus the TLS handshake for ldaps or starttls
//	bind     ... and an ANONYMOUS bind
//	rootdse  ... and reading the server's "root DSE" (the default)
//
// The root DSE is a small public entry every LDAP server publishes at the empty
// name "": it lists the naming contexts and supported versions. Reading it proves
// the directory is accepting and answering queries, not just listening.
//
// WHY NO PASSWORD. Options are stored as plain text, so this prober never asks for
// credentials, and everything it does works without logging in. Some servers
// refuse an anonymous root DSE read; for those the probe counts as lost, and you
// should measure `bind` or `connect` instead.
//
// Try it by hand:
//
//	make
//	echo '{"protocol":"fogping-probe/1","type":"ldap","host":"ldap.example.com","port":389,
//	       "packets":3,"gap_ms":200,"timeout_ms":3000,"options":{"stage":"rootdse"}}' | ./probe
package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// protocol is the version string both sides must agree on.
const protocol = "fogping-probe/1"

// request is what fogping sends us, with the protocol's snake_case names.
//
// Options arrive as map[string]any because every prober defines its own. fogping
// has already checked them against OUR manifest.json (types, ranges, choices,
// defaults filled in), so reading them is simple. We still read them through small
// helpers that fall back safely: a prober must never crash because an option was
// missing.
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

// failure: we could not measure at all. fogping records this as a failed round,
// which is NOT packet loss.
type failure struct {
	Protocol string `json:"protocol"`
	Error    string `json:"error"`
}

func main() {
	// os.Exit would skip deferred calls, so main only wires things together.
	os.Exit(run(os.Stdin, os.Stdout))
}

func run(in io.Reader, out io.Writer) int {
	// Never trust the input, even from fogping: read a bounded amount, so a bug on
	// the other side cannot make us allocate without limit.
	var req request
	if err := json.NewDecoder(io.LimitReader(in, 64<<10)).Decode(&req); err != nil {
		return reply(out, fail("bad request: "+err.Error()))
	}
	cfg, err := parse(req)
	if err != nil {
		return reply(out, fail(err.Error()))
	}
	rtts := measure(cfg)
	return reply(out, measurement{Protocol: protocol, Sent: cfg.packets, Received: len(rtts), MS: rtts})
}

// config is the request after validation, in the types the probing code needs.
type config struct {
	host         string
	port         int
	security     string // none | starttls | ldaps
	stage        string // connect | bind | rootdse
	packets      int
	gap, timeout time.Duration
}

// parse validates the request. Rules only this prober knows live here: fogping
// checks what the manifest can express, nothing more.
func parse(req request) (config, error) {
	if req.Protocol != protocol {
		return config{}, errors.New("unsupported protocol")
	}
	if req.Packets < 1 || req.Packets > 1000 {
		return config{}, errors.New("packets out of range")
	}
	if req.TimeoutMs < 1 || req.TimeoutMs > 600_000 || req.GapMs < 0 || req.GapMs > 600_000 {
		return config{}, errors.New("gap_ms or timeout_ms out of range")
	}
	if req.Port < 1 || req.Port > 65535 {
		return config{}, errors.New("port must be 1-65535")
	}
	// The host comes from a user and fogping only checks it has no spaces. We
	// build network addresses from it, so check it against what a host may be.
	// A whitelist is safer than trying to list every dangerous character.
	if !validHost(req.Host) {
		return config{}, errors.New("host must be a hostname or an IP address")
	}
	c := config{
		host:     req.Host,
		port:     req.Port,
		security: optString(req.Options, "security", "none"),
		stage:    optString(req.Options, "stage", "rootdse"),
		packets:  req.Packets,
		gap:      time.Duration(req.GapMs) * time.Millisecond,
		timeout:  time.Duration(req.TimeoutMs) * time.Millisecond,
	}
	// fogping checked the choices already; checking again costs one line and means
	// this prober is safe even if it is run by hand with a careless request.
	if c.security != "none" && c.security != "starttls" && c.security != "ldaps" {
		return config{}, errors.New("security must be none, starttls or ldaps")
	}
	if c.stage != "connect" && c.stage != "bind" && c.stage != "rootdse" {
		return config{}, errors.New("stage must be connect, bind or rootdse")
	}
	return c, nil
}

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

// measure runs all the probes of one round. A probe that fails for any reason the
// server or the network is responsible for has no entry: that is packet loss.
func measure(c config) []float64 {
	rtts := []float64{} // not nil: see the note on measurement.MS
	for i := 0; i < c.packets; i++ {
		if i > 0 {
			time.Sleep(c.gap)
		}
		if ms, err := probeOnce(c); err == nil {
			rtts = append(rtts, ms)
		}
	}
	return rtts
}

// probeOnce opens a connection, goes as far as c.stage, and returns the elapsed
// milliseconds. Any error means this probe is lost.
func probeOnce(c config) (float64, error) {
	// One deadline for the whole probe. Each step below gets whatever time is LEFT,
	// so a slow connect cannot be followed by a full timeout for every later step.
	deadline := time.Now().Add(c.timeout)
	start := time.Now()

	// The certificate is always verified. For a host name, ServerName makes Go check
	// the certificate is for that name; for an IP address it is matched against the
	// certificate's IP entries. There is deliberately no "skip verification" option:
	// a monitor that says "up" for a server your clients refuse is lying.
	tlsCfg := &tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12}
	addr := net.JoinHostPort(c.host, fmt.Sprint(c.port))
	dialer := &net.Dialer{Timeout: c.timeout}

	var conn *ldap.Conn
	var err error
	if c.security == "ldaps" {
		conn, err = ldap.DialURL("ldaps://"+addr, ldap.DialWithDialer(dialer), ldap.DialWithTLSConfig(tlsCfg))
	} else {
		conn, err = ldap.DialURL("ldap://"+addr, ldap.DialWithDialer(dialer))
	}
	if err != nil {
		return 0, err
	}
	// Closing sends an "unbind" and hangs up politely. It is not part of the
	// measurement: it happens after we read the clock.
	defer conn.Close()

	// remaining sets the time each later operation may take.
	remaining := func() time.Duration {
		d := time.Until(deadline)
		if d < time.Millisecond {
			d = time.Millisecond
		}
		return d
	}

	if c.security == "starttls" {
		conn.SetTimeout(remaining())
		if err := conn.StartTLS(tlsCfg); err != nil {
			return 0, err
		}
	}
	if c.stage == "connect" {
		return since(start), nil
	}

	// Anonymous bind: an empty name and no password. UnauthenticatedBind is the
	// call for this; Bind("", "") would refuse an empty password locally.
	conn.SetTimeout(remaining())
	if err := conn.UnauthenticatedBind(""); err != nil {
		return 0, err
	}
	if c.stage == "bind" {
		return since(start), nil
	}

	// The root DSE: a base-scope search of the empty name, asking for two small
	// attributes. SizeLimit 1 because there is exactly one entry.
	conn.SetTimeout(remaining())
	res, err := conn.Search(ldap.NewSearchRequest(
		"", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false,
		"(objectClass=*)", []string{"namingContexts", "supportedLDAPVersion"}, nil))
	if err != nil {
		return 0, err
	}
	if len(res.Entries) != 1 {
		// A server that answers "success" with nothing is not serving the
		// directory the way a healthy one does: lost.
		return 0, errors.New("root DSE not returned")
	}
	return since(start), nil
}

func since(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000.0 }

// ---- small helper: read an option without ever panicking ----

func optString(o map[string]any, key, def string) string {
	if s, ok := o[key].(string); ok && s != "" {
		return s
	}
	return def
}

// ---- output ----

func reply(out io.Writer, answer any) int {
	if err := json.NewEncoder(out).Encode(answer); err != nil {
		return 1
	}
	return 0
}

// fail reports a problem that stopped us measuring. Exit status 0 on purpose: we
// DID produce a valid answer, and fogping judges the answer, not the exit code.
// Keep the text short and free of secrets; fogping cuts it at 200 characters.
func fail(msg string) failure { return failure{Protocol: protocol, Error: msg} }
