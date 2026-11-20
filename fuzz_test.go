package main

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Fuzz tests for everything that reads input fogping does not control: a prober's
// manifest, a prober's answer, a user's options, and a targets file. Each one checks
// two things for ANY input: it never panics, and when it accepts the input, what it
// returns satisfies the rules the rest of fogping relies on. "Accepted" is where
// bugs hide: a parser that accepts something it should not hands it to code that
// trusts it.
//
// `go test` runs the seeds only (the conformance vectors and a few hand-made
// cases). To search for new inputs:  go test -run '^$' -fuzz FuzzParseManifest -fuzztime 30s

// seedVectors adds the "want accepted" and "want refused" raw inputs of a
// conformance file to the corpus, so the fuzzer starts from realistic documents.
func seedVectors(f *testing.F, file, field string) {
	b, err := os.ReadFile("docs/conformance/" + file)
	if err != nil {
		f.Fatal(err)
	}
	var doc struct {
		Vectors []map[string]json.RawMessage `json:"vectors"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		f.Fatal(err)
	}
	for _, v := range doc.Vectors {
		if raw, ok := v[field]; ok {
			f.Add([]byte(raw))
		}
	}
}

func FuzzParseManifest(f *testing.F) {
	seedVectors(f, "manifest.json", "manifest")
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"protocol":"fogping-probe/1","type":"x","label":"X","options":null}`))
	typeRe := regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	f.Fuzz(func(t *testing.T, raw []byte) {
		m, err := parseManifest(raw)
		if err != nil {
			if errCode(err) == "" {
				t.Fatalf("an error with no code: %v", err)
			}
			return
		}
		if m.Protocol != probeProtocol || !typeRe.MatchString(m.Type) {
			t.Fatalf("accepted protocol %q type %q", m.Protocol, m.Type)
		}
		if n := utf8.RuneCountInString(m.Label); n < 1 || n > 16 {
			t.Fatalf("accepted a label of %d characters", n)
		}
		if err := validateSpecs(m.Options); err != nil {
			t.Fatalf("accepted options that fail validation: %v", err)
		}
	})
}

