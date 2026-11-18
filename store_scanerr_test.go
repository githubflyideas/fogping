package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// A row the store cannot read must leave a trace in the log. It used to end the
// series silently, so the chart showed fewer points and nothing said why.
func TestReadErrorsAreLogged(t *testing.T) {
	s, err := NewStore(t.TempDir(), []TargetCfg{{Name: "bad", Type: "tcp", Host: "127.0.0.1", Port: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, ok := s.targetID("bad")
	if !ok {
		t.Fatal("target not registered")
	}
	now := time.Now().Unix()
	// SQLite is loosely typed: text in an INTEGER column is stored as text, and
	// scanning it into an int fails.
	if _, err := s.db.Exec(`INSERT INTO rounds(target_id,t,sent,recv,samples) VALUES(?,?,?,?,?)`,
		id, now, "not a number", 1, []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO rounds_hourly(target_id,t,lo,p50,p90,p99,hi,loss_pct,bursts,n) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, now-bucketSec, 1, 1, 1, 1, 1, "not a number", 0, 1); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	if got := s.queryRaw(context.Background(), "bad", now-10, now+10); len(got) != 0 {
		t.Errorf("queryRaw returned %d rows for an unreadable one", len(got))
	}
	if got := s.queryHourly(context.Background(), "bad", now-2*bucketSec, now+10); len(got) != 0 {
		t.Errorf("queryHourly returned %d rows for an unreadable one", len(got))
	}
	out := buf.String()
	for _, want := range []string{"scan raw", "scan hourly"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// A client that disconnected mid-read is not a fault: no log line for it.
func TestCancelledReadIsNotLogged(t *testing.T) {
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	ctx, cancel := context.WithCancel(context.Background())
	rows, err := s.db.QueryContext(ctx, `SELECT t FROM rounds`)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for rows.Next() {
	}
	logRowsErr(ctx, "read raw", rows)
	rows.Close()
	if buf.Len() != 0 {
		t.Errorf("a cancelled read was logged: %s", buf.String())
	}
}
