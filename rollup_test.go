package main

import (
	"context"
	"testing"
	"time"
)

// Bug: rollup only ran at 00:05, so a fresh or restarted instance had no hourly
// buckets and every window over 24h was empty. The first housekeeping tick must
// catch up on everything raw holds.
func TestRollupCatchUpOnStart(t *testing.T) {
	s, _ := NewStore(t.TempDir(), []TargetCfg{{Name: "A"}})
	defer s.Close()
	now := time.Now()
	for i := 0; i < 36*60; i++ {
		s.Append("A", Round{T: now.Unix() - int64(i)*60, S: 20, R: 20, MS: []float64{40, 41}})
	}
	s.Flush()
	(&rollupSched{hot: 48 * time.Hour}).tick(s, now)
	got := s.ReadRange(context.Background(), "A", now.Unix()-3*86400, now.Unix())
	if len(got.Buckets) < 36 {
		t.Fatalf("3d window right after start: %d buckets, want ~37", len(got.Buckets))
	}
}

// Bug: Rollup(now-2d) with an unaligned `since` rewrote the bucket containing it from
// partial data just before retention deleted the rest, truncating it permanently
// (a JST host kept 535 of 1440 minutes per daily bucket). Drive the real scheduler
// through three days on a UTC+9 clock, probing as time passes, with nightly
// retention — every complete hour must end up with all 60 rounds.
func TestRollupSurvivesRetention(t *testing.T) {
	s, _ := NewStore(t.TempDir(), []TargetCfg{{Name: "A"}})
	defer s.Close()
	jst := time.FixedZone("JST", 9*3600)
	start := time.Date(2026, 9, 1, 13, 7, 0, 0, jst)
	end := start.Add(72 * time.Hour)
	sched := &rollupSched{hot: 48 * time.Hour}
	lastRet := ""
	for now := start; now.Before(end); now = now.Add(time.Minute) {
		s.Append("A", Round{T: now.Unix(), S: 20, R: 20, MS: []float64{40}})
		if now.Minute()%5 != 0 {
			continue
		}
		s.Flush()
		sched.tick(s, now)
		if d := now.Format("2006-01-02"); now.Hour() == 0 && now.Minute() == 5 && lastRet != d {
			lastRet = d
			s.Retention(now, 2, 40)
		}
	}
	s.Flush()
	sched.tick(s, end.Add(10*time.Minute))

	first := (start.Unix()/3600 + 1) * 3600 // first hour the run covered completely
	last := end.Unix()/3600*3600 - 3600
	rows, _ := s.db.Query(`SELECT t, n FROM rounds_hourly WHERE t >= ? AND t <= ? ORDER BY t`, first, last)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var ts int64
		var n int
		rows.Scan(&ts, &n)
		if n != 60 {
			t.Errorf("bucket %s: n=%d, want 60", time.Unix(ts, 0).In(jst).Format("01-02 15:04"), n)
		}
		seen++
	}
	if want := int((last-first)/3600) + 1; seen != want {
		t.Fatalf("hourly buckets: got %d, want %d", seen, want)
	}
}

// Bug: hourly rows went out as Rounds with S=n rounds and R derived from the loss
// percentage, so anything under half a round per bucket (0.8% at 60 rounds/h) was
// rounded to 0% — typical internet loss vanished from every chart over 24h. And
// the five percentiles were shipped as if they were samples; the chart took the
// middle one (P90) as its median line.
func TestHourlyBucketShape(t *testing.T) {
	s, _ := NewStore(t.TempDir(), []TargetCfg{{Name: "A"}})
	defer s.Close()
	h := time.Now().Unix()/3600*3600 - 7200
	for i := 0; i < 60; i++ {
		ms := make([]float64, 0, 20)
		for j := 0; j < 20; j++ {
			ms = append(ms, float64(30+j)) // per round: 30..49 ms
		}
		r := Round{T: h + int64(i)*60, S: 20, R: 20, MS: ms}
		if i == 7 { // one lost packet in 1200
			r.R, r.MS = 19, ms[:19]
		}
		s.Append("A", r)
	}
	s.Append("A", Round{T: h + 3600, S: 20, R: 0}) // an hour where everything was lost
	s.Flush()
	if err := s.Rollup(h, h); err != nil {
		t.Fatal(err)
	}
	got := s.ReadRange(context.Background(), "A", h-86400, h+7200).Buckets
	if len(got) != 2 {
		t.Fatalf("want 2 buckets, got %+v", got)
	}
	b := got[0]
	if b.Loss < 0.08 || b.Loss > 0.09 {
		t.Errorf("1/1200 lost should be ~0.083%%, got %.4f%%", b.Loss)
	}
	if len(b.Q) != 5 || b.Q[0] != 30 || b.Q[1] != 39 || b.Q[4] != 49 {
		t.Errorf("percentiles wrong: %v", b.Q)
	}
	if got[1].Q != nil || got[1].Loss != 100 {
		t.Errorf("all-lost hour should have no percentiles and 100%% loss: %+v", got[1])
	}
}
