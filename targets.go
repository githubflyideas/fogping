package main

import (
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

func (s *Store) ActiveTargets() ([]TargetCfg, error) {
	rows, err := s.db.Query(`SELECT id,name,type,host,port,pace,interval_sec,options
	                           FROM targets WHERE active=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TargetCfg
	for rows.Next() {
		var t TargetCfg
		var opts string
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Host, &t.Port, &t.Pace, &t.IntervalSec, &opts); err != nil {
			return nil, err
		}
		t.Options = decodeOptions(opts)
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

const upsertTarget = `INSERT INTO targets(name,type,host,port,pace,interval_sec,options,active)
VALUES(?,?,?,?,?,?,?,1)
ON CONFLICT(name) DO UPDATE SET type=excluded.type, host=excluded.host, port=excluded.port,
  pace=excluded.pace, interval_sec=excluded.interval_sec, options=excluded.options, active=1`

// CreateTarget adds a target, or reactivates a deleted one of the same name. It
// refuses to overwrite an active target: that's what UpdateTarget is for.
func (s *Store) CreateTarget(t TargetCfg) (TargetCfg, error) {
	res, err := s.db.Exec(upsertTarget+` WHERE targets.active=0`,
		t.Name, t.Type, t.Host, t.Port, t.Pace, t.IntervalSec, encodeOptions(t.Options))
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
	res, err := s.db.Exec(`UPDATE targets SET name=?,type=?,host=?,port=?,pace=?,interval_sec=?,options=?
	                        WHERE id=? AND active=1`,
		t.Name, t.Type, t.Host, t.Port, t.Pace, t.IntervalSec, encodeOptions(t.Options), t.ID)
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

// TargetNames maps every known name to whether it is active. Import previews use
// it to tell "already there" from "deleted, will be restored".
func (s *Store) TargetNames() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name, active FROM targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var active bool
		if err := rows.Scan(&name, &active); err != nil {
			return nil, err
		}
		out[name] = active
	}
	return out, rows.Err()
}

// ImportTargets adds a batch in one transaction, with exactly CreateTarget's rule
// per row: a new name is inserted, a deleted name is reactivated (history comes
// back), an active name is refused. Any refusal or error rolls the whole batch
// back, so an import is never half-applied.
func (s *Store) ImportTargets(ts []TargetCfg) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	for _, t := range ts {
		res, err := tx.Exec(upsertTarget+` WHERE targets.active=0`,
			t.Name, t.Type, t.Host, t.Port, t.Pace, t.IntervalSec, encodeOptions(t.Options))
		if err != nil {
			return fmt.Errorf("%q: %w", t.Name, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%q: %w", t.Name, errNameTaken)
		}
	}
	return tx.Commit()
}
