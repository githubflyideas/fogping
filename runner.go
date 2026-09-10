package main

import (
	"log"
	"sort"
	"sync"
)

// Runner owns the set of running probe loops, keyed by target id. Every change —
// web edit, list import — goes through Reload, which diffs the DB against what is
// running. One mutex, one writer path; the web handlers only read snapshots.
type Runner struct {
	mu    sync.Mutex
	probe ProbeCfg
	store *Store
	det   *Detector
	live  map[int64]liveTarget
}

type liveTarget struct {
	cfg  TargetCfg
	stop chan struct{}
}

func NewRunner(p ProbeCfg, s *Store, d *Detector) *Runner {
	return &Runner{probe: p, store: s, det: d, live: map[int64]liveTarget{}}
}

// Reload re-reads active targets from the DB and applies the difference.
func (r *Runner) Reload() error {
	ts, err := r.store.ActiveTargets()
	if err != nil {
		return err
	}
	r.apply(ts)
	return nil
}

func (r *Runner) apply(fresh []TargetCfg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := map[int64]TargetCfg{}
	for _, t := range fresh {
		want[t.ID] = t
	}
	for id, lt := range r.live {
		t, ok := want[id]
		if ok && t == lt.cfg {
			continue
		}
		close(lt.stop)
		delete(r.live, id)
		switch {
		case !ok:
			r.store.RemoveTarget(lt.cfg.Name)
			log.Printf("[%s] target removed (history kept)", lt.cfg.Name)
		case t.Name != lt.cfg.Name:
			r.store.RenameTarget(lt.cfg.Name, t.Name)
			log.Printf("[%s] renamed to %q", lt.cfg.Name, t.Name)
		}
	}
	for id, t := range want {
		if _, ok := r.live[id]; ok {
			continue
		}
		if err := r.store.EnsureTarget(t); err != nil {
			log.Printf("[%s] init failed: %v", t.Name, err)
			continue
		}
		stop := make(chan struct{})
		r.live[id] = liveTarget{cfg: t, stop: stop}
		go probeLoop(t, r.probe, r.store, r.det, stop)
		log.Printf("[%s] probing %s", t.Name, targetAddr(t))
	}
}

// Targets is a snapshot of what is running, in creation order.
func (r *Runner) Targets() []TargetCfg {
	r.mu.Lock()
	out := make([]TargetCfg, 0, len(r.live))
	for _, lt := range r.live {
		out = append(out, lt.cfg)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
