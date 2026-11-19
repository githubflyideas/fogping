package main

// Start-up made easy to get right, without loosening anything.
//
// Three tools, each for a moment where an operator can go wrong:
//
//   - prepare reads every flag and credential and returns ALL the problems at once,
//     so a start that is refused names everything to fix, not the first thing.
//   - --check runs the same preparation plus the checks that need the machine (data
//     directory, database version, port, plugins) and reports, without starting:
//     run it before restarting a service, and a mistake never takes monitoring down.
//   - --gen-token writes a strong random token to a file only its owner can read,
//     so the safe way to configure Zabbix is also the easy one.
//
// And at every start a security summary says, in one block, what is exposed: where
// it listens, whether there is a login, whether editing is on, what integrations run.
//
// Every refusal stays a refusal. These tools only make the right configuration
// easier to reach (ISO/IEC 27001:2022 A.8.9 configuration management, A.8.15
// logging, A.8.24 use of cryptography; ISO/IEC 25010:2023 user error protection).

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// setup is everything the command line and the environment decided.
type setup struct {
	cfg          *Config
	users        map[string]string
	localOnly    bool
	probes       bool
	cliPasswd    bool // a password was given on the command line (visible in ps)
	tokenIgnored bool // a Zabbix token is set but --zabbix was not given
}

// startFlags are the parsed flags prepare works from.
type startFlags struct {
	days                           int
	localOnly, edit, zabbix, probe bool
}

// prepare turns flags, key=value arguments and the environment into a setup. It
// does not stop at the first problem: every problem is returned, so whoever reads
// the refusal can fix them all in one go.
func prepare(f startFlags, args []string, getenv func(string) string) (setup, []error) {
	var problems []error
	s := setup{cfg: defaultConfig(), localOnly: f.localOnly, probes: f.probe}
	days, err := retentionDays(f.days)
	if err != nil {
		problems = append(problems, err)
		days = defaultDays
	}
	s.cfg.RetentionDays = days

	token, rest, ignored, err := resolveZabbixToken(f.zabbix, args, getenv)
	if err != nil {
		problems = append(problems, err)
	}
	s.tokenIgnored = ignored
	s.cfg.StatsToken = token

	users, cliPasswd, err := resolveAuth(rest, getenv)
	if err != nil {
		problems = append(problems, err)
	}
	s.users, s.cliPasswd = users, cliPasswd

	if f.localOnly {
		s.cfg.Listen = "127.0.0.1" + portOf(s.cfg.Listen)
	}
	s.cfg.Editable = f.edit
	// The security rule stands exactly as before; it is only collected with the rest.
	if err := checkEditAllowed(s.cfg.Editable, len(users) > 0, f.localOnly); err != nil {
		problems = append(problems, err)
	}
	return s, problems
}

// securitySummary describes what this instance exposes. Lines that deserve a second
// look start with "!".
func securitySummary(s setup, probesLoaded []string) []string {
	var out []string
	line := func(risky bool, name, text string) {
		mark := " "
		if risky {
			mark = "!"
		}
		out = append(out, fmt.Sprintf("%s %-9s %s", mark, name, text))
	}
	if s.localOnly {
		line(false, "listen", s.cfg.Listen+" (this machine only)")
	} else {
		line(true, "listen", s.cfg.Listen+" (every interface: reachable from the network)")
	}
	switch {
	case len(s.users) > 0 && s.cliPasswd:
		line(true, "login", fmt.Sprintf("on, %d user(s); the password is on the command line, visible in ps", len(s.users)))
	case len(s.users) > 0:
		line(false, "login", fmt.Sprintf("on, %d user(s)", len(s.users)))
	case s.localOnly:
		line(false, "login", "off (only this machine can connect; put a reverse proxy with login in front)")
	default:
		line(true, "login", "OFF: anyone who can reach the port can see the charts")
	}
	if s.cfg.Editable {
		line(false, "editing", "on (targets can be changed in the web UI)")
	} else {
		line(false, "editing", "off (read-only)")
	}
	if s.cfg.StatsToken != "" {
		line(false, "zabbix", "on: GET /api/stats with a read-only token")
	} else {
		line(false, "zabbix", "off")
	}
	switch {
	case !s.probes:
		line(false, "probes", "off (external probers are not loaded)")
	case len(probesLoaded) == 0:
		line(false, "probes", "on, none loaded")
	default:
		line(true, "probes", "on, running with fogping's privileges: "+strings.Join(probesLoaded, ", "))
	}
	return out
}

