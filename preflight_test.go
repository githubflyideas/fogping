package main

import (
	"bytes"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

// A refused start names every problem at once, and each refusal is the same as
// before: --edit without a login, --zabbix without a token, too many days.
func TestPrepareReportsEveryProblem(t *testing.T) {
	_, problems := prepare(startFlags{days: maxDays + 1, edit: true, zabbix: true}, nil, noEnv)
	if len(problems) != 3 {
		t.Fatalf("want 3 problems, got %d: %v", len(problems), problems)
	}
	joined := ""
	for _, p := range problems {
		joined += p.Error() + "\n"
	}
	for _, want := range []string{"--days", "--zabbix needs a token", "--edit needs a login"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	s, problems := prepare(startFlags{days: 30, edit: true}, []string{"user=admin", "passwd=pw"}, noEnv)
	if len(problems) != 0 || !s.cfg.Editable || len(s.users) != 1 || !s.cliPasswd {
		t.Fatalf("good setup: %+v %v", s, problems)
	}
}

func TestSecuritySummaryMarksWhatIsExposed(t *testing.T) {
	open, _ := prepare(startFlags{days: 30}, nil, noEnv)
	text := strings.Join(securitySummary(open, nil), "\n")
	for _, want := range []string{"! listen", "! login     OFF"} {
		if !strings.Contains(text, want) {
			t.Errorf("open instance: missing %q in\n%s", want, text)
		}
	}
	runningAsRoot = func() bool { return false }
	defer func() { runningAsRoot = func() bool { return os.Geteuid() == 0 } }()
	local, _ := prepare(startFlags{days: 30, localOnly: true}, nil, noEnv)
	if text := strings.Join(securitySummary(local, nil), "\n"); strings.Contains(text, "!") {
		t.Errorf("localhost-only instance flagged:\n%s", text)
	}
	pw, _ := prepare(startFlags{days: 30}, []string{"user=a", "passwd=b"}, noEnv)
	if text := strings.Join(securitySummary(pw, nil), "\n"); !strings.Contains(text, "visible in ps") {
		t.Errorf("command-line password not flagged:\n%s", text)
	}
}

func checkSetup(t *testing.T, dir string) setup {
	t.Helper()
	s, problems := prepare(startFlags{days: 30, localOnly: true}, nil, noEnv)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	s.cfg.DataDir = dir
	s.cfg.Listen = "127.0.0.1:0"
	return s
}

// --check never starts anything and never changes the database: no file appears in
// an empty directory, and an old database is not migrated.
func TestCheckChangesNothing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if code := runCheck(&out, checkSetup(t, dir), nil); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "fogping.db")); err == nil {
		t.Fatal("--check created a database")
	}
	if !strings.Contains(out.String(), "none yet") || !strings.Contains(out.String(), "would start") {
		t.Fatalf("report:\n%s", out.String())
	}

	db, _ := sql.Open("sqlite3", filepath.Join(dir, "fogping.db"))
	db.Exec(`PRAGMA user_version = 1`)
	db.Close()
	out.Reset()
	runCheck(&out, checkSetup(t, dir), nil)
	if !strings.Contains(out.String(), "will be upgraded") {
		t.Fatalf("old database not reported:\n%s", out.String())
	}
	if v, _ := databaseVersion(dir); v != 1 {
		t.Fatalf("--check migrated the database to %d", v)
	}
}

func TestCheckFindsWhatWouldStopTheStart(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite3", filepath.Join(dir, "fogping.db"))
	db.Exec(`PRAGMA user_version = 999`)
	db.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	s := checkSetup(t, dir)
	s.cfg.Listen = ln.Addr().String() // taken
	var out bytes.Buffer
	_, problems := prepare(startFlags{days: 30, edit: true}, nil, noEnv)
	if code := runCheck(&out, s, problems); code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"FAIL  --edit needs a login", "newer fogping", "cannot listen", "would NOT start (3 problem(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
}

// A plugin that would be skipped is a warning in --check, not a failure: fogping
// would still start without it.
func TestCheckReportsPluginsWithoutLoadingThem(t *testing.T) {
	root := t.TempDir()
	ok := `echo '{"protocol":"fogping-probe/1","error":"x"}'`
	plugin(t, root, "good", "", ok, 0o755)
	plugin(t, root, "noexec", "", ok, 0o644)
	report := checkProbes(root)
	var loads, skips int
	for _, c := range report {
		if c.err != nil && c.warnOnly && strings.Contains(c.err.Error(), "noexec would be skipped") {
			skips++
		}
		if c.err == nil && len(c.probes) == 1 && c.probes[0] == "good" {
			loads++
		}
	}
	if loads != 1 || skips != 1 {
		t.Fatalf("report %+v", report)
	}
	if _, registered := proberFor("good"); registered {
		t.Fatal("--check registered a plugin")
	}
}

func TestGenTokenIsStrongPrivateAndNeverPrinted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zabbix.token")
	var out bytes.Buffer
	if err := genToken(&out, path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	token := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Fatalf("token %q", token)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if strings.Contains(out.String(), token) {
		t.Fatal("the token was printed")
	}
	// It is accepted by the token rules fogping applies at start.
	got, _, _, err := resolveZabbixToken(true, []string{"zabbix-token-file=" + path}, noEnv)
	if err != nil || got != token {
		t.Fatalf("not accepted: %v", err)
	}
	// An existing file is never overwritten.
	if err := genToken(&out, path); err == nil {
		t.Fatal("overwrote an existing token")
	}
	if b2, _ := os.ReadFile(path); string(b2) != string(b) {
		t.Fatal("the existing token changed")
	}
}

func TestSecuritySummaryFlagsRootPlainHTTPAndWeakPasswords(t *testing.T) {
	old := runningAsRoot
	runningAsRoot = func() bool { return true }
	defer func() { runningAsRoot = old }()
	s, _ := prepare(startFlags{days: 30}, []string{"user=a,b", "passwd=short,long-enough-pw"}, noEnv)
	text := strings.Join(securitySummary(s, nil), "\n")
	for _, want := range []string{"! account", "! transport", "1 password(s) shorter than 8"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	runningAsRoot = func() bool { return false }
	local, _ := prepare(startFlags{days: 30, localOnly: true}, nil, noEnv)
	if text := strings.Join(securitySummary(local, nil), "\n"); strings.Contains(text, "!") {
		t.Errorf("a local, ordinary-account instance is flagged:\n%s", text)
	}
}
