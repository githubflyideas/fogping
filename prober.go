package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A prober is one way of measuring a target: ICMP echo, a TCP connect, and later
// whatever an external module adds. This registry is the only place that knows
// which probers exist. Validation, the probe loop, the address shown for a
// target and the targets file all ask it, so adding a prober is one
// registerProber call and nothing else in fogping changes.
type prober struct {
	// name is the target type as stored and as the API speaks it: "icmp".
	name string
	// label is the short name the UI shows: "PING".
	label string
	// fileName is the type in a netping-targets file ("ping" for icmp), so files
	// stay readable by pingping. Defaults to name.
	fileName string
	// usesPort says the target has a meaningful port field.
	usesPort bool
	// options declares the settings this prober takes beyond the core fields
	// (options.go). The editor form, validation, storage and the targets file all
	// follow from it.
	options []optionSpec
	// validate checks and normalises the fields only this prober cares about.
	// The fields every target has (name, host, pace, interval) are checked by
	// normalizeTarget for all of them.
	validate func(t *TargetCfg) error
	// round measures the target once: packets probes, gap apart, each waiting at
	// most timeout. ctx is cancelled when the target stops.
	round func(ctx context.Context, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error)
}

// The built-in probers' names. Everything else spells a type through these.
const (
	typeICMP = "icmp"
	typeTCP  = "tcp"
)

var (
	probers = map[string]*prober{}
	order   []string // registration order: the order the UI lists types in
)

// registerProber adds a prober. Names are unique; a duplicate is a programming
// error, caught at start.
func registerProber(p *prober) {
	if p.fileName == "" {
		p.fileName = p.name
	}
	if _, dup := probers[p.name]; dup {
		panic("prober registered twice: " + p.name)
	}
	checkSpecs(p.name, p.options)
	if p.label == "" {
		p.label = strings.ToUpper(p.name)
	}
	probers[p.name] = p
	order = append(order, p.name)
}

// probeTypeInfo is what the UI needs to list and edit a type.
type probeTypeInfo struct {
	Type     string       `json:"type"`
	Label    string       `json:"label"`
	UsesPort bool         `json:"uses_port"`
	Options  []optionSpec `json:"options"`
}

func probeTypes() []probeTypeInfo {
	out := make([]probeTypeInfo, 0, len(order))
	for _, n := range order {
		p := probers[n]
		out = append(out, probeTypeInfo{Type: p.name, Label: p.label, UsesPort: p.usesPort, Options: append([]optionSpec{}, p.options...)})
	}
	return out
}

// unregisterProber removes a prober. Only tests need it: in the program the
// registry is filled at start and never shrinks.
func unregisterProber(name string) {
	delete(probers, name)
	for i, n := range order {
		if n == name {
			order = append(order[:i:i], order[i+1:]...)
			break
		}
	}
}

func proberFor(typ string) (*prober, bool) {
	p, ok := probers[typ]
	return p, ok
}

// proberForFile finds a prober by the type a targets file uses, or by its own
// name, ignoring case.
func proberForFile(fileType string) (*prober, bool) {
	ft := strings.ToLower(strings.TrimSpace(fileType))
	for _, p := range probers {
		if ft == p.fileName || ft == p.name {
			return p, true
		}
	}
	return nil, false
}

// proberNames lists the types for error messages, e.g. "icmp|tcp".
func proberNames(file bool) string {
	var names []string
	for _, p := range probers {
		if file {
			names = append(names, p.fileName)
		} else {
			names = append(names, p.name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// targetAddr is the address a target is shown with: host, or host:port for a
// prober that uses a port.
func targetAddr(t TargetCfg) string {
	if p, ok := proberFor(t.Type); ok && p.usesPort {
		return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	}
	return t.Host
}

func init() {
	registerProber(&prober{
		name:     typeICMP,
		label:    "PING",
		fileName: "ping",
		validate: func(t *TargetCfg) error {
			t.Port = 0
			return nil
		},
		round: func(ctx context.Context, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error) {
			return icmpRound(ctx, t.Host, packets, gap, timeout)
		},
	})
	registerProber(&prober{
		name:     typeTCP,
		usesPort: true,
		validate: func(t *TargetCfg) error {
			if t.Port < 1 || t.Port > 65535 {
				return fmt.Errorf("tcp target needs a port 1-65535")
			}
			return nil
		},
		round: func(ctx context.Context, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error) {
			return tcpRound(ctx, t.Host, t.Port, packets, gap, timeout)
		},
	})
}
