package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

// loginFailDelay is paid by every wrong password before the answer goes out.
// Tests shorten it.
var loginFailDelay = time.Second

// sessions: in-memory, so a restart logs everyone out — acceptable and simple
// for a single-binary tool. Auth exists only when users are passed on the CLI.
type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (s *sessions) issue() string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[tok] = time.Now().Add(7 * 24 * time.Hour)
	s.mu.Unlock()
	return tok
}

func (s *sessions) valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok || time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	return true
}

// revoke ends a session server-side, so a copied cookie stops working at logout
// rather than when it expires a week later.
func (s *sessions) revoke(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

func serveWeb(cfg *Config, store *Store, run *Runner, users map[string]string) error {
	return http.ListenAndServe(cfg.Listen, newMux(cfg, store, run, users))
}

// webApp holds what the HTTP handlers share. newMux only wires routes; each
// area of the API registers its own, so no single function grows past a
// screenful.
type webApp struct {
	cfg      *Config
	store    *Store
	run      *Runner
	users    map[string]string
	sess     *sessions
	throttle *loginThrottle
}

func newMux(cfg *Config, store *Store, run *Runner, users map[string]string) http.Handler {
	return securityHeaders(apiRules(buildMux(cfg, store, run, users)))
}

// buildMux is newMux without the outer wrappers, so tests can ask the router
// which pattern serves a request.
func buildMux(cfg *Config, store *Store, run *Runner, users map[string]string) *http.ServeMux {
	a := &webApp{cfg: cfg, store: store, run: run, users: users,
		sess: &sessions{m: map[string]time.Time{}}, throttle: newLoginThrottle()}
	mux := http.NewServeMux()
	a.registerPages(mux)
	a.registerAuth(mux)
	a.registerInfo(mux)
	a.registerTargets(mux)
	mux.HandleFunc("GET /api/series", a.guard(a.handleSeries))
	registerStats(mux, cfg.StatsToken, store, run)
	return mux
}

func (a *webApp) authed(r *http.Request) bool {
	if len(a.users) == 0 {
		return true
	}
	c, err := r.Cookie("fogping_session")
	return err == nil && a.sess.valid(c.Value)
}

func (a *webApp) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			jsonErr(w, http.StatusUnauthorized, "sign in first")
			return
		}
		t0 := time.Now()
		h(w, r)
		if d := time.Since(t0); d > time.Second {
			log.Printf("slow request: %s %s took %v", r.URL.Path, r.URL.RawQuery, d)
		}
	}
}

// page serves one embedded HTML file with a CSP that allows its own inline
// scripts by hash.
func page(name string) http.HandlerFunc {
	b, _ := staticFS.ReadFile("static/" + name)
	csp := pageCSP(b)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Write(b)
	}
}

func (a *webApp) registerPages(mux *http.ServeMux) {
	index := page("index.html")
	// Pages are bound to GET like every endpoint, so the router itself can tell a
	// wrong method (405) from a wrong path (404) — a catch-all "/" would answer
	// every request it does not recognise with 404, API calls included.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		index(w, r)
	})
	mux.HandleFunc("GET /login", page("login.html"))
	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
}

func (a *webApp) registerAuth(mux *http.ServeMux) {
	// login: constant-time compare, 1s delay on failure, and a per-address lockout
	// after repeated failures (see loginthrottle.go).
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
}

