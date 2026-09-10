package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

// demoTarget is seeded into a brand-new database so the very first launch shows smoke.
var demoTarget = TargetCfg{Name: "Demo", Type: "icmp", Host: "www.google.com", Pace: "fast"}

func main() {
	localOnly := flag.Bool("localhost", false, "bind 127.0.0.1 only; put Caddy/Nginx in front for auth/TLS")
	days := flag.Int("days", 40, "days of history to keep; UI hides windows beyond this")
	edit := flag.Bool("edit", false, "allow adding/editing/deleting targets from the web UI (default: read-only)")
	showVer := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVer {
		fmt.Println("fogping", version)
		return
	}

	cfg := defaultConfig()
	if *days > 0 {
		cfg.RetentionDays = *days
	}
	users, err := parseAuthArgs(flag.Args())
	if err != nil {
		log.Fatalf("bad auth args: %v", err)
	}
	if *localOnly {
		cfg.Listen = "127.0.0.1" + portOf(cfg.Listen)
	}
	cfg.Editable = *edit
	// An open, writable UI would let anyone who can reach the port make this host
	// ping or TCP-connect to arbitrary addresses. Refuse rather than warn.
	if cfg.Editable && len(users) == 0 && !*localOnly {
		log.Fatalf("--edit needs a login (user=... passwd=...) or --localhost behind your own auth proxy")
	}

	store, err := NewStore(cfg.DataDir, nil)
	if err != nil {
		log.Fatalf("store init failed: %v", err)
	}
	os.MkdirAll(cfg.TargetsDir, 0o755)
	if _, err := ingestLists(cfg.TargetsDir, store); err != nil {
		log.Printf("ingest: %v", err)
	}
	if n, err := store.TargetRows(); err == nil && n == 0 {
		if err := store.ImportTargets([]TargetCfg{demoTarget}); err == nil {
			log.Printf("new database — seeded a demo target (www.google.com)")
		}
	}

	detector := NewDetector(store)
	run := NewRunner(cfg.Probe, store, detector)
	if err := run.Reload(); err != nil {
		log.Fatalf("load targets: %v", err)
	}
	stop := make(chan struct{})
	go store.flushLoop(stop)
	go ingestLoop(cfg.TargetsDir, store, run, stop)
	go housekeeping(cfg, store, stop)

	mode := "read-only targets (restart with --edit to change them in the web UI)"
	if cfg.Editable {
		mode = "targets editable in the web UI"
	}
	log.Printf("fogping %s up · %d targets · %s · listening on %s · data in %s · %d-day retention",
		version, len(run.Targets()), mode, cfg.Listen, cfg.DataDir, cfg.RetentionDays)
	if len(run.Targets()) == 0 {
		log.Printf(`no active targets — add them with --edit, or: echo "1.2.3.4 my-link" >> %s/ping.list`, cfg.TargetsDir)
	}
	log.Printf("➜  open http://localhost%s for the smoke graph", portOf(cfg.Listen))
	if len(users) == 0 {
		log.Printf("tip: web UI is open; protect it with  ./fogping user=u1,u2 passwd=p1,p2  or --localhost + reverse proxy")
	} else {
		log.Printf("web login enabled for %d user(s)", len(users))
	}

	go func() {
		if err := serveWeb(cfg, store, run, users); err != nil {
			log.Fatalf("web server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
	if err := store.Close(); err != nil { // final flush; waits for in-flight queries
		log.Printf("close: %v", err)
	}
	log.Printf("fogping shut down")
}

// parseAuthArgs parses trailing "user=a,b passwd=x,y" arguments into a cred map.
func parseAuthArgs(args []string) (map[string]string, error) {
	var users, pws []string
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("unrecognized argument %q (expected user=... passwd=...)", a)
		}
		switch k {
		case "user", "users":
			users = strings.Split(v, ",")
		case "passwd", "password", "passwords":
			pws = strings.Split(v, ",")
		default:
			return nil, fmt.Errorf("unknown key %q (expected user=, passwd=)", k)
		}
	}
	if len(users) == 0 && len(pws) == 0 {
		return nil, nil
	}
	if len(users) != len(pws) {
		return nil, fmt.Errorf("user count (%d) != passwd count (%d)", len(users), len(pws))
	}
	m := map[string]string{}
	for i := range users {
		if users[i] == "" || pws[i] == "" {
			return nil, fmt.Errorf("empty user or passwd at position %d", i+1)
		}
		m[users[i]] = pws[i]
	}
	return m, nil
}

// housekeeping: rollup on start and every 5 minutes; retention nightly at 00:05.
func housekeeping(cfg *Config, store *Store, stop chan struct{}) {
	sched := &rollupSched{hot: time.Duration(cfg.HotDays) * 24 * time.Hour}
	sched.tick(store, time.Now())
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	last := ""
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		now := time.Now()
		sched.tick(store, now)
		if day := now.Format("2006-01-02"); now.Hour() == 0 && now.Minute() == 5 && last != day {
			last = day
			store.Retention(now, cfg.HotDays, cfg.RetentionDays)
		}
	}
}

func portOf(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return ":8518"
}
