package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginThrottleLocksAfterFreeFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginThrottle()
	l.now = func() time.Time { return now }
	const k = "192.0.2.7"

	for i := 1; i <= loginFreeFails; i++ {
		if _, ok := l.begin(k); !ok {
			t.Fatalf("attempt %d refused within the free failures", i)
		}
		if d := l.fail(k); d != 0 {
			t.Fatalf("failure %d started a %v lock, want none yet", i, d)
		}
	}
	if _, ok := l.begin(k); !ok {
		t.Fatal("attempt after the free failures refused before it was judged")
	}
	if d := l.fail(k); d != loginBaseLock {
		t.Fatalf("first lock = %v, want %v", d, loginBaseLock)
	}
	// Locked: refused before the password is even looked at.
	if d, ok := l.begin(k); ok || d != loginBaseLock {
		t.Fatalf("begin while locked = %v,%v, want %v,false", d, ok, loginBaseLock)
	}
	// Another address is unaffected.
	if _, ok := l.begin("192.0.2.8"); !ok {
		t.Fatal("a different address was locked out too")
	}

	// The lock doubles per further failure and is capped.
	want := loginBaseLock
	for i := 0; i < 10; i++ {
		now = now.Add(loginMaxLock + time.Second)
		if _, ok := l.begin(k); !ok {
			t.Fatalf("round %d: still locked after the lock expired", i)
		}
		want *= 2
		if want > loginMaxLock {
			want = loginMaxLock
		}
		if d := l.fail(k); d != want {
			t.Fatalf("round %d: lock %v, want %v", i, d, want)
		}
	}

	// A success after the lock expires clears the record.
	now = now.Add(loginMaxLock)
	if _, ok := l.begin(k); !ok {
		t.Fatal("refused after the lock expired")
	}
	l.success(k)
	if _, ok := l.m[k]; ok {
		t.Fatal("success did not clear the record")
	}

	// An address that stays quiet for loginForget starts over.
	for i := 0; i < loginFreeFails; i++ {
		l.begin(k)
		l.fail(k)
	}
	now = now.Add(loginForget + time.Second)
	l.begin(k)
	if d := l.fail(k); d != 0 {
		t.Fatalf("after a quiet hour the first failure locked for %v", d)
	}
}

func TestLoginThrottleOneAttemptAtATime(t *testing.T) {
	l := newLoginThrottle()
	if _, ok := l.begin("a"); !ok {
		t.Fatal("first attempt refused")
	}
	if _, ok := l.begin("a"); ok {
		t.Fatal("a parallel attempt from the same address was let through")
	}
	l.release("a")
	if _, ok := l.begin("a"); !ok {
		t.Fatal("release did not free the address")
	}
}

func TestLoginThrottlePruneKeepsLocks(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginThrottle()
	l.now = func() time.Time { return now }
	for i := 0; i <= loginFreeFails; i++ {
		l.begin("locked")
		l.fail("locked")
	}
	for i := 0; len(l.m) < loginMaxKeys; i++ {
		k := "x" + time.Duration(i).String()
		l.begin(k)
		l.release(k)
		l.m[k] = &loginRecord{fails: 1, last: now}
	}
	l.begin("newcomer")
	if _, ok := l.m["locked"]; !ok {
		t.Fatal("pruning dropped a locked address")
	}
	if len(l.m) >= loginMaxKeys {
		t.Fatalf("table not pruned: %d entries", len(l.m))
	}
}

func TestLoginClientKey(t *testing.T) {
	for addr, want := range map[string]string{
		"192.0.2.1:5555":             "192.0.2.1",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64",
		"[2001:db8:1:2:ff::1]:80":    "2001:db8:1:2::/64",
		"[::ffff:192.0.2.1]:80":      "192.0.2.1",
	} {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.RemoteAddr = addr
		if got := loginClientKey(r); got != want {
			t.Errorf("%s -> %q, want %q", addr, got, want)
		}
	}
}
