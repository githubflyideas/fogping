package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Program parameters are constants. Targets live in SQLite (the targets table);
// targets/*.list files are only an import inbox — see ingest.go.
type Config struct {
	Listen        string
	DataDir       string
	TargetsDir    string
	Probe         ProbeCfg
	HotDays       int  // full samples kept this long
	RetentionDays int  // downsampled data kept this long
	Editable      bool // --edit: web UI may create/update/delete targets
}

func defaultConfig() *Config {
	return &Config{
		Listen:        "0.0.0.0:8518",
		DataDir:       "./data",
		TargetsDir:    "./targets",
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

// normalizeTarget is the single validation gate: web CRUD and list import both
// pass through it, so the DB never holds a target the prober can't run.
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

// parseListFile reads one list file into validated targets. Any bad line rejects
// the whole file: a half-imported list is harder to reason about than none.
func parseListFile(path, typ string) ([]TargetCfg, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []TargetCfg
	seen := map[string]bool{}
	for ln, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		t, err := parseListLine(line, typ)
		if err == nil {
			err = normalizeTarget(&t)
		}
		if err == nil && seen[t.Name] {
			err = fmt.Errorf("duplicate name %q", t.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", filepath.Base(path), ln+1, err)
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	return out, nil
}

// line format: host[:port]  [name...]  [pace=fast|slow] [interval=sec]
func parseListLine(line, typ string) (TargetCfg, error) {
	t := TargetCfg{Type: typ}
	fields := strings.Fields(line)
	if len(fields) == 0 { // fuzz 发现:纯空白行会越界 —— 上游会挡,但函数自身必须皮实
		return t, fmt.Errorf("empty line")
	}
	addr := fields[0]
	if typ == "tcp" {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return t, fmt.Errorf("tcp target %q must be host:port", addr)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			return t, fmt.Errorf("tcp target %q: bad port", addr)
		}
		t.Host, t.Port = host, port
	} else {
		t.Host = addr
	}
	var nameParts []string
	for _, f := range fields[1:] {
		k, v, isKV := strings.Cut(f, "=")
		if !isKV {
			nameParts = append(nameParts, f)
			continue
		}
		switch k {
		case "pace":
			t.Pace = v
		case "interval":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return t, fmt.Errorf("interval=%q invalid", v)
			}
			t.IntervalSec = n
		} // unknown k=v silently ignored: forward compatibility
	}
	if len(nameParts) > 0 {
		t.Name = strings.Join(nameParts, " ")
	} else {
		t.Name = addr
	}
	return t, nil
}
