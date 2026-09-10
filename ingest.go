package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// targets/ is an inbox, not a config. A ping.list or tcp.list dropped there is
// imported into SQLite (upsert by name) and moved aside, so the database stays the
// only source of truth and `echo "1.2.3.4 name" >> targets/ping.list` keeps working
// for scripts and first-time setup. It also migrates pre-SQLite-targets installs.
//
//	ok  -> contents appended to <file>.imported, original removed
//	bad -> renamed to <file>.rejected, error logged with the line number
//
// Filesystem access already implies more trust than the web UI, so the inbox works
// with or without --edit.
func ingestLists(dir string, store *Store) (int, error) {
	total := 0
	for _, spec := range []struct{ file, typ string }{{"ping.list", "icmp"}, {"tcp.list", "tcp"}} {
		path := filepath.Join(dir, spec.file)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		// Claim the file first so an `echo >>` racing with us lands in a fresh ping.list
		// that the next pass picks up, instead of being read half-way or deleted.
		work := path + ".ingesting"
		if err := os.Rename(path, work); err != nil {
			return total, err
		}
		ts, err := parseListFile(work, spec.typ)
		if err == nil && len(ts) > 0 {
			err = store.ImportTargets(ts)
		}
		if err != nil {
			os.Rename(work, path+".rejected")
			return total, fmt.Errorf("%v — file moved to %s.rejected; fix it and move it back", err, path)
		}
		if err := appendAndRemove(work, path+".imported"); err != nil {
			log.Printf("ingest: archive %s: %v", path, err)
		}
		if len(ts) > 0 {
			log.Printf("ingest: %d target(s) from %s imported into the database (archived as %s.imported)",
				len(ts), path, spec.file)
		}
		total += len(ts)
	}
	return total, nil
}

func appendAndRemove(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	stamp := fmt.Sprintf("# --- imported %s ---\n", time.Now().Format(time.RFC3339))
	if _, err := f.WriteString(stamp + string(b) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// ingestLoop polls the inbox every 3s — the same cadence the list reload had.
func ingestLoop(dir string, store *Store, run *Runner, stop <-chan struct{}) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		n, err := ingestLists(dir, store)
		if err != nil {
			log.Printf("ingest: %v", err)
		}
		if n > 0 {
			if err := run.Reload(); err != nil {
				log.Printf("reload: %v", err)
			}
		}
	}
}
