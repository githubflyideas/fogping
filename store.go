package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Storage is SQLite with three tiers. Raw rounds keep every sample for recent days;
// hourly and daily rollups keep the shape (min/p50/p90/p99/max/loss/bursts) for much
// longer. A query picks the tier that fits the window, so any span — an hour or a
// year — comes back as at most a few thousand rows and the browser never chokes.
//
// Why not plain files anymore: reading 45 days of JSONL took ~40s and shipped
// millions of points to the browser. The tiering is the same idea SmokePing gets
// from RRD, expressed in SQL so the data stays inspectable and exportable.
const (
	ringCap    = 1440 // in-memory flight recorder, ~24h at 1/min
	hourlyFrom = 24 * time.Hour
	dailyFrom  = 30 * 24 * time.Hour
)

type Store struct {
	mu    sync.RWMutex
	db    *sql.DB
	dir   string
	ids   map[string]int64 // target name -> row id
	rings map[string][]Round
	names []string
	total uint64
	start time.Time

	wmu     sync.Mutex // serializes writes; SQLite takes one writer at a time
	pending []pendingRound
}

type pendingRound struct {
	id int64
	r  Round
}

func NewStore(dir string, targets []TargetCfg) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "fogping.db")
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	// WAL lets the prober write while a long query reads; without it every probe
	// would block on whatever chart someone has open.
	for _, pragma := range []string{
		// INCREMENTAL lets reclaim() hand pages back later; it must be set before any
		// table exists, which is why it leads this list.
		"PRAGMA auto_vacuum = INCREMENTAL",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -32000", // 32MB page cache
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	db.SetMaxOpenConns(4)

	schema := `
CREATE TABLE IF NOT EXISTS targets (
  id   INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE
);

-- Raw rounds. Samples are a packed float32 blob: 4 bytes each instead of ~6 chars
-- of JSON, and no parsing on read.
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

-- Rollups. Same columns at both resolutions so the query path is identical.
CREATE TABLE IF NOT EXISTS rounds_hourly (
  target_id INTEGER NOT NULL,
  t         INTEGER NOT NULL,
  lo REAL, p50 REAL, p90 REAL, p99 REAL, hi REAL,
  loss_pct REAL, bursts INTEGER, n INTEGER,
  PRIMARY KEY (target_id, t)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS rounds_daily (
  target_id INTEGER NOT NULL,
  t         INTEGER NOT NULL,
  lo REAL, p50 REAL, p90 REAL, p99 REAL, hi REAL,
  loss_pct REAL, bursts INTEGER, n INTEGER,
  PRIMARY KEY (target_id, t)
) WITHOUT ROWID;
`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	s := &Store{
		db:    db,
		dir:   dir,
		ids:   map[string]int64{},
		rings: map[string][]Round{},
		start: time.Now(),
	}
	for _, t := range targets {
		if err := s.EnsureTarget(t); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { s.Flush(); return s.db.Close() }

// EnsureTarget registers a target, creating its row if new.
func (s *Store) EnsureTarget(t TargetCfg) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[t.Name]; ok {
		return nil
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO targets(name) VALUES(?)`, t.Name); err != nil {
		return err
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM targets WHERE name = ?`, t.Name).Scan(&id); err != nil {
		return err
	}
	s.ids[t.Name] = id
	s.rings[t.Name] = nil
	s.names = append(s.names, t.Name)
	return nil
}

// RemoveTarget drops it from the live set; stored rows are kept.
func (s *Store) RemoveTarget(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rings, name)
	delete(s.ids, name)
	for i, n := range s.names {
		if n == name {
			s.names = append(s.names[:i], s.names[i+1:]...)
			break
		}
	}
}

func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.names...)
}

func (s *Store) Counters() (uint64, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.total, s.start
}

// packSamples stores RTTs as float32 — 0.01ms resolution is far beyond what any
// network measurement means, and it halves the blob.
func packSamples(ms []float64) []byte {
	b := make([]byte, 4*len(ms))
	for i, v := range ms {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(float32(v)))
	}
	return b
}