// runCheck is --check: it reports on the configuration and the machine and exits
// without starting anything. 0 means fogping would start; 1 means it would not.
func runCheck(w io.Writer, s setup, problems []error) int {
	fmt.Fprintln(w, "fogping --check: nothing is started; this only reports.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "configuration")
	if len(problems) == 0 {
		fmt.Fprintln(w, "  ok    flags, login and tokens")
	}
	for _, p := range problems {
		fmt.Fprintf(w, "  FAIL  %v\n", p)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "this machine")
	machine := checkMachine(s)
	for _, c := range machine {
		status := "ok  "
		if c.err != nil {
			status = "FAIL"
		}
		detail := c.detail
		if c.err != nil {
			detail = c.err.Error()
		}
		fmt.Fprintf(w, "  %s  %-9s %s\n", status, c.name, detail)
	}

	var loaded []string
	for _, c := range machine {
		loaded = append(loaded, c.probes...)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "security summary")
	for _, l := range securitySummary(s, loaded) {
		fmt.Fprintln(w, "  "+l)
	}

	failed := len(problems)
	for _, c := range machine {
		if c.err != nil && !c.warnOnly {
			failed++
		}
	}
	fmt.Fprintln(w)
	if failed > 0 {
		fmt.Fprintf(w, "result: fogping would NOT start (%d problem(s) above)\n", failed)
		return 1
	}
	fmt.Fprintln(w, "result: fogping would start")
	return 0
}

type machineCheck struct {
	name, detail string
	err          error
	warnOnly     bool     // reported, but fogping would still start (a skipped plugin)
	probes       []string // the plugins that would load
}

// checkMachine looks at what the start-up needs from this machine, without changing
// anything: the database is opened read-only and never migrated.
func checkMachine(s setup) []machineCheck {
	var out []machineCheck

	dataErr := checkDataDir(s.cfg.DataDir)
	out = append(out, machineCheck{name: "data", detail: s.cfg.DataDir + " is writable", err: dataErr})

	if v, err := databaseVersion(s.cfg.DataDir); err != nil {
		out = append(out, machineCheck{name: "database", err: err})
	} else if v < 0 {
		out = append(out, machineCheck{name: "database", detail: "none yet; a new one will be created"})
	} else if v > schemaVersion() {
		out = append(out, machineCheck{name: "database",
			err: fmt.Errorf("schema version %d was written by a newer fogping (this one knows up to %d)", v, schemaVersion())})
	} else {
		detail := fmt.Sprintf("schema version %d", v)
		if v < schemaVersion() {
			detail += fmt.Sprintf(", will be upgraded to %d at start (back up %s first)", schemaVersion(), s.cfg.DataDir)
		}
		out = append(out, machineCheck{name: "database", detail: detail})
	}

	if ln, err := net.Listen("tcp", s.cfg.Listen); err != nil {
		out = append(out, machineCheck{name: "port", err: fmt.Errorf("cannot listen on %s: %v", s.cfg.Listen, err)})
	} else {
		ln.Close()
		out = append(out, machineCheck{name: "port", detail: s.cfg.Listen + " is free"})
	}

	if s.probes {
		out = append(out, checkProbes(probesDir)...)
	}
	return out
}

// checkDataDir says whether the data directory exists and can be written, or can be
// created. It writes one empty file and removes it again.
func checkDataDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(filepath.Clean(dir))
		if err := writable(parent); err != nil {
			return fmt.Errorf("%s does not exist and cannot be created: %v", dir, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return writable(dir)
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".fogping-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// databaseVersion reads the schema version without changing the file: read-only,
// no migration. -1 means there is no database yet.
func databaseVersion(dir string) (int, error) {
	path := filepath.Join(dir, "fogping.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return -1, nil
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("cannot read %s: %v", path, err)
	}
	return v, nil
}

// checkProbes reports on each plugin directory exactly as --probes would treat it.
// A plugin that would be skipped is reported but does not stop fogping, so it is a
// warning, not a failure.
func checkProbes(dir string) []machineCheck {
	fi, err := os.Stat(dir)
	if err != nil {
		return []machineCheck{{name: "probes", warnOnly: true,
			err: fmt.Errorf("%s/ cannot be read (%v); no plugin would load", dir, err)}}
	}
	if why := untrusted(fi); why != "" {
		return []machineCheck{{name: "probes", warnOnly: true,
			err: fmt.Errorf("%s/ is %s; no plugin would load", dir, why)}}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []machineCheck{{name: "probes", warnOnly: true, err: err}}
	}
	var out []machineCheck
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, _, err := inspectProbe(filepath.Join(dir, e.Name()))
		if err != nil {
			out = append(out, machineCheck{name: "probe", warnOnly: true,
				err: fmt.Errorf("%s would be skipped: %v", e.Name(), err)})
			continue
		}
		out = append(out, machineCheck{name: "probe", detail: m.Type + " would load", probes: []string{m.Type}})
	}
	if len(out) == 0 {
		out = append(out, machineCheck{name: "probes", detail: dir + "/ holds no plugins"})
	}
	return out
}

// genToken writes a new random token to path, readable by its owner only. It never
// overwrites a file (an existing token may be in use) and never prints the token:
// it would end up in the terminal's scrollback and in logs.
func genToken(w io.Writer, path string) error {
	b := make([]byte, 32) // 256 bits from the operating system's CSPRNG
	if _, err := rand.Read(b); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; not overwriting it (it may be in use). Remove it first, or choose another path", path)
		}
		return err
	}
	if _, err := f.WriteString(hex.EncodeToString(b) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote a new 64-character token to %s (readable by you only).\n", path)
	fmt.Fprintf(w, "start fogping with it:\n  FOGPING_ZABBIX_TOKEN_FILE=%s ./fogping --zabbix\n", path)
	fmt.Fprintf(w, "and give the same token to the Zabbix template ({$FOGPING.TOKEN}).\n")
	return nil
}
