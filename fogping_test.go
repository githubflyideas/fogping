package main

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

func TestParseListLine(t *testing.T) {
	tt, err := parseListLine("59.43.247.1 HK CN2 pace=fast", "icmp")
	if err != nil || tt.Name != "HK CN2" || tt.Pace != "fast" {
		t.Fatalf("icmp line: %+v %v", tt, err)
	}
	tt, err = parseListLine("10.0.0.5:443 gw interval=30", "tcp")
	if err != nil || tt.Host != "10.0.0.5" || tt.Port != 443 || tt.IntervalSec != 30 {
		t.Fatalf("tcp line: %+v %v", tt, err)
	}
	if _, err := parseListLine("noport name", "tcp"); err == nil {
		t.Fatal("tcp without port should fail")
	}
}

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

func FuzzParseListLine(f *testing.F) {
	f.Add("1.2.3.4 name pace=fast", "icmp")
	f.Add("10.0.0.5:443 gw interval=30", "tcp")
	f.Add("host:99999 x", "tcp")
	f.Add("::1 v6", "icmp")
	f.Fuzz(func(t *testing.T, line, typ string) {
		if typ != "icmp" && typ != "tcp" {
			typ = "icmp"
		}
		if len(line) == 0 || line[0] == '#' {
			return
		}
		// 只要求不 panic、不接受空 host
		tt, err := parseListLine(line, typ)
		if err == nil && tt.Host == "" {
			t.Fatalf("accepted empty host: %q", line)
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

	got := s.ReadRange(context.Background(), "T1", now-60, now+60)
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
	dir := t.TempDir()
	s, err := NewStore(dir, []TargetCfg{{Name: "T2", Type: "icmp", Host: "1.1.1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 40 days of rounds, one per hour
	now := time.Now().Unix()
	for i := 0; i < 40*24; i++ {
		s.Append("T2", Round{T: now - int64(i)*3600, S: 20, R: 20,
			MS: []float64{40, 41, 42, 43}})
	}
	s.Flush()
	if err := s.Rollup(now - 41*86400); err != nil {
		t.Fatal(err)
	}

	// 6h window -> raw tier, exact rounds
	short := s.ReadRange(context.Background(), "T2", now-6*3600, now)
	// 10d window -> hourly tier
	mid := s.ReadRange(context.Background(), "T2", now-10*86400, now)
	// 40d window -> daily tier, must be far fewer rows than hourly
	long := s.ReadRange(context.Background(), "T2", now-40*86400, now)

	if len(short) == 0 || len(mid) == 0 || len(long) == 0 {
		t.Fatalf("empty tier: short=%d mid=%d long=%d", len(short), len(mid), len(long))
	}
	if len(long) > 60 {
		t.Fatalf("daily tier should collapse 40d into ~40 rows, got %d", len(long))
	}
	if len(mid) < len(long) {
		t.Fatalf("hourly tier should be denser than daily: %d vs %d", len(mid), len(long))
	}
	t.Logf("rows returned — 6h:%d  10d:%d  40d:%d", len(short), len(mid), len(long))
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
