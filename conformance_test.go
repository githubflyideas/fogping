package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The vectors in docs/conformance are the standard in executable form: every
// implementation of the probe protocol, in any language, must give these answers.

type vectorFile[T any] struct {
	Vectors []T `json:"vectors"`
}

func loadVectors[T any](t *testing.T, name string) []T {
	t.Helper()
	b, err := os.ReadFile("docs/conformance/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var f vectorFile[T]
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(f.Vectors) == 0 {
		t.Fatalf("%s has no vectors", name)
	}
	return f.Vectors
}

func TestConformanceManifests(t *testing.T) {
	type vec struct {
		Name     string          `json:"name"`
		Manifest json.RawMessage `json:"manifest"`
		Error    string          `json:"error"`
	}
	for _, v := range loadVectors[vec](t, "manifest.json") {
		_, err := parseManifest(v.Manifest)
		if got := errCode(err); got != v.Error || (v.Error == "" && err != nil) {
			t.Errorf("%s: got %q (%v), want %q", v.Name, got, err, v.Error)
		}
	}
}

func TestConformanceOptions(t *testing.T) {
	type vec struct {
		Name    string         `json:"name"`
		Options []optionSpec   `json:"options"`
		Input   map[string]any `json:"input"`
		Want    map[string]any `json:"want"`
		Error   string         `json:"error"`
	}
	for _, v := range loadVectors[vec](t, "options.json") {
		tg := TargetCfg{Options: v.Input}
		err := normalizeOptions(&prober{name: "vec", options: v.Options}, &tg)
		if v.Error != "" {
			if got := errCode(err); got != v.Error {
				t.Errorf("%s: got code %q (%v), want %q", v.Name, got, err, v.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		// Compare as JSON: ints are int here and float64 in the file.
		got, _ := json.Marshal(tg.Options)
		var a, b any
		json.Unmarshal(got, &a)
		want, _ := json.Marshal(v.Want)
		json.Unmarshal(want, &b)
		if len(v.Want) == 0 {
			a, b = len(tg.Options), 0
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: got %s, want %s", v.Name, got, want)
		}
	}
}

// Every built-in prober's declaration is a valid manifest of its own: the
// standard describes what fogping itself does.
func TestBuiltinsAreValidManifests(t *testing.T) {
	for _, pt := range probeTypes() {
		raw, _ := json.Marshal(Manifest{Protocol: probeProtocol, Type: pt.Type, Label: pt.Label, UsesPort: pt.UsesPort, Options: pt.Options})
		if _, err := parseManifest(raw); err != nil {
			t.Errorf("%s: %v", pt.Type, err)
		}
	}
}
