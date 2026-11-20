package main

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// loginThrottle locks a client out of /api/login after repeated wrong passwords.
//
// It is keyed by the client's address (RemoteAddr — never X-Forwarded-For, which
// the client writes itself), not by account name, so an attacker can lock out only
// their own address and never the operator. IPv6 clients are keyed by their /64,
// because one host usually holds the whole prefix and could otherwise rotate
// through it.
//
// The first loginFreeFails wrong passwords cost only the fixed one-second delay.
// Each one after that locks the address for loginBaseLock, doubling per further
// failure up to loginMaxLock. While the lock holds, even the right password is
// refused with 429: a lock that let the right guess through would only slow the
// guessing down, not stop it from landing. A correct login clears the record, and
// an address that stays quiet for loginForget starts over.
//
// Only one attempt per address is evaluated at a time. Without that, a client could
// fire a thousand guesses in parallel and every one would pass the lock check
// before the first failure was counted.
const (
	loginFreeFails = 5
	loginBaseLock  = 30 * time.Second
	loginMaxLock   = 15 * time.Minute
	loginForget    = time.Hour
	loginMaxKeys   = 4096 // tracked addresses before idle ones are pruned
)

type loginThrottle struct {
	mu  sync.Mutex
	m   map[string]*loginRecord
	now func() time.Time // tests move the clock
}

type loginRecord struct {
	fails int
	until time.Time // locked until
	last  time.Time // last activity
	busy  bool      // an attempt from this address is being evaluated
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{m: map[string]*loginRecord{}, now: time.Now}
}

// loginClientKey is the address a login attempt is charged to.
func loginClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return host
	case ip.To4() != nil:
		return ip.To4().String() // also folds ::ffff:a.b.c.d into a.b.c.d
	default:
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
}

// begin reserves the address for one attempt. When it returns false the caller
// must refuse the request (429) and tell the client to wait the returned time.
func (l *loginThrottle) begin(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rec, ok := l.m[key]
	if ok && !rec.busy && now.Sub(rec.last) > loginForget && !now.Before(rec.until) {
		delete(l.m, key)
		ok = false
	}
	if !ok {
		if len(l.m) >= loginMaxKeys {
			l.prune(now)
		}
		rec = &loginRecord{}
		l.m[key] = rec
	}
	if d := rec.until.Sub(now); d > 0 {
		return d, false
	}
	if rec.busy {
		return time.Second, false
	}
	rec.busy = true
	rec.last = now
	return 0, true
}

// fail ends an attempt with a wrong password and returns the lock it started,
// or 0 while the address is still within its free failures.
func (l *loginThrottle) fail(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rec, ok := l.m[key]
	if !ok {
		rec = &loginRecord{}
		l.m[key] = rec
	}
	rec.busy = false
	rec.fails++
	rec.last = now
	over := rec.fails - loginFreeFails
	if over <= 0 {
		return 0
	}
	d := loginBaseLock
	for i := 1; i < over && d < loginMaxLock; i++ {
		d *= 2
	}
	if d > loginMaxLock {
		d = loginMaxLock
	}
	rec.until = now.Add(d)
	return d
}

// success ends an attempt with the right password and forgets the address.
func (l *loginThrottle) success(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

// release ends an attempt that was never judged (a malformed request), without
// counting it either way.
func (l *loginThrottle) release(key string) {
	l.mu.Lock()
	if rec, ok := l.m[key]; ok {
		rec.busy = false
		if rec.fails == 0 {
			delete(l.m, key)
		}
	}
	l.mu.Unlock()
}

// prune drops idle addresses, then, if the table is still full, every address
// that is neither locked nor mid-attempt. Locked addresses are never dropped, so
// flooding the table cannot be used to clear a lock. Called with l.mu held.
func (l *loginThrottle) prune(now time.Time) {
	for k, rec := range l.m {
		if !rec.busy && now.Sub(rec.last) > loginForget && !now.Before(rec.until) {
			delete(l.m, k)
		}
	}
	if len(l.m) < loginMaxKeys {
		return
	}
	for k, rec := range l.m {
		if !rec.busy && !now.Before(rec.until) {
			delete(l.m, k)
		}
	}
}

// retryAfter is the Retry-After header value for a wait, rounded up to whole
// seconds.
func retryAfter(d time.Duration) string {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}
