package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plugin writes a plugin directory: a manifest and a shell script as `probe`.
func plugin(t *testing.T, root, typ, manifest, script string, mode os.FileMode) string {
	t.Helper()
	d := filepath.Join(root, typ)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest == "" {
		manifest = `{"protocol":"fogping-probe/1","type":"` + typ + `","label":"X","options":[{"key":"word","label":"Word","kind":"string"}]}`
	}
	os.WriteFile(filepath.Join(d, "manifest.json"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(d, "probe"), []byte("#!/bin/sh\n"+script), mode)
	os.Chmod(filepath.Join(d, "probe"), mode)
	if typ != typeICMP && typ != typeTCP { // a test may reuse a built-in's name; it must never remove it
		t.Cleanup(func() { unregisterProber(typ) })
	}
	t.Cleanup(cleanupProbes)
	return d
}

func runPlugin(t *testing.T, typ string, packets int, opts map[string]any) (Round, error) {
	t.Helper()
	p, ok := proberFor(typ)
	if !ok {
		t.Fatalf("%s not registered", typ)
	}
	return p.round(context.Background(), TargetCfg{Type: typ, Host: "h.example", Options: opts}, packets, time.Millisecond, 10*time.Millisecond)
}

func TestExternalProberRound(t *testing.T) {
	root := t.TempDir()
	// Echoes what it was given so the test can see exactly what reaches it.
	plugin(t, root, "echo", "", `
req=$(cat)
case "$req" in *'"host":"h.example"'*'"word":"hi"'*) ;; *) echo "bad request: $req" >&2; exit 3;; esac
[ "$#" -eq 0 ] || { echo "argv not empty" >&2; exit 4; }
[ "$(env | grep -vE '^(PATH|PWD|SHLVL|_)=' | wc -l)" -eq 0 ] || { echo "env leaked: $(env)" >&2; exit 5; }
echo '{"protocol":"fogping-probe/1","sent":2,"received":2,"ms":[1.5,2.5]}'
`, 0o755)
	if got := loadProbes(root); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("loaded %v", got)
	}
	r, err := runPlugin(t, "echo", 2, map[string]any{"word": "hi"})
	if err != nil || r.S != 2 || r.R != 2 || len(r.MS) != 2 {
		t.Fatalf("round %+v err %v", r, err)
	}
}

func TestExternalProberFailures(t *testing.T) {
	root := t.TempDir()
	runMargin = 300 * time.Millisecond
	t.Cleanup(func() { runMargin = 5 * time.Second })
	plugin(t, root, "selferr", "", `echo '{"protocol":"fogping-probe/1","error":"no route"}'`, 0o755)
	plugin(t, root, "garbage", "", `echo 'not json'`, 0o755)
	plugin(t, root, "lying", "", `echo '{"protocol":"fogping-probe/1","sent":1,"received":5,"ms":[1,1,1,1,1]}'`, 0o755)
	plugin(t, root, "crash", "", `echo boom >&2; exit 7`, 0o755)
	plugin(t, root, "hang", "", `sleep 30`, 0o755)
	plugin(t, root, "flood", "", `yes aaaaaaaaaaaaaaaa | head -c 200000`, 0o755)
	if got := loadProbes(root); len(got) != 6 {
		t.Fatalf("loaded %v", got)
	}
	// The prober's own failure is a valid answer: a round that could not be measured.
	if r, err := runPlugin(t, "selferr", 1, nil); err != nil || r.S != 0 || r.E != "no route" {
		t.Errorf("selferr: %+v %v", r, err)
	}
	for typ, want := range map[string]string{
		"garbage": "answered badly", "lying": "answered badly", "crash": "boom",
		"hang": "did not answer", "flood": "larger than",
	} {
		t0 := time.Now()
		_, err := runPlugin(t, typ, 1, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", typ, err, want)
		}
		if typ == "hang" && time.Since(t0) > 4*time.Second {
			t.Errorf("hang took %v: not killed at the deadline", time.Since(t0))
		}
	}
}

