package main

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestProbeParams(t *testing.T) {
	g := ProbeCfg{IntervalSec: 60, Packets: 20}
	if iv, pk := probeParams(TargetCfg{Pace: "fast"}, g); iv.Seconds() != 15 || pk != 30 {
		t.Fatalf("fast: %v %d", iv, pk)
	}
	if iv, _ := probeParams(TargetCfg{Pace: "fast", IntervalSec: 7}, g); iv.Seconds() != 7 {
		t.Fatalf("explicit interval wins: %v", iv)
	}
}

func TestRobustZ(t *testing.T) {
	base := make([]float64, 60)
	for i := range base {
		base[i] = float64(i % 3) // 0,1,2 loss pattern
	}
	if z := robustZ(9, base); z < 3 {
		t.Fatalf("9 losses vs 0-2 baseline should be anomalous, z=%v", z)
	}
}

func BenchmarkRobustZ(b *testing.B) {
	base := make([]float64, 240) // 4h 基线 @60s
	for i := range base {
		base[i] = float64(i % 3)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		robustZ(9, base)
	}
}

func BenchmarkCalcStats24h(b *testing.B) {
	rounds := make([]Round, 1440) // 24h @60s
	for i := range rounds {
		ms := make([]float64, 20)
		for j := range ms {
			ms[j] = 38 + float64(j%7)
		}
		rounds[i] = Round{T: int64(i * 60), S: 20, R: 20, MS: ms}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		calcStats(rounds)
	}
}

