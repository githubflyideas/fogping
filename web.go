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
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

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

func serveWeb(cfg *Config, store *Store, run *Runner, users map[string]string) error {
	return http.ListenAndServe(cfg.Listen, newMux(cfg, store, run, users))
}

func newMux(cfg *Config, store *Store, run *Runner, users map[string]string) http.Handler {
	sess := &sessions{m: map[string]time.Time{}}
	mux := http.NewServeMux()

	authed := func(r *http.Request) bool {
		if len(users) == 0 {
			return true
		}
		c, err := r.Cookie("fogping_session")
		return err == nil && sess.valid(c.Value)
	}
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authed(r) {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			t0 := time.Now()
			h(w, r)
			if d := time.Since(t0); d > time.Second {
				log.Printf("slow request: %s %s took %v", r.URL.Path, r.URL.RawQuery, d)
			}
		}
	}
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := staticFS.ReadFile("static/" + name)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(b)
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !authed(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		page("index.html")(w, r)
	})
	mux.HandleFunc("/login", page("login.html"))
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))

	// login: constant-time compare, 1s delay on failure to make brute force boring
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var body struct{ User, Pass string }
		json.NewDecoder(r.Body).Decode(&body)
		want, ok := users[body.User]
		if len(users) == 0 || !ok ||
			subtle.ConstantTimeCompare([]byte(body.Pass), []byte(want)) != 1 {
			time.Sleep(time.Second)
			log.Printf("web login failed from %s", r.RemoteAddr)
			http.Error(w, `{"error":"auth"}`, http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "fogping_session", Value: sess.issue(),
			Path: "/", HttpOnly: true, MaxAge: 7 * 24 * 3600, SameSite: http.SameSiteLaxMode})
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "fogping_session", Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"version": version, "retention_days": cfg.RetentionDays,
			"editable": cfg.Editable})
	})

	mux.HandleFunc("GET /api/targets", guard(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		type item struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Type        string `json:"type"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			Addr        string `json:"addr"`
			IntervalSec int    `json:"interval_sec"` // effective
			IntervalSet int    `json:"interval_set"` // explicit override, 0 = from pace
			Pace        string `json:"pace"`
			Down        bool   `json:"down"`
			Last1h      Stats  `json:"last_1h"`
			Last24h     Stats  `json:"last_24h"`
		}
		out := []item{}
		for _, t := range run.Targets() {
			iv, _ := probeParams(t, cfg.Probe)
			pace := t.Pace
			if pace == "" {
				pace = "normal"
			}
			rec := store.Recent(t.Name, now.Add(-time.Hour).Unix())
			down := len(rec) > 0 && rec[len(rec)-1].R == 0
			out = append(out, item{
				ID: t.ID, Name: t.Name, Type: t.Type, Host: t.Host, Port: t.Port, Addr: targetAddr(t),
				IntervalSec: int(iv.Seconds()), IntervalSet: t.IntervalSec, Pace: pace, Down: down,
				Last1h:  calcStats(rec),
				Last24h: calcStats(store.Recent(t.Name, now.Add(-24*time.Hour).Unix())),
			})
		}
		writeJSON(w, out)
	}))

	// Target CRUD. Always routed so a read-only instance answers with a reason
	// instead of a bare 405; every write goes DB first, then Runner.Reload.
	write := func(h func(w http.ResponseWriter, r *http.Request, id int64)) http.HandlerFunc {
		return guard(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Editable {
				jsonErr(w, http.StatusForbidden, "read-only: restart fogping with --edit to change targets")
				return
			}
			if !sameOrigin(r) {
				jsonErr(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
			var id int64
			if v := r.PathValue("id"); v != "" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil || n <= 0 {
					jsonErr(w, http.StatusBadRequest, "bad id")
					return
				}
				id = n
			}
			h(w, r, id)
		})
	}
	decode := func(w http.ResponseWriter, r *http.Request) (TargetCfg, bool) {
		var t TargetCfg
		// JSON content type forces a CORS preflight on cross-site requests, which we
		// never answer — so a form on another site can't drive these endpoints.
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			jsonErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return t, false
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&t); err != nil {
			jsonErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
			return t, false
		}
		if err := normalizeTarget(&t); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return t, false
		}
		return t, true
	}
	done := func(w http.ResponseWriter, t TargetCfg, err error, verb string) {
		switch {
		case errors.Is(err, errNameTaken), errors.Is(err, errNameDeleted):
			jsonErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, errNotFound):
			jsonErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			log.Printf("target %s: %v", verb, err)
			jsonErr(w, http.StatusInternalServerError, err.Error())
		default:
			if err := run.Reload(); err != nil {
				log.Printf("reload after %s: %v", verb, err)
			}
			log.Printf("web: target %s %q (%s)", verb, t.Name, targetAddr(t))
			writeJSON(w, t)
		}
	}
	mux.HandleFunc("POST /api/targets", write(func(w http.ResponseWriter, r *http.Request, _ int64) {
		if t, ok := decode(w, r); ok {
			t, err := store.CreateTarget(t)
			done(w, t, err, "created")
		}
	}))
	mux.HandleFunc("PUT /api/targets/{id}", write(func(w http.ResponseWriter, r *http.Request, id int64) {
		if t, ok := decode(w, r); ok {
			t.ID = id
			t, err := store.UpdateTarget(t)
			done(w, t, err, "updated")
		}
	}))
	mux.HandleFunc("DELETE /api/targets/{id}", write(func(w http.ResponseWriter, r *http.Request, id int64) {
		var t TargetCfg
		for _, x := range run.Targets() {
			if x.ID == id {
				t = x
			}
		}
		done(w, t, store.DeactivateTarget(id), "deleted")
	}))

	// raw rounds for smoke. Supports either minutes=N (recent window) or from/to unix
	// (arbitrary range). No thinning: every sample is returned as stored.
	mux.HandleFunc("/api/series", guard(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		name := q.Get("target")
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
		if from == 0 || to == 0 {
			minutes, _ := strconv.Atoi(q.Get("minutes"))
			if minutes <= 0 || minutes > 432000 { // up to 300 days
				minutes = 360
			}
			to = time.Now().Unix()
			from = to - int64(minutes)*60
		}
		writeJSON(w, store.ReadRange(r.Context(), name, from, to))
	}))

	return mux
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

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