func TestExternalProberStopsWithTheTarget(t *testing.T) {
	root := t.TempDir()
	plugin(t, root, "slow", "", `sleep 30`, 0o755)
	loadProbes(root)
	p, _ := proberFor("slow")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	t0 := time.Now()
	_, err := p.round(ctx, TargetCfg{Type: "slow", Host: "h"}, 1, time.Millisecond, time.Minute)
	if err != context.Canceled || time.Since(t0) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(t0))
	}
}

func TestExternalProbersAreSkippedWhenWrong(t *testing.T) {
	root := t.TempDir()
	ok := `echo '{"protocol":"fogping-probe/1","error":"x"}'`
	plugin(t, root, "good", "", ok, 0o755)
	plugin(t, root, "noexec", "", ok, 0o644)
	plugin(t, root, "writable", "", ok, 0o777)
	plugin(t, root, "badmanifest", `{"protocol":"fogping-probe/9","type":"badmanifest","label":"X"}`, ok, 0o755)
	plugin(t, root, "wrongdir", `{"protocol":"fogping-probe/1","type":"other","label":"X"}`, ok, 0o755)
	plugin(t, root, "icmp", `{"protocol":"fogping-probe/1","type":"icmp","label":"X"}`, ok, 0o755) // a built-in's name
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
	os.WriteFile(filepath.Join(root, "afile"), nil, 0o644)
	got := loadProbes(root)
	if len(got) != 1 || got[0] != "good" {
		t.Fatalf("loaded %v, want only good", got)
	}
	if p, _ := proberFor("icmp"); p.fileName != "ping" {
		t.Fatal("a plugin replaced the built-in icmp prober")
	}
	if got := loadProbes(filepath.Join(root, "missing")); got != nil {
		t.Fatalf("missing dir loaded %v", got)
	}
}

// An external prober is a first-class type: options are validated, a target is
// stored, listed for the UI and survives the targets file.
func TestExternalProberIsFirstClass(t *testing.T) {
	root := t.TempDir()
	plugin(t, root, "word", "", `echo '{"protocol":"fogping-probe/1","error":"x"}'`, 0o755)
	loadProbes(root)
	tg := TargetCfg{Type: "word", Host: "h", Options: map[string]any{"word": "w", "bogus": 1}}
	if err := normalizeTarget(&tg); err == nil || errCode(err) != codeUnknownOption {
		t.Fatalf("unknown option: %v", err)
	}
	tg = TargetCfg{Type: "word", Host: "h", Port: 99, Options: map[string]any{"word": "w"}}
	if err := normalizeTarget(&tg); err != nil || tg.Port != 0 {
		t.Fatalf("%v port=%d", err, tg.Port)
	}
	raw, _ := json.Marshal(toFileTarget(tg))
	if back, _, err := fromFileRow(raw); err != nil || back.OptString("word") != "w" {
		t.Fatalf("file round trip %+v %v", back, err)
	}
	found := false
	for _, pt := range probeTypes() {
		found = found || (pt.Type == "word" && len(pt.Options) == 1)
	}
	if !found {
		t.Fatal("not in probeTypes")
	}
}

// A directory others can write to could have its prober swapped for any program, so
// neither it nor a probes/ directory in that state is trusted.
func TestUntrustedDirectoriesAreRefused(t *testing.T) {
	root := t.TempDir()
	ok := `echo '{"protocol":"fogping-probe/1","error":"x"}'`
	d := plugin(t, root, "open", "", ok, 0o755)
	os.Chmod(d, 0o777)
	plugin(t, root, "fine", "", ok, 0o755)
	if got := loadProbes(root); len(got) != 1 || got[0] != "fine" {
		t.Fatalf("loaded %v, want only fine", got)
	}
	os.Chmod(root, 0o777)
	defer os.Chmod(root, 0o700)
	if got := loadProbes(root); got != nil {
		t.Fatalf("loaded %v from a world-writable probes directory", got)
	}
}