func (a *webApp) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Before the throttle: this refuses a request that carries no credentials at
	// all, so it must not take the address's one login slot (begin) or count as
	// a guess.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		jsonErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	key := loginClientKey(r)
	if wait, ok := a.throttle.begin(key); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		jsonErr(w, http.StatusTooManyRequests, "too many failed logins, try again later")
		return
	}
	var body struct{ User, Pass string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.throttle.fail(key) // release the slot taken by begin; junk of this size is abuse, so it counts
			jsonErr(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		// Anything else unreadable falls through as an empty login and fails
		// like a wrong password, so the throttle counts it.
	}
	if !a.checkPassword(body.User, body.Pass) {
		a.loginFailed(w, r, key, body.User)
		return
	}
	a.throttle.success(key)
	http.SetCookie(w, &http.Cookie{Name: "fogping_session", Value: a.sess.issue(),
		Path: "/", HttpOnly: true, MaxAge: 7 * 24 * 3600, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *webApp) checkPassword(user, pass string) bool {
	want, ok := a.users[user]
	return len(a.users) != 0 && ok &&
		subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1
}

func (a *webApp) loginFailed(w http.ResponseWriter, r *http.Request, key, user string) {
	time.Sleep(loginFailDelay)
	if lock := a.throttle.fail(key); lock > 0 {
		log.Printf("web login failed for %q from %s; address locked for %v", user, r.RemoteAddr, lock)
	} else {
		log.Printf("web login failed for %q from %s", user, r.RemoteAddr)
	}
	jsonErr(w, http.StatusUnauthorized, "wrong username or password")
}

func (a *webApp) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("fogping_session"); err == nil {
		a.sess.revoke(c.Value) // otherwise the token stays valid until it expires
	}
	http.SetCookie(w, &http.Cookie{Name: "fogping_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *webApp) registerInfo(mux *http.ServeMux) {
	// version is public (it is in every release download anyway); the instance's
	// settings are not, once a login is configured.
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			writeJSON(w, map[string]any{"version": version})
			return
		}
		writeJSON(w, map[string]any{"version": version, "retention_days": a.cfg.RetentionDays,
			"hot_days": a.cfg.HotDays, // raw-sample days: the compare overlay asks for hourly rows beyond this
			"editable": a.cfg.Editable})
	})
	mux.HandleFunc("GET /api/probe-types", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, probeTypes())
	}))
}

// targetItem is one row of GET /api/targets.
type targetItem struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Host        string         `json:"host"`
	Port        int            `json:"port"`
	Addr        string         `json:"addr"`
	IntervalSec int            `json:"interval_sec"` // effective
	IntervalSet int            `json:"interval_set"` // explicit override, 0 = from pace
	Pace        string         `json:"pace"`
	Options     map[string]any `json:"options,omitempty"`
	Down        bool           `json:"down"`
	ProbeError  string         `json:"probe_error,omitempty"` // why the latest round could not be measured
	Last1h      Stats          `json:"last_1h"`
	Last24h     Stats          `json:"last_24h"`
}

func (a *webApp) handleTargets(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	out := []targetItem{}
	for _, t := range a.run.Targets() {
		out = append(out, a.targetItem(t, now))
	}
	writeJSON(w, out)
}

func (a *webApp) targetItem(t TargetCfg, now time.Time) targetItem {
	iv, _ := probeParams(t, a.cfg.Probe)
	pace := t.Pace
	if pace == "" {
		pace = "normal"
	}
	rec := a.store.Recent(t.Name, now.Add(-time.Hour).Unix())
	down, probeErr := latestState(rec)
	return targetItem{
		ID: t.ID, Name: t.Name, Type: t.Type, Host: t.Host, Port: t.Port, Addr: targetAddr(t),
		IntervalSec: int(iv.Seconds()), IntervalSet: t.IntervalSec, Pace: pace, Options: t.Options, Down: down, ProbeError: probeErr,
		Last1h:  calcStats(rec),
		Last24h: calcStats(a.store.Recent(t.Name, now.Add(-24*time.Hour).Unix())),
	}
}

// latestState: down means the last measured round got no reply. A round that
// could not be measured says nothing about the target; it is reported as
// probe_error.
func latestState(rec []Round) (down bool, probeErr string) {
	for i := len(rec) - 1; i >= 0; i-- {
		if rec[i].S == 0 {
			if i == len(rec)-1 {
				probeErr = rec[i].E
			}
			continue
		}
		return rec[i].R == 0, probeErr
	}
	return false, probeErr
}

// write gates every target change. Always routed so a read-only instance
// answers with a reason instead of a bare 405; every write goes DB first, then
// Runner.Reload.
func (a *webApp) write(h func(w http.ResponseWriter, r *http.Request, id int64)) http.HandlerFunc {
	return a.guard(func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.Editable {
			jsonErr(w, http.StatusForbidden, "read-only: restart fogping with --edit to change targets")
			return
		}
		if !sameOrigin(r) {
			jsonErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		h(w, r, id)
	})
}

