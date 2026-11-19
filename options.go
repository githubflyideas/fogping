package main

// Per-target options: the settings only one prober understands.
//
// Every target has the same core fields (name, host, port, pace, interval). A
// prober that needs more — a DNS record type, an SMTP greeting, a TTL — declares
// them as option specs when it registers. fogping then does the rest: the
// editor form is built from the specs, values are validated against them, stored
// with the target, exported, imported, and handed to the prober's round
// function as TargetCfg.Options. A prober author writes the spec and reads the
// values; nothing else in fogping knows the option exists.
//
// The rules, which external authors can rely on:
//   - A prober's options are a flat object: keys to string, integer, boolean or
//     one-of-a-list values. Nothing nested.
//   - Unknown keys are refused, so a typo is an error, not a silently ignored
//     setting.
//   - Options never change the meaning of a core field. They are additive.
//   - New options may be added to a prober later if they are optional or have a
//     default; an option that exists is not renamed or retyped. That keeps stored
//     targets and exported files working across versions.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxOptions     = 20
	maxOptionBytes = 4096
	defaultMaxLen  = 256
)

// Option kinds.
const (
	optString = "string"
	optInt    = "int"
	optBool   = "bool"
	optChoice = "choice"
)

// Error codes. They are part of the standard (docs/PROBE-PROTOCOL.md): every
// implementation reports the same code for the same mistake, and the conformance
// vectors check codes, not wording.
const (
	codeUnknownOption = "unknown_option"
	codeRequired      = "required"
	codeWrongType     = "wrong_type"
	codeOutOfRange    = "out_of_range"
	codeBadChoice     = "bad_choice"
	codeTooLong       = "too_long"
	codeBadValue      = "bad_value"
	codeTooLarge      = "too_large"

	codeBadProtocol    = "bad_protocol"
	codeBadType        = "bad_type"
	codeBadLabel       = "bad_label"
	codeBadKey         = "bad_key"
	codeDuplicateKey   = "duplicate_key"
	codeBadKind        = "bad_kind"
	codeMissingChoices = "missing_choices"
	codeBadDefault     = "bad_default"
	codeTooManyOptions = "too_many_options"
)

// codedError is an error with a stable code for programs.
type codedError struct{ code, msg string }

func (e *codedError) Error() string { return e.msg }

func errCode(err error) string {
	var c *codedError
	if errors.As(err, &c) {
		return c.code
	}
	return ""
}

func coded(code, format string, a ...any) error {
	return &codedError{code: code, msg: fmt.Sprintf(format, a...)}
}

var optionKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// optionSpec declares one option of a prober. It is sent to the UI as JSON.
type optionSpec struct {
	Key      string   `json:"key"`   // lowercase snake_case
	Label    string   `json:"label"` // shown next to the input
	Help     string   `json:"help,omitempty"`
	Kind     string   `json:"kind"`
	Required bool     `json:"required,omitempty"`
	Default  any      `json:"default,omitempty"`
	Min      int      `json:"min,omitempty"`     // int
	Max      int      `json:"max,omitempty"`     // int; 0 means no upper bound
	MaxLen   int      `json:"max_len,omitempty"` // string, default 256
	Choices  []string `json:"choices,omitempty"` // choice
}

// validateSpecs checks a prober's option declarations.
func validateSpecs(specs []optionSpec) error {
	if len(specs) > maxOptions {
		return coded(codeTooManyOptions, "more than %d options", maxOptions)
	}
	seen := map[string]bool{}
	for _, s := range specs {
		switch {
		case !optionKeyRe.MatchString(s.Key):
			return coded(codeBadKey, "option %q: key must be lowercase snake_case, at most 32 characters", s.Key)
		case seen[s.Key]:
			return coded(codeDuplicateKey, "option %q declared twice", s.Key)
		case strings.TrimSpace(s.Label) == "":
			return coded(codeBadLabel, "option %q needs a label", s.Key)
		}
		seen[s.Key] = true
		switch s.Kind {
		case optString, optInt, optBool:
		case optChoice:
			if len(s.Choices) == 0 {
				return coded(codeMissingChoices, "option %q: choice needs choices", s.Key)
			}
		default:
			return coded(codeBadKind, "option %q: unknown kind %q", s.Key, s.Kind)
		}
		if s.Default != nil {
			if _, err := coerceOption(s, s.Default); err != nil {
				return coded(codeBadDefault, "option %q: default %v", s.Key, err)
			}
		}
	}
	return nil
}

// checkSpecs panics on a malformed declaration made in Go: that is a bug in the
// prober, and it must show up when fogping starts, not when someone opens the
// editor.
func checkSpecs(prober string, specs []optionSpec) {
	if err := validateSpecs(specs); err != nil {
		panic(fmt.Sprintf("prober %s: %v", prober, err))
	}
}