// normalizeTarget is the only gate between the web API and the targets table.
func TestNormalizeTarget(t *testing.T) {
	ok := []TargetCfg{
		{Host: " 59.43.247.1 ", Name: "HK CN2", Pace: "fast"},
		{Type: "tcp", Host: "10.0.0.5", Port: 443, IntervalSec: 30},
		{Host: "example.com", Pace: "normal"},
	}
	for _, c := range ok {
		if err := normalizeTarget(&c); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	c := TargetCfg{Type: "tcp", Host: "10.0.0.5", Port: 443}
	normalizeTarget(&c)
	if c.Name != "10.0.0.5:443" {
		t.Fatalf("tcp default name: %q", c.Name)
	}
	c = TargetCfg{Host: "1.1.1.1", Pace: "normal"}
	normalizeTarget(&c)
	if c.Pace != "" || c.Name != "1.1.1.1" || c.Type != "icmp" {
		t.Fatalf("defaults: %+v", c)
	}
	bad := []TargetCfg{
		{Host: ""},
		{Host: "a b"},
		{Type: "tcp", Host: "h"},
		{Type: "tcp", Host: "h", Port: 70000},
		{Type: "udp", Host: "h"},
		{Host: "h", Pace: "turbo"},
		{Host: "h", IntervalSec: -1},
		{Host: "h", Name: strings.Repeat("x", 65)},
		{Host: "h", Name: "a\x00b"},
	}
	for _, c := range bad {
		if err := normalizeTarget(&c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func FuzzNormalizeTarget(f *testing.F) {
	f.Add("icmp", "1.1.1.1", 0, "", "fast", 0)
	f.Add("tcp", "10.0.0.5", 443, "gw", "", 30)
	f.Add("", " ", -1, "\x00", "normal", 99999)
	f.Fuzz(func(t *testing.T, typ, host string, port int, name, pace string, iv int) {
		c := TargetCfg{Type: typ, Host: host, Port: port, Name: name, Pace: pace, IntervalSec: iv}
		if normalizeTarget(&c) != nil {
			return
		}
		// whatever gets in must be something the prober can run and the UI can show
		switch {
		case c.Host == "" || strings.ContainsAny(c.Host, " \t\n"):
			t.Fatalf("bad host accepted: %q", c.Host)
		case c.Type != "icmp" && c.Type != "tcp":
			t.Fatalf("bad type accepted: %q", c.Type)
		case c.Type == "tcp" && (c.Port < 1 || c.Port > 65535):
			t.Fatalf("bad port accepted: %d", c.Port)
		case c.Name == "" || utf8.RuneCountInString(c.Name) > 64:
			t.Fatalf("bad name accepted: %q", c.Name)
		}
	})
}

func FuzzRobustZ(f *testing.F) {
	f.Add(float64(5), []byte{1, 2, 3, 0, 1})
	f.Fuzz(func(t *testing.T, x float64, raw []byte) {
		series := make([]float64, len(raw))
		for i, b := range raw {
			series[i] = float64(b)
		}
		z := robustZ(x, series) // 不 panic、不产出 NaN 即可
		if z != z {
			t.Fatalf("NaN: x=%v series=%v", x, series)
		}
	})
}

// SQLite storage: round trip, tier selection, rollup correctness.
func TestSQLiteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tg := TargetCfg{Name: "T1", Type: "icmp", Host: "1.1.1.1"}
	s, err := NewStore(dir, []TargetCfg{tg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().Unix()
	in := Round{T: now, S: 20, R: 19, MS: []float64{40.1, 41.2, 39.8}, B: true, Z: 3.42}
	if err := s.Append("T1", in); err != nil {
		t.Fatal(err)
	}
	s.Flush()

	got := s.ReadRange(context.Background(), "T1", now-60, now+60).Rounds
	if len(got) != 1 {
		t.Fatalf("want 1 round, got %d", len(got))
	}
	r := got[0]
	if r.T != in.T || r.S != in.S || r.R != in.R || !r.B {
		t.Fatalf("metadata lost: %+v", r)
	}
	if len(r.MS) != 3 || math.Abs(r.MS[0]-40.1) > 0.01 {
		t.Fatalf("samples wrong: %v", r.MS)
	}
	if math.Abs(r.Z-3.42) > 0.01 {
		t.Fatalf("z lost: %v", r.Z)
	}
}

func TestTierSelection(t *testing.T) {
	s, err := NewStore(t.TempDir(), []TargetCfg{{Name: "T2", Type: "icmp", Host: "1.1.1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Unix()
	for i := 0; i < 40*24; i++ { // 40 days, one round per hour
		s.Append("T2", Round{T: now - int64(i)*3600, S: 20, R: 20, MS: []float64{40, 41, 42, 43}})
	}
	s.Flush()
	if err := s.Rollup(now-41*86400, now-41*86400); err != nil {
		t.Fatal(err)
	}
	short := s.ReadRange(context.Background(), "T2", now-6*3600, now)
	long := s.ReadRange(context.Background(), "T2", now-40*86400, now)
	if short.Tier != "raw" || len(short.Rounds) == 0 {
		t.Fatalf("6h should be raw rounds: %+v", short.Tier)
	}
	if long.Tier != "hourly" || len(long.Buckets) < 40*24-1 {
		t.Fatalf("40d should be ~960 hourly buckets, got %s/%d", long.Tier, len(long.Buckets))
	}
}

// Deleting rows must actually hand disk back, not just mark pages reusable.
func TestReclaimShrinksFile(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, []TargetCfg{{Name: "R", Type: "icmp", Host: "1.1.1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// write a chunk of history, then flush
	now := time.Now().Unix()
	ms := make([]float64, 20)
	for i := range ms {
		ms[i] = 40 + float64(i)
	}
	for i := 0; i < 60000; i++ {
		s.Append("R", Round{T: now - int64(i)*60, S: 20, R: 20, MS: ms})
	}
	s.Flush()

	dbPath := dir + "/fogping.db"
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// drop nearly all of it
	if _, err := s.db.Exec(`DELETE FROM rounds WHERE t < ?`, now-60); err != nil {
		t.Fatal(err)
	}
	s.reclaim()

	after, _ := os.Stat(dbPath)
	t.Logf("db size: %d KB -> %d KB", before.Size()/1024, after.Size()/1024)
	if after.Size() >= before.Size() {
		t.Fatalf("file did not shrink: %d -> %d", before.Size(), after.Size())
	}
}

func TestSplitArgsAnyOrder(t *testing.T) {
	f, a := splitArgs([]string{"user=admin", "passwd=x", "--edit", "--days", "300", "--localhost=true"})
	if strings.Join(f, " ") != "--edit --days 300 --localhost=true" || strings.Join(a, " ") != "user=admin passwd=x" {
		t.Fatalf("flags=%q auth=%q", f, a)
	}
}
