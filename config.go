package main

import (
	"fmt"
	"net"
	"strconv"
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
	HotDays       int  // full samples kept this long
	RetentionDays int  // downsampled data kept this long
	Editable      bool // --edit: web UI may create/update/delete targets
}

func defaultConfig() *Config {
	return &Config{
		Listen:        "0.0.0.0:8518",
		DataDir:       "./data",
		Probe:         ProbeCfg{IntervalSec: 60, Packets: 20, GapMs: 50, TimeoutMs: 1000},
		HotDays:       2,  // raw samples: enough for the sub-day windows
		RetentionDays: 40, // hourly rollups; --days overrides
	}
}

type TargetCfg struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // "icmp" (default) | "tcp"
	Host        string `json:"host"`
	Port        int    `json:"port"`         // tcp only
	Pace        string `json:"pace"`         // "fast"(15s) | ""/"normal"(60s) | "slow"(300s)
	IntervalSec int    `json:"interval_sec"` // explicit seconds, highest priority
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
		t.Type = "icmp"
	}
	switch t.Type {
	case "icmp":
		t.Port = 0
	case "tcp":
		if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("tcp target needs a port 1-65535")
		}
	default:
		return fmt.Errorf("unknown type %q (icmp|tcp)", t.Type)
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

func targetAddr(t TargetCfg) string {
	if t.Type == "tcp" {
		return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	}
	return t.Host
}
