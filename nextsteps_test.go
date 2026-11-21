package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// The startup "next steps" block is the spoon-feeding a first-time user
// reads: it must offer the login line when there is no login, the --edit
// line in the form this start's own mode needs, and nothing it cannot use.
func TestPrintNextStepsSpoonFeeds(t *testing.T) {
	capture := func(cfg *Config, users map[string]string, localOnly bool) string {
		var buf bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(prev) })
		printNextSteps(cfg, users, localOnly)
		return buf.String()
	}

	// A plain first start: no login, read-only targets.
	out := capture(defaultConfig(), nil, false)
	for _, want := range []string{
		"./fogping user=admin passwd=change-me",         // the login line
		"FOGPING_PASSWD=change-me ./fogping user=admin", // the private way to set it
		"./fogping --edit user=admin passwd=change-me",  // the edit line, complete
		"./fogping help", // the way to everything else
	} {
		if !strings.Contains(out, want) {
			t.Errorf("first start: next steps missing %q in:\n%s", want, out)
		}
	}

	// A start that has a login already: no login lines, the short edit line.
	cfg := defaultConfig()
	out = capture(cfg, map[string]string{"admin": "a-long-password"}, false)
	if strings.Contains(out, "set a login") {
		t.Errorf("with a login: next steps still offer one:\n%s", out)
	}
	if !strings.Contains(out, "./fogping --edit (with your usual login)") {
		t.Errorf("with a login: next steps missing the short edit line:\n%s", out)
	}

	// Localhost: no login lines (the proxy's job), the localhost edit line.
	out = capture(defaultConfig(), nil, true)
	if strings.Contains(out, "set a login") || strings.Contains(out, "passwd=change-me") {
		t.Errorf("localhost: next steps offer a login line:\n%s", out)
	}
	if !strings.Contains(out, "./fogping --localhost --edit") {
		t.Errorf("localhost: next steps missing the localhost edit line:\n%s", out)
	}

	// Editable already: no edit line at all, only the help pointer.
	cfg = defaultConfig()
	cfg.Editable = true
	out = capture(cfg, map[string]string{"admin": "a-long-password"}, false)
	if strings.Contains(out, "add targets") {
		t.Errorf("editable: next steps still offer the edit line:\n%s", out)
	}
	if !strings.Contains(out, "./fogping help") {
		t.Errorf("editable: next steps missing the help pointer:\n%s", out)
	}
}