func FuzzParseRoundResult(f *testing.F) {
	seedVectors(f, "round.json", "result")
	f.Add([]byte(`{"protocol":"fogping-probe/1","sent":1,"received":1,"ms":[1e999]}`))
	f.Add([]byte(`{"protocol":"fogping-probe/1","sent":1,"received":1,"ms":[-0]}`))
	f.Add([]byte(`{"protocol":"fogping-probe/1","error":"\u0000\u0007x"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		const packets = 10
		r, err := parseRoundResult(raw, 100, packets)
		if err != nil {
			if errCode(err) == "" {
				t.Fatalf("an error with no code: %v", err)
			}
			return
		}
		if r.T != 100 {
			t.Fatalf("time %d", r.T)
		}
		if r.E != "" { // the prober's own failure: a round that could not be measured
			if r.S != 0 || r.R != 0 || len(r.MS) != 0 {
				t.Fatalf("an error round carries a measurement: %+v", r)
			}
			if utf8.RuneCountInString(r.E) > maxErrorChars {
				t.Fatalf("error text of %d characters", utf8.RuneCountInString(r.E))
			}
			for _, c := range r.E {
				if unicode.IsControl(c) {
					t.Fatalf("control character %U in the error text", c)
				}
			}
			return
		}
		if r.S < 1 || r.S > packets || r.R < 0 || r.R > r.S || len(r.MS) != r.R {
			t.Fatalf("inconsistent counts: %+v", r)
		}
		for _, v := range r.MS {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > maxRTTms {
				t.Fatalf("sample %v", v)
			}
		}
	})
}

// fuzzSpecs has one option of every kind, so one corpus exercises all of them.
var fuzzSpecs = []optionSpec{
	{Key: "name", Label: "Name", Kind: optString, Required: true, MaxLen: 16},
	{Key: "mode", Label: "Mode", Kind: optChoice, Choices: []string{"a", "b"}, Default: "a"},
	{Key: "count", Label: "Count", Kind: optInt, Min: 1, Max: 9},
	{Key: "flag", Label: "Flag", Kind: optBool},
}

func FuzzNormalizeOptions(f *testing.F) {
	seedVectors(f, "options.json", "input")
	f.Add([]byte(`{"name":"x","count":1e300}`))
	f.Add([]byte(`{"name":"x","count":-0.0}`))
	f.Add([]byte(`{"name":["x"],"mode":{"a":1}}`))
	f.Add([]byte(`{"name":"\u202e\u0000"}`))
	p := &prober{name: "fuzz", options: fuzzSpecs}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var in map[string]any
		if json.Unmarshal(raw, &in) != nil {
			return // not an object: the API layer refuses it before this
		}
		tg := TargetCfg{Options: in}
		if err := normalizeOptions(p, &tg); err != nil {
			if errCode(err) == "" {
				t.Fatalf("an error with no code: %v", err)
			}
			return
		}
		allowed := map[string]bool{"name": true, "mode": true, "count": true, "flag": true}
		for k, v := range tg.Options {
			if !allowed[k] {
				t.Fatalf("an undeclared key %q survived", k)
			}
			switch k {
			case "name", "mode":
				if _, ok := v.(string); !ok {
					t.Fatalf("%s is %T", k, v)
				}
			case "count":
				if n, ok := v.(int); !ok || n < 1 || n > 9 {
					t.Fatalf("count is %#v", v)
				}
			case "flag":
				if _, ok := v.(bool); !ok {
					t.Fatalf("flag is %T", v)
				}
			}
		}
		if _, ok := tg.Options["name"]; !ok {
			t.Fatal("a required option is missing from an accepted target")
		}
		// Normalising is idempotent: what comes out is what a stored target looks like,
		// and it must be accepted again as it is.
		again := TargetCfg{Options: tg.Options}
		if err := normalizeOptions(p, &again); err != nil {
			t.Fatalf("the output is refused when fed back: %v", err)
		}
		if encodeOptions(again.Options) != encodeOptions(tg.Options) {
			t.Fatalf("not idempotent: %s then %s", encodeOptions(tg.Options), encodeOptions(again.Options))
		}
		if len(encodeOptions(tg.Options)) > maxOptionBytes {
			t.Fatal("accepted options over the size limit")
		}
	})
}

// A targets file row: whatever the bytes, an accepted row must be a target the editor
// would accept as it stands.
func FuzzFromFileRow(f *testing.F) {
	f.Add([]byte(`{"type":"ping","host":"10.0.0.1"}`))
	f.Add([]byte(`{"type":"tcp","host":"example.org","port":443,"pace":"fast","interval":10}`))
	f.Add([]byte(`{"type":"http","host":"example.org","port":443,"options":{"path":"/x?a=b","expect_status":200}}`))
	f.Add([]byte(`{"type":"http","host":"a/b","port":1}`))
	f.Add([]byte(`{"type":"tcp","host":"999.999.999.999","port":99999}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"type":"ping","host":"h","extra":{"a":[1,2,3]}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		tg, _, err := fromFileRow(raw)
		if err != nil {
			return
		}
		again := tg
		again.Options = nil
		if tg.Options != nil {
			again.Options = map[string]any{}
			for k, v := range tg.Options {
				again.Options[k] = v
			}
		}
		if err := normalizeTarget(&again); err != nil {
			t.Fatalf("an accepted row is refused by the editor's own rules: %v (%+v)", err, tg)
		}
		if p, ok := proberFor(tg.Type); !ok {
			t.Fatalf("accepted a row of unknown type %q", tg.Type)
		} else if p.usesPort && (tg.Port < 1 || tg.Port > 65535) {
			t.Fatalf("accepted port %d", tg.Port)
		}
		if numericButNotIP(tg.Host) {
			t.Fatalf("accepted host %q", tg.Host)
		}
	})
}
