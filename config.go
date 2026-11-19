package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Program parameters are constants. Targets live in SQLite (the targets table) and
// are changed only through the web UI, when started with --edit.
type Config struct {
	Listen        string
	DataDir       string
	Probe         ProbeCfg
	HotDays       int    // full samples kept this long
	RetentionDays int    // downsampled data kept this long
	Editable      bool   // --edit: web UI may create/update/delete targets
	StatsToken    string // non-empty turns on GET /api/stats (stats.go)
}

func defaultConfig() *Config {
	return &Config{
		Listen:        "0.0.0.0:8518",
		DataDir:       "./data",
		Probe:         ProbeCfg{IntervalSec: 60, Packets: 20, GapMs: 50, TimeoutMs: 1000},
		HotDays:       2,           // raw samples: enough for the sub-day windows
		RetentionDays: defaultDays, // hourly rollups; --days overrides
	}
}

// Hourly history is kept defaultDays by default and never longer than maxDays.
const (
	defaultDays = 93
	maxDays     = 93
)

// retentionDays turns the --days value into the retention to use. 0 or negative
// keeps the default (as it always has); above maxDays is refused at startup rather
// than silently clamped, so nobody believes they are keeping a year of history.
func retentionDays(n int) (int, error) {
	if n > maxDays {
		return 0, fmt.Errorf("--days max is %d (got %d)", maxDays, n)
	}
	if n <= 0 {
		return defaultDays, nil
	}
	return n, nil
}

type TargetCfg struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // a registered prober (prober.go); "icmp" when empty
	Host        string `json:"host"`
	Port        int    `json:"port"`         // tcp only
	Pace        string `json:"pace"`         // "fast"(15s) | ""/"normal"(60s) | "slow"(300s)
	IntervalSec int    `json:"interval_sec"` // explicit seconds, highest priority
	// Options are the settings only this type's prober understands (options.go).
	Options map[string]any `json:"options,omitempty"`
}

type ProbeCfg struct {
	IntervalSec int
	Packets     int
	GapMs       int
	TimeoutMs   int
}

// normalizeTarget is the single validation gate for the web API, so the DB never
// holds a target the prober can't run.
func normalizeTarget(t *TargetCfg) error {
	t.Type = strings.TrimSpace(t.Type)
	t.Host = strings.TrimSpace(t.Host)
	t.Name = strings.TrimSpace(t.Name)
	t.Pace = strings.TrimSpace(t.Pace)
	if t.Type == "" {
		t.Type = typeICMP
	}
	p, ok := proberFor(t.Type)
	if !ok {
		return fmt.Errorf("unknown type %q (%s)", t.Type, proberNames(false))
	}
	if err := normalizeOptions(p, t); err != nil {
		return err
	}
	if err := p.validate(t); err != nil {
		return err
	}
	if t.Host == "" || len(t.Host) > 253 || strings.ContainsFunc(t.Host, unicode.IsSpace) {
		return fmt.Errorf("invalid host %q", t.Host)
	}
	switch t.Pace {
	case "normal":
		t.Pace = ""
	case "", "fast", "slow":
	default:
		return fmt.Errorf("invalid pace %q (fast|normal|slow)", t.Pace)
	}
	if t.IntervalSec < 0 || t.IntervalSec > 86400 {
		return fmt.Errorf("interval must be 1-86400 seconds (0 = use pace)")
	}
	if t.Name == "" {
		t.Name = targetAddr(*t)
	}
	if utf8.RuneCountInString(t.Name) > 64 || strings.ContainsFunc(t.Name, unicode.IsControl) {
		return fmt.Errorf("name must be at most 64 printable characters")
	}
	return nil
}
