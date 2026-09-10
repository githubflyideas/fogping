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

// Storage is SQLite with two tiers. Raw rounds keep every sample for HotDays; the
// hourly rollup keeps the shape (min/p50/p90/p99/max, loss, bursts) for the whole
// retention window. Windows up to a day read raw rounds (real smoke); anything
// longer reads hourly buckets — 300 days is 7200 rows, which the chart handles fine.
//
// There used to be a daily tier as well. It was kept exactly as long as the hourly
// one, so it saved no disk, and deriving it from an unaligned window truncated its
// buckets; it is dropped on startup (see NewStore).
const (
	ringCap    = 1440 // in-memory flight recorder, ~24h at 1/min
	hourlyFrom = 24 * time.Hour
	bucketSec  = 3600
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

`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrateTargets(db); err != nil {
		return nil, err
	}
	var hadDaily int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='rounds_daily'`).Scan(&hadDaily)
	if hadDaily > 0 {
		if _, err := db.Exec(`DROP TABLE rounds_daily`); err != nil {
			return nil, err
		}
		log.Printf("storage: dropped the daily rollup table (hourly covers the same span)")
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

// EnsureTarget registers a target in the live set (creating a bare row if the name
// is unknown, which only tests rely on) and replays its last 24h into the ring, so
// a target re-added with an old name comes back with its flight recorder intact.
func (s *Store) EnsureTarget(t TargetCfg) error {
	s.mu.RLock()
	_, ok := s.ids[t.Name]
	s.mu.RUnlock()
	if ok {
		return nil
	}
	var id int64
	err := s.db.QueryRow(`SELECT id FROM targets WHERE name = ?`, t.Name).Scan(&id)
	if err == sql.ErrNoRows {
		var res sql.Result
		if res, err = s.db.Exec(`INSERT INTO targets(name) VALUES(?)`, t.Name); err == nil {
			id, err = res.LastInsertId()
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ids[t.Name] = id
	s.rings[t.Name] = nil
	s.names = append(s.names, t.Name)
	s.mu.Unlock()
	s.replay(t.Name)
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

// RenameTarget moves the live state to a new name. History follows automatically:
// rows are keyed by target_id, and the id doesn't change on rename.
func (s *Store) RenameTarget(old, new string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.ids[old]
	if !ok {
		return
	}
	s.ids[new], s.rings[new] = id, s.rings[old]
	delete(s.ids, old)
	delete(s.rings, old)
	for i, n := range s.names {
		if n == old {
			s.names[i] = new
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
			return // main calls Close, which does the final flush
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

// replay rebuilds one target's in-memory ring from its last 24h.
func (s *Store) replay(name string) {
	rounds := s.queryRaw(context.Background(), name, time.Now().Add(-24*time.Hour).Unix(), time.Now().Unix())
	if len(rounds) > ringCap {
		rounds = rounds[len(rounds)-ringCap:]
	}
	s.mu.Lock()
	if _, ok := s.ids[name]; ok {
		s.rings[name] = rounds
	}
	s.mu.Unlock()
	if len(rounds) > 0 {
		log.Printf("[%s] replayed %d rounds", name, len(rounds))
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

// Series is what /api/series returns. Raw rounds and hourly buckets are different
// things — a bucket's five numbers are percentiles, not samples — so they travel in
// different fields and the front end draws each honestly.
type Series struct {
	Tier    string   `json:"tier"` // "raw" | "hourly"
	Rounds  []Round  `json:"rounds,omitempty"`
	Buckets []Bucket `json:"buckets,omitempty"`
}

type Bucket struct {
	T      int64     `json:"t"`
	Q      []float64 `json:"q,omitempty"` // min,p50,p90,p99,max — absent when every packet was lost
	Loss   float64   `json:"loss"`        // percent of packets, not rounds
	N      int       `json:"n"`           // rounds in the bucket
	Bursts int       `json:"bursts,omitempty"`
}

// ReadRange: up to a day, raw rounds; beyond, hourly buckets.
func (s *Store) ReadRange(ctx context.Context, name string, from, to int64) Series {
	if time.Duration(to-from)*time.Second <= hourlyFrom {
		return Series{Tier: "raw", Rounds: s.queryRaw(ctx, name, from, to)}
	}
	return Series{Tier: "hourly", Buckets: s.queryHourly(ctx, name, from, to)}
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

func (s *Store) queryHourly(ctx context.Context, name string, from, to int64) []Bucket {
	id, ok := s.targetID(name)
	if !ok {
		return []Bucket{}
	}
	// A bucket is stamped with its start, so widen by one bucket to keep the one
	// that straddles `from`.
	rows, err := s.db.QueryContext(ctx,
		`SELECT t,lo,p50,p90,p99,hi,loss_pct,bursts,n FROM rounds_hourly
		  WHERE target_id=? AND t > ? AND t <= ? ORDER BY t`, id, from-bucketSec, to)
	if err != nil {
		log.Printf("query hourly: %v", err)
		return []Bucket{}
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		var q [5]sql.NullFloat64
		if err := rows.Scan(&b.T, &q[0], &q[1], &q[2], &q[3], &q[4], &b.Loss, &b.Bursts, &b.N); err != nil {
			log.Printf("scan hourly: %v", err)
			break
		}
		if q[0].Valid {
			b.Q = []float64{q[0].Float64, q[1].Float64, q[2].Float64, q[3].Float64, q[4].Float64}
		}
		out = append(out, b)
	}
	return out
}

// Rollup recomputes every hourly bucket from `since` onward, in one streaming pass
// and one transaction (readers never see a half-written bucket).
//
// The one rule that matters: a bucket may only be recomputed while raw still holds
// all of it. `since` is aligned down to a bucket boundary — never recompute half a
// bucket — and anything starting before `horizon` (the oldest instant raw is
// guaranteed to hold) is left alone, because retention may already have deleted its
// first minutes and a recompute would overwrite the good value with a truncated one.
func (s *Store) Rollup(since, horizon int64) error {
	start := since / bucketSec * bucketSec
	if start < horizon {
		start = (horizon + bucketSec - 1) / bucketSec * bucketSec
	}
	rows, err := s.db.Query(`SELECT target_id, t, sent, recv, samples, burst FROM rounds
	                          WHERE t >= ? ORDER BY target_id, t`, start)
	if err != nil {
		return err
	}
	type agg struct {
		id, t                int64
		sent, recv, n, burst int
		q                    []any // 5 values, or 5 nils
	}
	var out []agg
	var cur agg
	var vals []float64 // reused: only one bucket's samples are ever held in memory
	open := false
	closeBucket := func() {
		if !open {
			return
		}
		cur.q = []any{nil, nil, nil, nil, nil}
		if len(vals) > 0 {
			sort.Float64s(vals)
			for i, p := range []float64{0, .5, .9, .99, 1} {
				cur.q[i] = round2(vals[int(float64(len(vals)-1)*p)])
			}
		}
		out = append(out, cur)
	}
	for rows.Next() {
		var id, t int64
		var sent, recv, burst int
		var blob []byte
		if err := rows.Scan(&id, &t, &sent, &recv, &blob, &burst); err != nil {
			rows.Close()
			return err
		}
		if b := t / bucketSec * bucketSec; !open || id != cur.id || b != cur.t {
			closeBucket()
			cur, vals, open = agg{id: id, t: b}, vals[:0], true
		}
		cur.sent += sent
		cur.recv += recv
		cur.burst += burst
		cur.n++
		vals = append(vals, unpackSamples(blob)...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	closeBucket()
	if len(out) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO rounds_hourly
	    (target_id,t,lo,p50,p90,p99,hi,loss_pct,bursts,n) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, a := range out {
		loss := 0.0
		if a.sent > 0 {
			loss = 100 * float64(a.sent-a.recv) / float64(a.sent)
		}
		if _, err := stmt.Exec(a.id, a.t, a.q[0], a.q[1], a.q[2], a.q[3], a.q[4], loss, a.burst, a.n); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

// rollupSched decides what Rollup covers on each housekeeping tick: everything raw
// still holds on the first tick (so a fresh or restarted instance never shows an
// empty 3d/7d chart), then the last two hours every five minutes.
type rollupSched struct {
	hot  time.Duration
	last time.Time
}

func (r *rollupSched) tick(s *Store, now time.Time) {
	horizon := now.Add(-r.hot).Unix()
	since := now.Add(-2 * time.Hour).Unix()
	switch {
	case r.last.IsZero():
		since = horizon
	case now.Sub(r.last) < 5*time.Minute:
		return
	}
	if err := s.Rollup(since, horizon); err != nil {
		log.Printf("rollup: %v", err)
		return
	}
	r.last = now
}

// Retention deletes raw rounds past rawDays and hourly buckets past keepDays.
func (s *Store) Retention(now time.Time, rawDays, keepDays int) {
	if rawDays > 0 {
		cut := now.AddDate(0, 0, -rawDays).Unix()
		if res, err := s.db.Exec(`DELETE FROM rounds WHERE t < ?`, cut); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				log.Printf("retention: dropped %d raw rounds older than %dd", n, rawDays)
			}
		}
	}
	if keepDays > 0 {
		s.db.Exec(`DELETE FROM rounds_hourly WHERE t < ?`, now.AddDate(0, 0, -keepDays).Unix())
	}
	s.reclaim()
}

// reclaim returns freed pages to the filesystem. SQLite keeps deleted space inside
// the file, so a host that once ran with a long window would stay at its high-water
// mark forever. incremental_vacuum barely moves in WAL mode, so this does a full
// VACUUM when there is real slack — a few MB, under a second, once a night.
func (s *Store) reclaim() {
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
