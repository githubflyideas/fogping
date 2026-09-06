package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// fogping 2.0: a probe, text files, and a smoke graph. Nothing else.
// Config: v2.1 has no config file. These are the program's constants —
// the only things a user edits are targets/ping.list and tcp.list.
type Config struct {
	Listen        string
	DataDir       string
	TargetsDir    string
	Targets       []TargetCfg
	Probe         ProbeCfg
	HotDays       int // full samples kept this long
	RetentionDays int // downsampled data kept this long
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

// FinishConfig loads targets from the lists and validates them.
func FinishConfig(cfg *Config) error {
	listTargets, err := loadTargetLists(cfg.TargetsDir)
	if err != nil {
		return err
	}
	cfg.Targets = append(cfg.Targets, listTargets...)
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("no targets: add one line to %s/ping.list or tcp.list", cfg.TargetsDir)
	}
	return validateTargets(cfg)
}

type TargetCfg struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "icmp" (default) | "tcp"
	Host        string `json:"host"`
	Port        int    `json:"port"`         // tcp only
	Pace        string `json:"pace"`         // "fast"(15s) | "normal"(60s) | "slow"(300s)
	IntervalSec int    `json:"interval_sec"` // explicit seconds, highest priority
	dir         string
}

type ProbeCfg struct {
	IntervalSec int `json:"interval_sec"`
	Packets     int `json:"packets"`
	GapMs       int `json:"gap_ms"`
	TimeoutMs   int `json:"timeout_ms"`
}

var dirSan = regexp.MustCompile(`[^a-zA-Z0-9._\p{Han}-]`)

func validateTargets(cfg *Config) error {
	seen := map[string]bool{}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if t.Name == "" {
			return fmt.Errorf("target #%d has no name", i+1)
		}
		if t.Type == "" {
			t.Type = "icmp"
		}
		switch t.Type {
		case "icmp":
		case "tcp":
			if t.Port <= 0 {
				return fmt.Errorf("tcp target %q needs a port", t.Name)
			}
		default:
			return fmt.Errorf("target %q: unknown type %q", t.Name, t.Type)
		}
		switch t.Pace {
		case "", "fast", "normal", "slow":
		default:
			return fmt.Errorf("target %q: invalid pace %q (fast|slow)", t.Name, t.Pace)
		}
		t.dir = dirSan.ReplaceAllString(t.Name, "_")
		if seen[t.dir] {
			return fmt.Errorf("duplicate target name: %q", t.Name)
		}
		seen[t.dir] = true
	}
	return nil
}

// loadTargetLists reads targets/ping.list and tcp.list.
// line format: host[:port]  [name...]  [pace=fast|slow] [interval=sec]
func loadTargetLists(dir string) ([]TargetCfg, error) {
	var out []TargetCfg
	for _, spec := range []struct{ file, typ string }{{"ping.list", "icmp"}, {"tcp.list", "tcp"}} {
		raw, err := os.ReadFile(filepath.Join(dir, spec.file))
		if err != nil {
			continue
		}
		for ln, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			t, err := parseListLine(line, spec.typ)
			if err != nil {
				return nil, fmt.Errorf("%s/%s line %d: %w", dir, spec.file, ln+1, err)
			}
			out = append(out, t)
		}
	}
	return out, nil
}

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