func unpackSamples(b []byte) []float64 {
	out := make([]float64, len(b)/4)
	for i := range out {
		out[i] = round2(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))))
	}
	return out
}

// Append buffers the round and flushes in batches: one fsync per round would cap
// throughput far below what a few dozen targets need.
func (s *Store) Append(name string, r Round) error {
	s.mu.Lock()
	id, ok := s.ids[name]
	if ok {
		ring := append(s.rings[name], r)
		if len(ring) > ringCap {
			ring = append([]Round(nil), ring[len(ring)-ringCap:]...)
		}
		s.rings[name] = ring
		s.total++
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}

	s.wmu.Lock()
	s.pending = append(s.pending, pendingRound{id: id, r: r})
	n := len(s.pending)
	s.wmu.Unlock()
	// Batch when rounds arrive in bursts, but never sit on data: with a handful of
	// targets a size-only threshold would hold writes for minutes, losing them on a
	// crash and leaving the chart empty right after startup.
	if n >= 16 {
		return s.Flush()
	}
	return nil
}

// flushLoop commits whatever is buffered every couple of seconds.
func (s *Store) flushLoop(stop <-chan struct{}) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			s.Flush()
			return
		case <-tick.C:
			if err := s.Flush(); err != nil {
				log.Printf("flush: %v", err)
			}
		}
	}
}

