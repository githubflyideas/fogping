package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

// demoPingList is written on first run so the very first launch shows smoke.
const demoPingList = `# one ICMP target per line; # is a comment; saved changes apply automatically
# format: host  [name]  [pace=fast|slow]  [interval=seconds]
www.google.com Demo pace=fast
`

func main() {
	localOnly := flag.Bool("localhost", false, "bind 127.0.0.1 only; put Caddy/Nginx in front for auth/TLS")
	days := flag.Int("days", 40, "days of history to keep; UI hides windows beyond this")
	showVer := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVer {
		fmt.Println("fogping", version)
		return
	}

	// v2.1: no config file. Program parameters are constants; users edit target
	// lists and, optionally, pass web credentials on the command line:
	//   ./fogping user=u1,u2 passwd=p1,p2
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

	// first-run bootstrap: a demo target list, nothing else
	if _, err := os.Stat(filepath.Join(cfg.TargetsDir, "ping.list")); os.IsNotExist(err) {
		os.MkdirAll(cfg.TargetsDir, 0o755)
		os.WriteFile(filepath.Join(cfg.TargetsDir, "ping.list"), []byte(demoPingList), 0o644)
		log.Printf("no targets found — generated %s/ping.list (probing www.google.com)", cfg.TargetsDir)
		log.Printf(`add a target with one line: echo "1.2.3.4 my-link" >> %s/ping.list (applies automatically)`, cfg.TargetsDir)
	}
	if err := FinishConfig(cfg); err != nil {
		log.Fatalf("startup failed: %v", err)
	}

	store, err := NewStore(cfg.DataDir, cfg.Targets)
	if err != nil {
		log.Fatalf("store init failed: %v", err)
	}
	store.Replay()
	detector := NewDetector(store)

	// one probe loop per target, individually stoppable for hot reload
	mgr := map[string]chan struct{}{}
	for _, t := range cfg.Targets {
		ch := make(chan struct{})
		mgr[t.Name] = ch
		runningSig[t.Name] = t
		go probeLoop(t, cfg.Probe, store, detector, ch)
	}
	stop := make(chan struct{})
	go store.flushLoop(stop)
	go reloadLoop(cfg, store, detector, mgr, stop)
	go housekeeping(cfg, store, stop)

	log.Printf("fogping %s up · %d targets · listening on %s · data in %s · %d-day retention",
		version, len(cfg.Targets), cfg.Listen, cfg.DataDir, cfg.RetentionDays)
	log.Printf("➜  open http://localhost%s for the smoke graph", portOf(cfg.Listen))
	if len(users) == 0 {
		log.Printf("tip: web UI is open; protect it with  ./fogping user=u1,u2 passwd=p1,p2  or --localhost + reverse proxy")
	} else {
		log.Printf("web login enabled for %d user(s)", len(users))
	}

	go func() {
		if err := serveWeb(cfg, store, users); err != nil {
			log.Fatalf("web server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
	log.Printf("fogping shutting down")
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

var runningSig = map[string]TargetCfg{}

// reloadLoop: stdlib mtime polling every 3s — save the list, the chart follows.
func reloadLoop(cfg *Config, store *Store, det *Detector, mgr map[string]chan struct{}, stop chan struct{}) {
	stamp := func() string {
		out := ""
		for _, f := range []string{"ping.list", "tcp.list"} {
			if fi, err := os.Stat(filepath.Join(cfg.TargetsDir, f)); err == nil {
				out += fmt.Sprintf("%s:%d;", f, fi.ModTime().UnixNano())
			}
		}
		return out
	}
	last := stamp()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		if s := stamp(); s != last {
			last = s
			fresh, err := loadTargetLists(cfg.TargetsDir)
			if err != nil {
				log.Printf("target reload failed (keeping current set): %v", err)
				continue
			}
			tmp := &Config{Targets: fresh}
			if err := validateTargets(tmp); err != nil {
				log.Printf("target reload failed (keeping current set): %v", err)
				continue
			}
			applyTargets(cfg, tmp.Targets, store, det, mgr)
		}
	}
}

// applyTargets swaps the running target set without dropping in-memory history.
func applyTargets(cfg *Config, fresh []TargetCfg, store *Store, det *Detector, mgr map[string]chan struct{}) {
	sig := func(t TargetCfg) string {
		return fmt.Sprintf("%s|%s|%d|%s|%d", t.Type, t.Host, t.Port, t.Pace, t.IntervalSec)
	}
	want := map[string]TargetCfg{}
	for _, t := range fresh {
		want[t.Name] = t
	}
	for name, ch := range mgr {
		t, ok := want[name]
		if !ok || sig(t) != sig(runningSig[name]) {
			close(ch)
			delete(mgr, name)
			delete(runningSig, name)
			if !ok {
				store.RemoveTarget(name)
				log.Printf("[%s] target removed (data files kept)", name)
			}
		}
	}
	var all []TargetCfg
	for name, t := range want {
		all = append(all, t)
		if _, ok := mgr[name]; ok {
			continue
		}
		if err := store.EnsureTarget(t); err != nil {
			log.Printf("[%s] init failed: %v", name, err)
			continue
		}
		ch := make(chan struct{})
		mgr[name] = ch
		runningSig[name] = t
		go probeLoop(t, cfg.Probe, store, detector0(det), ch)
		log.Printf("[%s] target online (%s)", name, t.Host)
	}
	cfg.Targets = all
}

func detector0(d *Detector) *Detector { return d }

// housekeeping: nightly retention at 00:05 — filename-dated files, plain unlink.
func housekeeping(cfg *Config, store *Store, stop chan struct{}) {
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
		day := now.Format("2006-01-02")
		if now.Hour() == 0 && now.Minute() == 5 && last != day {
			last = day
			store.Rollup(time.Now().AddDate(0, 0, -2).Unix()) // 重算最近两天的聚合
			store.Retention(cfg.HotDays, cfg.RetentionDays)
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
