package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Docs regressions: no pointer to a presets/ directory that does not exist, no
// pinned version in the sample startup log, and every documented tar extraction
// ignores archive ownership (older release tarballs carry uid/gid 1001, which a
// root user inside a container cannot chown to).
func TestDocsStayAccurate(t *testing.T) {
	docs := []string{"README.md", "docs/GUIDE.md", "docs/targets-format.md"}
	tarX := regexp.MustCompile(`\btar\s+(-?x\S*|-\S*x\S*)`)
	for _, p := range docs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if _, err := os.Stat("presets"); err != nil && strings.Contains(s, "presets/") {
			t.Errorf("%s mentions presets/, which does not exist", p)
		}
		for _, line := range strings.Split(s, "\n") {
			if tarX.MatchString(line) && !strings.Contains(line, "--no-same-owner") {
				t.Errorf("%s: tar extraction without --no-same-owner: %s", p, strings.TrimSpace(line))
			}
		}
	}
	g, _ := os.ReadFile("docs/GUIDE.md")
	if !strings.Contains(string(g), "fogping X.Y.Z up ·") {
		t.Error("GUIDE: sample startup log should show `fogping X.Y.Z up`, not a pinned version")
	}
	if regexp.MustCompile(`fogping \d+\.\d+\.\d+ up`).Match(g) {
		t.Error("GUIDE: sample startup log pins a version that goes stale")
	}
}