// Flush writes buffered rounds in one transaction.
func (s *Store) Flush() error {
	s.wmu.Lock()
	batch := s.pending
	s.pending = nil
	s.wmu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO rounds(target_id,t,sent,recv,samples,burst,z)
	                          VALUES(?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, p := range batch {
		b := 0
		if p.r.B {
			b = 1
		}
		if _, err := stmt.Exec(p.id, p.r.T, p.r.S, p.r.R, packSamples(p.r.MS), b, p.r.Z); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

// Replay rebuilds the in-memory ring from the last 24h on startup.
func (s *Store) Replay() {
	cut := time.Now().Add(-24 * time.Hour).Unix()
	for _, name := range s.Names() {
		rounds := s.queryRaw(context.Background(), name, cut, time.Now().Unix())
		if len(rounds) > ringCap {
			rounds = rounds[len(rounds)-ringCap:]
		}
		s.mu.Lock()
		s.rings[name] = rounds
		s.mu.Unlock()
		if len(rounds) > 0 {
			log.Printf("[%s] replayed %d rounds", name, len(rounds))
		}
	}
}

// Recent serves the flight recorder — no disk, no SQL.
func (s *Store) Recent(name string, since int64) []Round {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ring := s.rings[name]
	i := sort.Search(len(ring), func(i int) bool { return ring[i].T >= since })
	return append([]Round(nil), ring[i:]...)
}

// ReadRange picks a tier by window width. Under a day: raw samples, real smoke.
// Up to a month: hourly rollups. Beyond: daily. Every path returns []Round, so
// callers and the front end never learn which tier answered.
func (s *Store) ReadRange(ctx context.Context, name string, from, to int64) []Round {
	span := time.Duration(to-from) * time.Second
	switch {
	case span <= hourlyFrom:
		return s.queryRaw(ctx, name, from, to)
	case span <= dailyFrom:
		return s.queryRollup(ctx, "rounds_hourly", name, from, to)
	default:
		return s.queryRollup(ctx, "rounds_daily", name, from, to)
	}
}

func (s *Store) targetID(name string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.ids[name]
	return id, ok
}

func (s *Store) queryRaw(ctx context.Context, name string, from, to int64) []Round {
	id, ok := s.targetID(name)
	if !ok {
		return []Round{}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t,sent,recv,samples,burst,z FROM rounds
		  WHERE target_id=? AND t BETWEEN ? AND ? ORDER BY t`, id, from, to)
	if err != nil {
		log.Printf("query raw: %v", err)
		return []Round{}
	}
	defer rows.Close()
	out := []Round{}
	for rows.Next() {
		var r Round
		var blob []byte
		var b int
		if err := rows.Scan(&r.T, &r.S, &r.R, &blob, &b, &r.Z); err != nil {
			break
		}
		r.MS = unpackSamples(blob)
		r.B = b != 0
		out = append(out, r)
	}
	return out
}

// queryRollup reshapes an aggregate row into a Round whose MS carries the five
// shape values. The chart draws them exactly like raw samples — the smoke just
// gets sparser the further back you look, which is what SmokePing does too.
func (s *Store) queryRollup(ctx context.Context, table, name string, from, to int64) []Round {
	id, ok := s.targetID(name)
	if !ok {
		return []Round{}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t,lo,p50,p90,p99,hi,loss_pct,bursts,n FROM `+table+`
		  WHERE target_id=? AND t BETWEEN ? AND ? ORDER BY t`, id, from, to)
	if err != nil {
		log.Printf("query %s: %v", table, err)
		return []Round{}
	}
	defer rows.Close()
	out := []Round{}
	for rows.Next() {
		var t int64
		var lo, p50, p90, p99, hi, loss float64
		var bursts, n int
		if err := rows.Scan(&t, &lo, &p50, &p90, &p99, &hi, &loss, &bursts, &n); err != nil {
			break
		}
		sent := n
		if sent == 0 {
			sent = 1
		}
		out = append(out, Round{
			T: t, S: sent, R: sent - int(float64(sent)*loss/100+0.5),
			MS: []float64{lo, p50, p90, p99, hi},
			B:  bursts > 0,
		})
	}
	return out
}

// Rollup recomputes aggregates for a period. Cheap enough to just redo the recent
// window on every run rather than track what changed.
func (s *Store) Rollup(since int64) error {
	for _, spec := range []struct{ table, unit string }{
		{"rounds_hourly", "3600"},
		{"rounds_daily", "86400"},
	} {
		q := `INSERT OR REPLACE INTO ` + spec.table + `
	(target_id,t,lo,p50,p90,p99,hi,loss_pct,bursts,n)
	SELECT target_id, (t/` + spec.unit + `)*` + spec.unit + `,
	       0,0,0,0,0,
	       100.0*(SUM(sent)-SUM(recv))/MAX(SUM(sent),1),
	       SUM(burst), COUNT(*)
	  FROM rounds WHERE t >= ?
	 GROUP BY target_id, t/` + spec.unit
		if _, err := s.db.Exec(q, since); err != nil {
			return err
		}
	}
	return s.rollupPercentiles(since)
}

// rollupPercentiles fills the shape columns. Percentiles need the samples
// themselves, so this pass reads raw rounds per bucket — still far cheaper than
// doing it at query time, and it happens once an hour.
func (s *Store) rollupPercentiles(since int64) error {
	for _, spec := range []struct {
		table string
		unit  int64
	}{
		{"rounds_hourly", 3600},
		{"rounds_daily", 86400},
	} {
		rows, err := s.db.Query(
			`SELECT target_id, (t/?)*?, samples FROM rounds WHERE t >= ? ORDER BY target_id, t`,
			spec.unit, spec.unit, since)
		if err != nil {
			return err
		}
		type key struct {
			id int64
			b  int64
		}
		buckets := map[key][]float64{}
		for rows.Next() {
			var id, bucket int64
			var blob []byte
			if err := rows.Scan(&id, &bucket, &blob); err != nil {
				break
			}
			buckets[key{id, bucket}] = append(buckets[key{id, bucket}], unpackSamples(blob)...)
		}
		rows.Close()

		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		stmt, err := tx.Prepare(`UPDATE ` + spec.table + `
		    SET lo=?,p50=?,p90=?,p99=?,hi=? WHERE target_id=? AND t=?`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for k, vals := range buckets {
			if len(vals) == 0 {
				continue
			}
			sort.Float64s(vals)
			q := func(p float64) float64 {
				i := int(float64(len(vals)-1) * p)
				return round2(vals[i])
			}
			if _, err := stmt.Exec(q(0), q(0.5), q(0.9), q(0.99), q(1), k.id, k.b); err != nil {
				stmt.Close()
				tx.Rollback()
				return err
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Retention deletes raw rounds past rawDays and rollups past keepDays. Rollups are
// tiny, so they can outlive the raw data by a long way.
func (s *Store) Retention(rawDays, keepDays int) {
	if rawDays > 0 {
		cut := time.Now().AddDate(0, 0, -rawDays).Unix()
		if res, err := s.db.Exec(`DELETE FROM rounds WHERE t < ?`, cut); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				log.Printf("retention: dropped %d raw rounds older than %dd", n, rawDays)
			}
		}
	}
	if keepDays > 0 {
		cut := time.Now().AddDate(0, 0, -keepDays).Unix()
		s.db.Exec(`DELETE FROM rounds_hourly WHERE t < ?`, cut)
		s.db.Exec(`DELETE FROM rounds_daily WHERE t < ?`, cut)
	}
	s.reclaim()
}

// reclaim returns freed pages to the filesystem. SQLite marks deleted space reusable
// but keeps the file at its high-water mark, so a box that once held 300 days would
// never shrink after switching to a shorter window — surprising on the small hosts
// this build targets. incremental_vacuum does it in bounded steps, unlike a full
// VACUUM which rewrites the whole database and needs twice the space.
func (s *Store) reclaim() {
	// SQLite keeps deleted space inside the file: mark it reusable, never give it
	// back. On a host that ran with a long window and then switched to a short one
	// the file would stay at its high-water mark forever, which is a bad surprise on
	// the small boxes this build is meant for.
	//
	// incremental_vacuum only frees a handful of pages per transaction in WAL mode,
	// so it never catches up. A full VACUUM rebuilds the file and returns everything;
	// at a few MB that costs well under a second, and it runs once a night.
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("reclaim checkpoint: %v", err)
	}
	var freelist, pageSize int
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelist); err != nil {
		return
	}
	s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	if freelist*pageSize < 4<<20 { // under 4MB of slack: leave it alone
		return
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		log.Printf("reclaim vacuum: %v", err)
		return
	}
	// VACUUM rebuilds through the WAL; without a second checkpoint the main file
	// keeps its old size and nothing looks reclaimed.
	s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	log.Printf("reclaim: vacuumed, %d free pages returned to the filesystem", freelist)
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

type Stats struct {
	Rounds  int     `json:"rounds"`
	P50     float64 `json:"p50"`
	P90     float64 `json:"p90"`
	P99     float64 `json:"p99"`
	LossPct float64 `json:"loss_pct"`
	Bursts  int     `json:"bursts"`
}

func calcStats(rounds []Round) Stats {
	st := Stats{Rounds: len(rounds)}
	var pool []float64
	sent, recv := 0, 0
	for _, r := range rounds {
		sent += r.S
		recv += r.R
		pool = append(pool, r.MS...)
		if r.B {
			st.Bursts++
		}
	}
	if sent > 0 {
		st.LossPct = 100 * float64(sent-recv) / float64(sent)
	}
	if len(pool) > 0 {
		sort.Float64s(pool)
		st.P50 = pct(pool, 50)
		st.P90 = pct(pool, 90)
		st.P99 = pct(pool, 99)
	}
	return st
}

// pct 要求已排序。
func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	return sorted[idx]
}

// Retention:保留期就是 rm。按天分文件让清理不需要任何压缩整理逻辑。
// downsampleRound collapses a round's samples to [min, median, P90, max].
// The round itself is preserved — same timestamp, same sent/recv counts, same burst
// flag and z-score — so cold data flows through the exact same code path as hot data.
// Only the within-round redundancy is dropped: on a month-wide axis those 20-30
