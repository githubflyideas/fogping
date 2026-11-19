package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrateFresh(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := userVersion(t, s.db); v != schemaVersion() {
		t.Fatalf("fresh db at version %d, want %d", v, schemaVersion())
	}
	s.Close()
	// Reopening runs nothing and keeps the version.
	s, err = NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := userVersion(t, s.db); v != schemaVersion() {
		t.Fatalf("reopened db at version %d", v)
	}
}

// A 1.0.x database: name-only targets, a daily rollup, user_version 0.
func TestMigrateFromUnversioned(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "fogping.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE targets (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE rounds_daily (target_id INTEGER, t INTEGER)`,
		`INSERT INTO targets(name) VALUES('old')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := userVersion(t, s.db); v != schemaVersion() {
		t.Fatalf("migrated db at version %d", v)
	}
	var typ string
	var active int
	if err := s.db.QueryRow(`SELECT type, active FROM targets WHERE name='old'`).Scan(&typ, &active); err != nil {
		t.Fatal(err)
	}
	if typ != typeICMP || active != 0 {
		t.Fatalf("old row type=%q active=%d", typ, active)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='rounds_daily'`).Scan(&n)
	if n != 0 {
		t.Fatal("rounds_daily not dropped")
	}
}

func TestMigrateRefusesNewer(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "fogping.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = NewStore(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "newer fogping") {
		t.Fatalf("want refusal, got %v", err)
	}
}

func TestProbeErrorIsNotLoss(t *testing.T) {
	s, err := NewStore(t.TempDir(), []TargetCfg{{Name: "x", Type: typeICMP, Host: "h"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Unix()
	good := Round{T: now - 20, S: 10, R: 10, MS: []float64{5, 5}}
	bad := Round{T: now - 10, E: "no ICMP socket"}
	for _, r := range []Round{good, bad} {
		if err := s.Append("x", r); err != nil {
			t.Fatal(err)
		}
	}
	st := calcStats(s.Recent("x", now-60))
	if st.Rounds != 1 || st.LossPct != 0 || st.ProbeErrors != 1 {
		t.Fatalf("stats %+v", st)
	}
	if b, _ := NewDetector(s).CheckBurst("x", bad); b {
		t.Fatal("probe error flagged as burst")
	}
	s.Flush()
	rows := s.queryRaw(context.Background(), "x", now-60, now)
	if len(rows) != 2 || rows[1].E != "no ICMP socket" {
		t.Fatalf("stored rows %+v", rows)
	}
}
