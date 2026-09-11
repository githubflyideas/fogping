package main

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattn/go-sqlite3"
)

// Targets live in the same table that already maps names to ids for the rounds.
// A row is either active (probed, shown) or inactive: a deleted target whose
// history is kept until retention ages it out. Re-adding the same name reactivates
// the row, so its history resumes.

var (
	errNotFound  = errors.New("target not found")
	errNameTaken = errors.New("a target with this name already exists")
	// Deleted rows keep their name so re-adding it restores history; renaming onto
	// one would silently merge two histories, so that path is refused.
	errNameDeleted = errors.New("name belongs to a deleted target — add it as a new target to restore its history")
)

// migrateTargets adds the config columns to a pre-existing name-only table (1.0.x).
// Old rows come out inactive; adding a target with the same name in the web UI
// reactivates the row, and with it the history.
func migrateTargets(db *sql.DB) error {
	have := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(targets)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
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
		if _, err := db.Exec(`ALTER TABLE targets ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return fmt.Errorf("migrate targets.%s: %w", col.name, err)
		}
	}
	return nil
}

func (s *Store) ActiveTargets() ([]TargetCfg, error) {
	rows, err := s.db.Query(`SELECT id,name,type,host,port,pace,interval_sec
	                           FROM targets WHERE active=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TargetCfg
	for rows.Next() {
		var t TargetCfg
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Host, &t.Port, &t.Pace, &t.IntervalSec); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TargetRows counts every row, active or not — zero means a brand-new database.
func (s *Store) TargetRows() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM targets`).Scan(&n)
	return n, err
}

const upsertTarget = `INSERT INTO targets(name,type,host,port,pace,interval_sec,active)
VALUES(?,?,?,?,?,?,1)
ON CONFLICT(name) DO UPDATE SET type=excluded.type, host=excluded.host, port=excluded.port,
  pace=excluded.pace, interval_sec=excluded.interval_sec, active=1`

// CreateTarget adds a target, or reactivates a deleted one of the same name. It
// refuses to overwrite an active target: that's what UpdateTarget is for.
func (s *Store) CreateTarget(t TargetCfg) (TargetCfg, error) {
	res, err := s.db.Exec(upsertTarget+` WHERE targets.active=0`,
		t.Name, t.Type, t.Host, t.Port, t.Pace, t.IntervalSec)
	if err != nil {
		return t, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return t, errNameTaken
	}
	err = s.db.QueryRow(`SELECT id FROM targets WHERE name=?`, t.Name).Scan(&t.ID)
	return t, err
}

// UpdateTarget rewrites an active target by id. Renames keep the id, so they keep history.
func (s *Store) UpdateTarget(t TargetCfg) (TargetCfg, error) {
	res, err := s.db.Exec(`UPDATE targets SET name=?,type=?,host=?,port=?,pace=?,interval_sec=?
	                        WHERE id=? AND active=1`,
		t.Name, t.Type, t.Host, t.Port, t.Pace, t.IntervalSec, t.ID)
	var se sqlite3.Error
	if errors.As(err, &se) && se.ExtendedCode == sqlite3.ErrConstraintUnique {
		var active bool
		s.db.QueryRow(`SELECT active FROM targets WHERE name=?`, t.Name).Scan(&active)
		if active {
			return t, errNameTaken
		}
		return t, errNameDeleted
	}
	if err != nil {
		return t, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return t, errNotFound
	}
	return t, nil
}

// DeactivateTarget stops probing; rounds stay until retention removes them.
func (s *Store) DeactivateTarget(id int64) error {
	res, err := s.db.Exec(`UPDATE targets SET active=0 WHERE id=? AND active=1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}
