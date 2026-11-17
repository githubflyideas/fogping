package main

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Read-only statistics for Zabbix (zabbix/README.md).
//
// Off unless a stats token is configured, and off means absent: no route, no
// help text the user has to read, nothing in the UI. fogping without it is
// exactly fogping. With a token, GET /api/stats returns the latest summary of
// every target — percentiles, loss, bursts — and nothing else: no hosts, no
// ports, no version, no raw samples. It never writes.
//
// The token is its own secret, not a login: Zabbix's HTTP agent cannot hold a
// session. Like the password it is read from the environment or a file, never
// from an argument every local account can see in ps.

// statsWindow is how far back the summary looks. Five minutes covers several
// rounds of a fast or normal target; a slow one (300 s) may have none, which is
// reported as rounds=0 with no percentiles rather than as zeros.
const statsWindow = 5 * time.Minute

// statsTarget is the contract with the Zabbix template. Fields may be added;
// none may be renamed or change meaning.
type statsTarget struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Rounds  int      `json:"rounds"`
	P50     *float64 `json:"p50,omitempty"` // ms
	P90     *float64 `json:"p90,omitempty"`
	P99     *float64 `json:"p99,omitempty"`
	LossPct *float64 `json:"loss_pct,omitempty"`
	Bursts  int      `json:"bursts"`       // loss bursts flagged in the window (the chart's ◆)
	Up      *int     `json:"up,omitempty"` // 1 if the latest round got any reply, else 0
}

type statsReport struct {
	WindowSec int           `json:"window_sec"`
	Targets   []statsTarget `json:"targets"`
}

// resolveStatsToken takes stats-token-file=PATH out of the key=value arguments,
// then falls back to FOGPING_STATS_TOKEN / FOGPING_STATS_TOKEN_FILE. An empty
// result means the integration is off.
func resolveStatsToken(args []string, getenv func(string) string) (token string, rest []string, err error) {
	file := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "stats-token-file="); ok {
			file = v
			continue
		}
		rest = append(rest, a)
	}
	if file == "" {
		if token = getenv("FOGPING_STATS_TOKEN"); token != "" {
			if getenv("FOGPING_STATS_TOKEN_FILE") != "" {
				return "", rest, errStr("set FOGPING_STATS_TOKEN or FOGPING_STATS_TOKEN_FILE, not both")
			}
		} else {
			file = getenv("FOGPING_STATS_TOKEN_FILE")
		}
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", rest, errStr("stats token file: " + err.Error())
		}
		token = strings.TrimSpace(string(b))
	}
	if token != "" && len(token) < 16 {
		return "", rest, errStr("stats token must be at least 16 characters")
	}
	return token, rest, nil
}

type errStr string

func (e errStr) Error() string { return string(e) }

// registerStats adds GET /api/stats when a token is configured, and nothing
// otherwise.
func registerStats(mux *http.ServeMux, token string, store *Store, run *Runner) {
	if token == "" {
		return
	}
	throttle := newLoginThrottle() // wrong tokens are guesses too
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET")
			jsonErr(w, http.StatusMethodNotAllowed, "read-only")
			return
		}
		key := loginClientKey(r)
		if wait, ok := throttle.begin(key); !ok {
			w.Header().Set("Retry-After", retryAfter(wait))
			jsonErr(w, http.StatusTooManyRequests, "too many failed attempts")
			return
		}
		got, isBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !isBearer || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			if lock := throttle.fail(key); lock > 0 {
				log.Printf("stats: bad token from %s; address locked for %v", r.RemoteAddr, lock)
			}
			jsonErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		throttle.success(key)
		writeJSON(w, buildStats(store, run.Targets(), time.Now()))
	})
}

func buildStats(store *Store, targets []TargetCfg, now time.Time) statsReport {
	out := statsReport{WindowSec: int(statsWindow / time.Second), Targets: []statsTarget{}}
	since := now.Add(-statsWindow).Unix()
	for _, t := range targets {
		rec := store.Recent(t.Name, since)
		st := calcStats(rec)
		item := statsTarget{ID: t.ID, Name: t.Name, Rounds: st.Rounds, Bursts: st.Bursts}
		if st.Rounds > 0 {
			loss := st.LossPct
			item.LossPct = &loss
			up := 0
			if rec[len(rec)-1].R > 0 {
				up = 1
			}
			item.Up = &up
			if st.LossPct < 100 { // some replies, so the percentiles are real
				p50, p90, p99 := st.P50, st.P90, st.P99
				item.P50, item.P90, item.P99 = &p50, &p90, &p99
			}
		}
		out.Targets = append(out.Targets, item)
	}
	return out
}
