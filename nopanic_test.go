package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Nothing a user does may stop the program from starting, so production code never
// calls panic. A mistake in an optional feature (a plugin, a bad Zabbix token, a line
// of a targets file) is reported and the feature is left off; the monitoring runs.
// This keeps it that way: a new panic in non-test code fails here.
func TestNoPanicInProductionCode(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`\bpanic\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "//") {
				continue
			}
			if re.MatchString(line) {
				t.Errorf("%s:%d: panic in production code: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