// coerceOption checks one value against its spec and returns it in canonical
// form: string, int, or bool. JSON numbers arrive as float64.
func coerceOption(s optionSpec, v any) (any, error) {
	switch s.Kind {
	case optString, optChoice:
		str, ok := v.(string)
		if !ok {
			return nil, coded(codeWrongType, "must be text")
		}
		str = strings.TrimSpace(str)
		if strings.ContainsFunc(str, unicode.IsControl) {
			return nil, coded(codeBadValue, "must not contain control characters")
		}
		if s.Kind == optChoice {
			for _, c := range s.Choices {
				if str == c {
					return str, nil
				}
			}
			return nil, coded(codeBadChoice, "must be one of %s", strings.Join(s.Choices, "|"))
		}
		max := s.MaxLen
		if max == 0 {
			max = defaultMaxLen
		}
		if utf8.RuneCountInString(str) > max {
			return nil, coded(codeTooLong, "must be at most %d characters", max)
		}
		return str, nil
	case optInt:
		var n int
		switch x := v.(type) {
		case float64:
			if x != float64(int(x)) {
				return nil, coded(codeWrongType, "must be a whole number")
			}
			n = int(x)
		case int:
			n = x
		default:
			return nil, coded(codeWrongType, "must be a number")
		}
		if n < s.Min || (s.Max != 0 && n > s.Max) {
			if s.Max != 0 {
				return nil, coded(codeOutOfRange, "must be %d-%d", s.Min, s.Max)
			}
			return nil, coded(codeOutOfRange, "must be at least %d", s.Min)
		}
		return n, nil
	case optBool:
		b, ok := v.(bool)
		if !ok {
			return nil, coded(codeWrongType, "must be true or false")
		}
		return b, nil
	}
	return nil, fmt.Errorf("unknown kind")
}

// normalizeOptions validates t.Options against the prober's specs, fills in
// defaults, and leaves the canonical map (nil when the prober has none).
func normalizeOptions(p *prober, t *TargetCfg) error {
	if len(t.Options) == 0 && len(p.options) == 0 {
		t.Options = nil
		return nil
	}
	specs := map[string]optionSpec{}
	for _, s := range p.options {
		specs[s.Key] = s
	}
	var unknown []string
	for k := range t.Options {
		if _, ok := specs[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		var keys []string
		for _, s := range p.options {
			keys = append(keys, s.Key)
		}
		have := "this type has no options"
		if len(keys) > 0 {
			have = "options of " + p.name + ": " + strings.Join(keys, ", ")
		}
		return coded(codeUnknownOption, "unknown option %q (%s)", unknown[0], have)
	}
	out := map[string]any{}
	for _, s := range p.options {
		v, given := t.Options[s.Key]
		if str, isStr := v.(string); isStr {
			v = strings.TrimSpace(str) // blank text counts as missing
		}
		if !given || v == nil || v == "" {
			switch {
			case s.Default != nil:
				v = s.Default
			case s.Required:
				return coded(codeRequired, "option %q is required", s.Key)
			default:
				continue
			}
		}
		c, err := coerceOption(s, v)
		if err != nil {
			return coded(errCode(err), "option %q %v", s.Key, err)
		}
		out[s.Key] = c
	}
	if len(out) == 0 {
		t.Options = nil
		return nil
	}
	if b, _ := json.Marshal(out); len(b) > maxOptionBytes {
		return coded(codeTooLarge, "options are too large (at most %d bytes)", maxOptionBytes)
	}
	t.Options = out
	return nil
}

// encodeOptions is the stored form: canonical JSON (keys sorted), "" for none.
func encodeOptions(o map[string]any) string {
	if len(o) == 0 {
		return ""
	}
	b, _ := json.Marshal(o)
	return string(b)
}

func decodeOptions(s string) map[string]any {
	if s == "" {
		return nil
	}
	var o map[string]any
	if json.Unmarshal([]byte(s), &o) != nil {
		return nil
	}
	return o
}

// Option accessors for probers: the values are already validated, so these only
// pick them out. A missing key gives the zero value.
func (t TargetCfg) OptString(key string) string { s, _ := t.Options[key].(string); return s }
func (t TargetCfg) OptBool(key string) bool     { b, _ := t.Options[key].(bool); return b }
func (t TargetCfg) OptInt(key string) int {
	switch n := t.Options[key].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// sameTarget says whether a reload changed anything the running probe depends on.
// TargetCfg holds a map, so it cannot be compared with ==. Stored numbers come
// back from JSON as float64 while a freshly validated map holds int, so compare
// the canonical encoding rather than the maps.
func sameTarget(a, b TargetCfg) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Type == b.Type && a.Host == b.Host &&
		a.Port == b.Port && a.Pace == b.Pace && a.IntervalSec == b.IntervalSec &&
		encodeOptions(a.Options) == encodeOptions(b.Options)
}
