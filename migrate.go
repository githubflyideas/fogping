package main

import (
	"database/sql"
	"fmt"
	"log"
)

// Schema versions. The database records the last migration applied in
// PRAGMA user_version; NewStore applies the ones after it, in order, each in
// its own transaction together with the version bump, so a crash leaves the
// database at one version or the next, never between.
//
// Migrations are only ever appended. Editing one that has shipped would leave
// databases that ran the old text different from new ones.
//
// A database newer than this build knows is refused rather than guessed at:
// running an old fogping on it could quietly drop what the newer one stores.
var migrations = []func(tx *sql.Tx) error{
	migrateV1,
}

func schemaVersion() int { return len(migrations) }

// migrateV1 is the baseline: the schema fogping 1.0 through 1.3 built with
// CREATE IF NOT EXISTS and column checks. 1.0.x rows (name only) come out
// inactive; re-adding the name in the web UI reactivates the row and its history. It is written so that it brings any
// of those databases, or an empty one, to the same state.
func migrateV1(tx *sql.Tx) error {
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS targets (
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE
);

-- Raw rounds. Samples are a packed float32 blob: 4 bytes each, no parsing on read.
CREATE TABLE IF NOT EXISTS rounds (
  target_id INTEGER NOT NULL,
  t         INTEGER NOT NULL,
  sent      INTEGER NOT NULL,
  recv      INTEGER NOT NULL,
  samples   BLOB,
  burst     INTEGER NOT NULL DEFAULT 0,
  z         REAL    NOT NULL DEFAULT 0,
  PRIMARY KEY (target_id, t)
) WITHOUT ROWID;

-- Hourly rollup. Percentile columns are NULL for a bucket in which every packet was lost.
CREATE TABLE IF NOT EXISTS rounds_hourly (
  target_id INTEGER NOT NULL,
  t         INTEGER NOT NULL,
  lo REAL, p50 REAL, p90 REAL, p99 REAL, hi REAL,
  loss_pct REAL, bursts INTEGER, n INTEGER,
  PRIMARY KEY (target_id, t)
) WITHOUT ROWID;

DROP TABLE IF EXISTS rounds_daily;
`); err != nil {
		return err
	}
	have, err := columns(tx, "targets")
	if err != nil {
		return err
	}
	for _, col := range []struct{ name, def string }{
		{"type", "TEXT NOT NULL DEFAULT 'icmp'"},
		{"host", "TEXT NOT NULL DEFAULT ''"},
		{"port", "INTEGER NOT NULL DEFAULT 0"},
		{"pace", "TEXT NOT NULL DEFAULT ''"},
		{"interval_sec", "INTEGER NOT NULL DEFAULT 0"},
		{"active", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if have[col.name] {
			continue
		}
		if _, err := tx.Exec(`ALTER TABLE targets ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return fmt.Errorf("targets.%s: %w", col.name, err)
		}
	}
	return nil
}

func columns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// migrate brings db to schemaVersion().
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion() {
		return fmt.Errorf("database schema is version %d, this fogping knows up to %d: use a newer fogping", v, schemaVersion())
	}
	for i := v; i < schemaVersion(); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := migrations[i](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if v > 0 {
			log.Printf("storage: schema upgraded to version %d", i+1)
		}
	}
	return nil
}
