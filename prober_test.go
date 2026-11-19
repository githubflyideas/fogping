package main

import (
	"context"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Only prober.go knows which probers exist. Anywhere else spelling out a type
// would be a second list that a new prober has to remember to update.
func TestNoCodeOutsideTheRegistryKnowsTheTypes(t *testing.T) {
	bad := regexp.MustCompile(`Type == "[a-z]|Type: "[a-z]|case "icmp"|case "tcp"|case "ping"`)
	files, _ := os.ReadDir(".")
	for _, f := range files {
		n := f.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "prober.go" {
			continue
		}
		b, _ := os.ReadFile(n)
		for i, line := range strings.Split(string(b), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d spells out a probe type: %s", n, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestBuiltInProbers(t *testing.T) {
	if got := proberNames(false); got != "http|icmp|tcp" {
		t.Errorf("names = %q", got)
	}
	if got := proberNames(true); got != "http|ping|tcp" {
		t.Errorf("file names = %q", got)
	}
	for in, want := range map[string]string{"ping": "icmp", "ICMP": "icmp", " tcp ": "tcp"} {
		if p, ok := proberForFile(in); !ok || p.name != want {
			t.Errorf("file type %q -> %v %v, want %s", in, p, ok, want)
		}
	}
	if targetAddr(TargetCfg{Type: "tcp", Host: "h", Port: 443}) != "h:443" || targetAddr(TargetCfg{Type: "icmp", Host: "h", Port: 443}) != "h" {
		t.Error("targetAddr")
	}
}

// fakeProber registers a prober for one test and removes it afterwards.
func fakeProber(t *testing.T, p *prober) {
	t.Helper()
	registerProber(p)
	t.Cleanup(func() { unregisterProber(p.name) })
}

// A new prober is one registerProber call: validation, addresses, the targets
// file and the probe loop all pick it up with nothing else changed.
func TestAddingAProberIsOneRegistration(t *testing.T) {
	rounds := make(chan TargetCfg, 4)
	fakeProber(t, &prober{
		name: "fake", fileName: "fake-file", usesPort: true,
		validate: func(t *TargetCfg) error { return nil },
		round: func(_ context.Context, tg TargetCfg, packets int, _, _ time.Duration) (Round, error) {
			rounds <- tg
			return Round{T: time.Now().Unix(), S: packets, R: packets, MS: []float64{1}}, nil
		},
	})
	tg := TargetCfg{Type: "fake", Host: "example.net", Port: 8080}
	if err := normalizeTarget(&tg); err != nil {
		t.Fatalf("validation: %v", err)
	}
	if tg.Name != "example.net:8080" {
		t.Errorf("default name %q", tg.Name)
	}
	var listed bool
	for _, pt := range probeTypes() {
		listed = listed || (pt.Type == "fake" && pt.UsesPort && pt.Label == "FAKE")
	}
	if !listed {
		t.Error("probeTypes (the UI list) does not include the new prober")
	}
	f := toFileTarget(tg)
	if f.Type != "fake-file" || f.Port != 8080 {
		t.Errorf("file row %+v", f)
	}
	back, _, err := fromFileRow([]byte(`{"type":"fake-file","host":"example.net","port":8080}`))
	if err != nil || back.Type != "fake" {
		t.Errorf("import: %+v %v", back, err)
	}

	s, err := NewStore(t.TempDir(), []TargetCfg{tg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { probeLoop(tg, ProbeCfg{IntervalSec: 3600, Packets: 3}, s, NewDetector(s), stop); close(done) }()
	select {
	case got := <-rounds:
		if got.Host != "example.net" {
			t.Errorf("round got %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe loop never called the new prober")
	}
	close(stop)
	<-done
	if n := len(s.Recent(tg.Name, 0)); n != 1 {
		t.Errorf("stored %d rounds, want 1", n)
	}
}

// Stopping a target cancels the round in flight; an external module will be
// killed through this.
func TestStopCancelsTheRoundInFlight(t *testing.T) {
	started := make(chan struct{})
	fakeProber(t, &prober{
		name:     "slow",
		validate: func(t *TargetCfg) error { return nil },
		round: func(ctx context.Context, tg TargetCfg, packets int, _, _ time.Duration) (Round, error) {
			close(started)
			<-ctx.Done() // a round that would otherwise never end
			return Round{T: time.Now().Unix(), S: packets}, ctx.Err()
		},
	})
	tg := TargetCfg{Name: "slow", Type: "slow", Host: "x"}
	s, _ := NewStore(t.TempDir(), []TargetCfg{tg})
	t.Cleanup(func() { s.Close() })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { probeLoop(tg, ProbeCfg{IntervalSec: 3600, Packets: 1}, s, NewDetector(s), stop); close(done) }()
	<-started
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping the target did not cancel its round")
	}
}

// A target whose prober is gone (an external module removed) writes nothing.
// It used to fall through a switch and store a zero round, time 0.
func TestMissingProberWritesNothing(t *testing.T) {
	tg := TargetCfg{Name: "orphan", Type: "gone", Host: "x"}
	s, _ := NewStore(t.TempDir(), []TargetCfg{tg})
	t.Cleanup(func() { s.Close() })
	done := make(chan struct{})
	go func() {
		probeLoop(tg, ProbeCfg{IntervalSec: 3600, Packets: 1}, s, NewDetector(s), make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probeLoop kept running without a prober")
	}
	if n := len(s.Recent("orphan", 0)); n != 0 {
		t.Errorf("stored %d rounds for a target with no prober", n)
	}
}

// A stop during the pause between packets ends the round at once, and a round
// cut short is never stored.
func TestTCPRoundStopsDuringGap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	port := ln.Addr().(*net.TCPAddr).Port
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	t0 := time.Now()
	_, err = tcpRound(ctx, "127.0.0.1", port, 5, time.Minute, time.Second)
	if err == nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("round not cut short: err=%v after %v", err, time.Since(t0))
	}
}