// pathID reads the optional {id} path value; 0 when the route has none.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		jsonErr(w, http.StatusBadRequest, "bad id")
		return 0, false
	}
	return n, true
}

func decodeTarget(w http.ResponseWriter, r *http.Request) (TargetCfg, bool) {
	var t TargetCfg
	// JSON content type forces a CORS preflight on cross-site requests, which we
	// never answer — so a form on another site can't drive these endpoints.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		jsonErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return t, false
	}
	// Unknown fields are refused: a typo such as "interval" for "interval_sec"
	// would otherwise create the target with a setting nobody asked for.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		badTargetJSON(w, err)
		return t, false
	}
	if err := normalizeTarget(&t); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return t, false
	}
	return t, true
}

func badTargetJSON(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		jsonErr(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	msg := "bad JSON: " + err.Error()
	if f, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		msg = "unknown field " + f + "; accepted fields: id, name, type, host, port, pace, interval_sec, options"
	}
	jsonErr(w, http.StatusBadRequest, msg)
}

func (a *webApp) done(w http.ResponseWriter, t TargetCfg, err error, verb string) {
	switch {
	case errors.Is(err, errNameTaken), errors.Is(err, errNameDeleted):
		jsonErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, errNotFound):
		jsonErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		log.Printf("target %s: %v", verb, err)
		jsonErr(w, http.StatusInternalServerError, err.Error())
	default:
		if err := a.run.Reload(); err != nil {
			log.Printf("reload after %s: %v", verb, err)
		}
		log.Printf("web: target %s %q (%s)", verb, t.Name, targetAddr(t))
		writeJSON(w, t)
	}
}

func (a *webApp) registerTargets(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/targets", a.guard(a.handleTargets))
	mux.HandleFunc("POST /api/targets", a.write(a.createTarget))
	// Export and import of the target list as a "netping-targets" file (see
	// targetsio.go). Export is gated like GET /api/targets — the file says nothing
	// that list does not — so it works on a read-only instance too. Import changes
	// what this host probes, so it takes the same gate as every other write:
	// --edit, a session when users are configured, and a same-origin request.
	mux.HandleFunc("GET /api/targets/export", a.guard(func(w http.ResponseWriter, r *http.Request) {
		serveTargetsExport(w, a.store, "fogping")
	}))
	mux.HandleFunc("POST /api/targets/import", a.write(func(w http.ResponseWriter, r *http.Request, _ int64) {
		serveTargetsImport(w, r, a.store, a.run)
	}))
	mux.HandleFunc("PUT /api/targets/{id}", a.write(a.updateTarget))
	mux.HandleFunc("DELETE /api/targets/{id}", a.write(a.deleteTarget))
}

func (a *webApp) createTarget(w http.ResponseWriter, r *http.Request, _ int64) {
	if t, ok := decodeTarget(w, r); ok {
		t, err := a.store.CreateTarget(t)
		a.done(w, t, err, "created")
	}
}

func (a *webApp) updateTarget(w http.ResponseWriter, r *http.Request, id int64) {
	if t, ok := decodeTarget(w, r); ok {
		t.ID = id
		t, err := a.store.UpdateTarget(t)
		a.done(w, t, err, "updated")
	}
}

func (a *webApp) deleteTarget(w http.ResponseWriter, r *http.Request, id int64) {
	var t TargetCfg
	for _, x := range a.run.Targets() {
		if x.ID == id {
			t = x
		}
	}
	a.done(w, t, a.store.DeactivateTarget(id), "deleted")
}

// handleSeries returns raw rounds. Supports either minutes=N (recent window)
// or from/to unix (arbitrary range). No thinning: every sample is returned as
// stored.
func (a *webApp) handleSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("target")
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	if from == 0 || to == 0 {
		minutes, _ := strconv.Atoi(q.Get("minutes"))
		if minutes <= 0 || minutes > maxDays*1440 { // up to 93 days
			minutes = 360
		}
		to = time.Now().Unix()
		from = to - int64(minutes)*60
	}
	writeJSON(w, a.store.ReadRange(r.Context(), name, from, to))
}

// sameOrigin rejects browser requests whose Origin doesn't match the Host they were
// sent to. Non-browser clients (curl) send no Origin and are let through to auth.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}
