package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func optionProber(t *testing.T) {
	fakeProber(t, &prober{
		name: "optfake",
		options: []optionSpec{
			{Key: "record", Label: "Record", Kind: optChoice, Choices: []string{"A", "AAAA", "MX"}, Default: "A"},
			{Key: "retries", Label: "Retries", Kind: optInt, Min: 0, Max: 5},
			{Key: "name", Label: "Query", Kind: optString, Required: true, MaxLen: 20},
			{Key: "tcp", Label: "Use TCP", Kind: optBool},
		},
		validate: func(*TargetCfg) error { return nil },
		round: func(_ context.Context, tg TargetCfg, packets int, _, _ time.Duration) (Round, error) {
			return Round{T: time.Now().Unix(), S: packets, R: packets, MS: []float64{1}}, nil
		},
	})
}

func TestOptionsValidation(t *testing.T) {
	optionProber(t)
	cases := []struct {
		name string
		opts map[string]any
		want string // error substring; "" = accepted
	}{
		{"ok with default filled", map[string]any{"name": "example.org"}, ""},
		{"all given", map[string]any{"name": "x", "record": "MX", "retries": float64(3), "tcp": true}, ""},
		{"missing required", map[string]any{"record": "A"}, `option "name" is required`},
		{"unknown key", map[string]any{"name": "x", "recrd": "A"}, `unknown option "recrd"`},
		{"bad choice", map[string]any{"name": "x", "record": "ZZ"}, "one of A|AAAA|MX"},
		{"int range", map[string]any{"name": "x", "retries": float64(9)}, "0-5"},
		{"int fraction", map[string]any{"name": "x", "retries": 1.5}, "whole number"},
		{"wrong type", map[string]any{"name": "x", "tcp": "yes"}, "true or false"},
		{"too long", map[string]any{"name": strings.Repeat("a", 21)}, "at most 20"},
	}
	for _, c := range cases {
		tg := TargetCfg{Type: "optfake", Host: "h", Options: c.opts}
		err := normalizeTarget(&tg)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
	tg := TargetCfg{Type: "optfake", Host: "h", Options: map[string]any{"name": "x"}}
	if err := normalizeTarget(&tg); err != nil || tg.Options["record"] != "A" {
		t.Fatalf("default not applied: %v %v", tg.Options, err)
	}
}

func TestBuiltinsTakeNoOptions(t *testing.T) {
	tg := TargetCfg{Type: typeICMP, Host: "h", Options: map[string]any{"x": 1}}
	if err := normalizeTarget(&tg); err == nil || !strings.Contains(err.Error(), "no options") {
		t.Fatalf("got %v", err)
	}
}

// Options survive the database, the targets file and the API shape.
func TestOptionsRoundTrip(t *testing.T) {
	optionProber(t)
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tg := TargetCfg{Name: "dns", Type: "optfake", Host: "h", Options: map[string]any{"name": "example.org", "retries": 2, "tcp": true}}
	if err := normalizeTarget(&tg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTarget(tg); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ActiveTargets()
	if len(got) != 1 || got[0].OptString("name") != "example.org" || got[0].OptInt("retries") != 2 ||
		!got[0].OptBool("tcp") || got[0].OptString("record") != "A" {
		t.Fatalf("stored %+v", got)
	}
	// File export and import.
	f := exportTargets(got, "fogping", time.Now())
	raw, _ := json.Marshal(f.Targets[0])
	back, _, err := fromFileRow(raw)
	if err != nil || back.OptString("name") != "example.org" {
		t.Fatalf("file round trip: %+v %v", back, err)
	}
	// A file row with a bad option is rejected on its own row.
	if _, _, err := fromFileRow([]byte(`{"type":"optfake","host":"h","options":{"name":"x","retries":99}}`)); err == nil {
		t.Fatal("bad option imported")
	}
	// The same options compare equal, so a reload does not restart the target.
	again, _ := s.ActiveTargets()
	if !sameTarget(got[0], again[0]) {
		t.Fatal("identical targets compare different")
	}
}

// A prober with a malformed declaration, or a name already taken, is refused with
// an error and registers nothing. It must never panic: a plugin cannot stop fogping
// from starting.
func TestBadSpecIsRefusedNotAPanic(t *testing.T) {
	for name, specs := range map[string][]optionSpec{
		"bad key":   {{Key: "Bad-Key", Label: "x", Kind: optString}},
		"dup":       {{Key: "a", Label: "x", Kind: optString}, {Key: "a", Label: "y", Kind: optString}},
		"no label":  {{Key: "a", Kind: optString}},
		"bad dflt":  {{Key: "a", Label: "x", Kind: optInt, Min: 1, Default: 0}},
		"no choice": {{Key: "a", Label: "x", Kind: optChoice}},
	} {
		before := len(probeTypes())
		err := registerProber(&prober{name: "badspec", options: specs, validate: func(*TargetCfg) error { return nil }})
		if err == nil {
			unregisterProber("badspec")
			t.Errorf("%s: accepted", name)
			continue
		}
		if _, ok := proberFor("badspec"); ok || len(probeTypes()) != before {
			t.Errorf("%s: refused but still registered", name)
		}
	}
	// A duplicate name is refused too, and the first registration is untouched.
	if err := registerProber(&prober{name: typeICMP, validate: func(*TargetCfg) error { return nil }}); err == nil {
		t.Error("a second prober named icmp was accepted")
	}
	if p, _ := proberFor(typeICMP); p.fileName != "ping" {
		t.Error("the built-in icmp prober was replaced")
	}
}

func TestProbeTypesExposeOptions(t *testing.T) {
	optionProber(t)
	for _, pt := range probeTypes() {
		if pt.Type == "optfake" && len(pt.Options) == 4 && pt.Options[0].Key == "record" {
			return
		}
	}
	t.Fatal("options missing from probeTypes")
}

// sameTarget lists TargetCfg's fields by hand. A new field must be added there,
// or changing it would not restart the running probe; this fails until it is.
func TestSameTargetCoversEveryField(t *testing.T) {
	if n := reflect.TypeOf(TargetCfg{}).NumField(); n != 8 {
		t.Fatalf("TargetCfg has %d fields; update sameTarget (options.go) and this number", n)
	}
}
