package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
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
