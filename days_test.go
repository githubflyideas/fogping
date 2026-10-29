package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// --days: default 93, 93 is accepted, anything above is refused (not clamped),
// and 0/negative keep the default as before.
func TestRetentionDays(t *testing.T) {
	if got := defaultConfig().RetentionDays; got != 93 {
		t.Fatalf("default retention = %d, want 93", got)
	}
	for _, c := range []struct{ in, want int }{{0, 93}, {-5, 93}, {1, 1}, {30, 30}, {90, 90}, {93, 93}} {
		got, err := retentionDays(c.in)
		if err != nil || got != c.want {
			t.Errorf("retentionDays(%d) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, n := range []int{94, 120, 300} {
		if _, err := retentionDays(n); err == nil || !strings.Contains(err.Error(), "--days max is 93") {
			t.Errorf("retentionDays(%d) err = %v, want '--days max is 93'", n, err)
		}
	}
}

// The console reads hot_days to know when the compare overlay must ask for
// hourly buckets instead of raw rounds.
func TestVersionReportsHotDays(t *testing.T) {
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := newMux(&Config{HotDays: 3, RetentionDays: 93}, s, NewRunner(ProbeCfg{}, s, NewDetector(s)), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/version", nil))
	var v struct {
		HotDays       int `json:"hot_days"`
		RetentionDays int `json:"retention_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.HotDays != 3 || v.RetentionDays != 93 {
		t.Errorf("hot_days=%d retention_days=%d, want 3 and 93", v.HotDays, v.RetentionDays)
	}
}
