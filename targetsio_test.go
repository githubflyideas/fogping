package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Shared with fogping/pingping byte for byte, like targetsio.go.

func mustPlan(t *testing.T, body string, existing map[string]bool) *importPlan {
	t.Helper()
	p, err := planImport([]byte(body), existing)
	if err != nil {
		t.Fatalf("planImport: %v", err)
	}
	return p
}

func TestTargetsExportImportRoundTrip(t *testing.T) {
	a, _ := NewStore(t.TempDir(), nil)
	defer a.Close()
	in := []TargetCfg{
		{Name: "Gateway", Type: "icmp", Host: "10.0.0.1", Pace: "fast", IntervalSec: 10},
		{Name: "", Type: "tcp", Host: "www.example.com", Port: 443},
		{Name: "v6 dns", Type: "tcp", Host: "2001:db8::53", Port: 53, Pace: "slow"},
	}
	for _, c := range in {
		if err := normalizeTarget(&c); err != nil {
			t.Fatal(err)
		}
		if _, err := a.CreateTarget(c); err != nil {
			t.Fatal(err)
		}
	}
	ts, _ := a.ActiveTargets()
	f := exportTargets(ts, "test", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	raw, _ := json.Marshal(f)
	for _, banned := range []string{`"id"`, `"interval_sec"`, `"active"`} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("export leaks %s: %s", banned, raw)
		}
	}
	if !strings.Contains(string(raw), `"type":"ping"`) || strings.Contains(string(raw), `"icmp"`) {
		t.Errorf("export must write ping, not icmp: %s", raw)
	}
	if again, _ := json.Marshal(exportTargets(ts, "test", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))); string(again) != string(raw) {
		t.Error("two exports of the same list differ")
	}

	b, _ := NewStore(t.TempDir(), nil)
	defer b.Close()
	p := mustPlan(t, string(raw), map[string]bool{})
	if len(p.Add) != 3 || len(p.Skip) != 0 || len(p.Invalid) != 0 {
		t.Fatalf("plan: %+v", p)
	}
	if err := b.ImportTargets(p.cfgs); err != nil {
		t.Fatal(err)
	}
	got, _ := b.ActiveTargets()
	if len(got) != len(ts) {
		t.Fatalf("got %d targets, want %d", len(got), len(ts))
	}
	for i := range got {
		got[i].ID, ts[i].ID = 0, 0
		if got[i] != ts[i] {
			t.Errorf("row %d: %+v != %+v", i, got[i], ts[i])
		}
	}
	// Importing the same file again adds nothing.
	names, _ := b.TargetNames()
	if p := mustPlan(t, string(raw), names); len(p.Add) != 0 || len(p.Skip) != 3 {
		t.Fatalf("re-import should skip all: %+v", p)
	}
}

func TestTargetsImportPlanRows(t *testing.T) {
	long := strings.Repeat("x", 65)
	body := `{"format":"netping-targets","version":1,"name":"t","extra":{"ignored":true},"targets":[
		{"name":"live","type":"tcp","host":"192.0.2.1","port":443},
		{"name":"gone","type":"ping","host":"192.0.2.2"},
		{"name":"new","type":"icmp","host":"192.0.2.3","pace":"fast","interval":10,"colour":"red"},
		{"name":"new","type":"ping","host":"192.0.2.4"},
		{"type":"tcp","host":"192.0.2.5","port":80},
		{"name":"bad type","type":"udp","host":"192.0.2.6"},
		{"name":"no type","host":"192.0.2.6"},
		{"name":"no port","type":"tcp","host":"192.0.2.7"},
		{"name":"big port","type":"tcp","host":"192.0.2.7","port":70000},
		{"name":"string port","type":"tcp","host":"192.0.2.7","port":"443"},
		{"name":"pace","type":"ping","host":"192.0.2.8","pace":"turbo"},
		{"name":"neg iv","type":"ping","host":"192.0.2.8","interval":-1},
		{"name":"huge iv","type":"ping","host":"192.0.2.8","interval":90000},
		{"name":"no host","type":"ping","host":"  "},
		{"name":"space host","type":"ping","host":"a b"},
		{"name":"` + long + `","type":"ping","host":"192.0.2.9"},
		"192.0.2.10",
		{"name":"ping port dropped","type":"PING","host":"192.0.2.11","port":7}
	]}`
	p := mustPlan(t, body, map[string]bool{"live": true, "gone": false})
	if p.Total != 18 {
		t.Errorf("total %d", p.Total)
	}
	var adds []string
	for _, a := range p.Add {
		adds = append(adds, fmt.Sprintf("%d:%s:%v", a.Row, a.Name, a.RestoresHistory))
	}
	if got, want := strings.Join(adds, " "), "2:gone:true 3:new:false 5:192.0.2.5:80:false 18:ping port dropped:false"; got != want {
		t.Errorf("adds\n got %s\nwant %s", got, want)
	}
	if len(p.Skip) != 2 || p.Skip[0].Code != "exists" || p.Skip[0].Row != 1 ||
		p.Skip[1].Code != "duplicate" || p.Skip[1].Row != 4 {
		t.Errorf("skips: %+v", p.Skip)
	}
	invalidRows := map[int]bool{}
	for _, iv := range p.Invalid {
		invalidRows[iv.Row] = true
		if iv.Error == "" {
			t.Errorf("row %d invalid without a reason", iv.Row)
		}
	}
	for _, r := range []int{6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17} {
		if !invalidRows[r] {
			t.Errorf("row %d should be invalid; invalid=%+v", r, p.Invalid)
		}
	}
	if len(p.Invalid) != 12 {
		t.Errorf("want 12 invalid, got %d: %+v", len(p.Invalid), p.Invalid)
	}
	// The editor's normalisation applies: normal pace, ping has no port.
	last := p.cfgs[len(p.cfgs)-1]
	if last.Type != "icmp" || last.Port != 0 || last.Pace != "" {
		t.Errorf("row 18 not normalised: %+v", last)
	}
	if a := p.Add[1]; a.Type != "ping" || a.Pace != "fast" || a.Interval != 10 {
		t.Errorf("row 3 preview: %+v", a)
	}
}

func TestTargetsImportFileRefusals(t *testing.T) {
	many := make([]string, maxImportTargets+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"type":"ping","host":"192.0.2.%d","name":"n%d"}`, i%250+1, i)
	}
	cases := map[string]string{
		"not json":       `hello`,
		"no format":      `{"version":1,"targets":[]}`,
		"other format":   `{"format":"smokeping","version":1,"targets":[]}`,
		"newer version":  `{"format":"netping-targets","version":2,"targets":[]}`,
		"zero version":   `{"format":"netping-targets","version":0,"targets":[]}`,
		"string version": `{"format":"netping-targets","version":"1","targets":[]}`,
		"no targets":     `{"format":"netping-targets","version":1}`,
		"targets object": `{"format":"netping-targets","version":1,"targets":{}}`,
		"too many":       `{"format":"netping-targets","version":1,"targets":[` + strings.Join(many, ",") + `]}`,
	}
	for name, body := range cases {
		_, err := planImport([]byte(body), nil)
		var ie *importError
		if !errors.As(err, &ie) {
			t.Errorf("%s: want a whole-file refusal, got %v", name, err)
		}
	}
	if _, err := planImport([]byte(`{"format":"netping-targets","version":2,"targets":[]}`), nil); err == nil ||
		!strings.Contains(err.Error(), "newer") {
		t.Errorf("newer version should say so, got %v", err)
	}
	if p, err := planImport([]byte(`{"format":"netping-targets","version":1,"targets":[]}`), nil); err != nil || len(p.Add) != 0 {
		t.Errorf("an empty list is a valid file: %v", err)
	}
	if p, err := planImport([]byte(`{"format":"netping-targets","version":1,"targets":[`+strings.Join(many[:maxImportTargets], ",")+`]}`), nil); err != nil || len(p.Add) != maxImportTargets {
		t.Errorf("exactly the cap must pass: %v", err)
	}
}

// One refused row rolls the whole batch back.
func TestImportTargetsIsAtomic(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	if _, err := s.CreateTarget(TargetCfg{Name: "taken", Type: "icmp", Host: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	err := s.ImportTargets([]TargetCfg{
		{Name: "fresh", Type: "icmp", Host: "192.0.2.2"},
		{Name: "taken", Type: "icmp", Host: "192.0.2.3"},
	})
	if !errors.Is(err, errNameTaken) {
		t.Fatalf("want errNameTaken, got %v", err)
	}
	names, _ := s.TargetNames()
	if _, ok := names["fresh"]; ok {
		t.Fatal("a refused batch left a row behind")
	}
	if ts, _ := s.ActiveTargets(); len(ts) != 1 || ts[0].Host != "192.0.2.1" {
		t.Fatalf("existing target changed: %+v", ts)
	}
}

// A deleted name comes back on its old id, exactly as when re-added in the editor.
func TestImportRestoresDeletedName(t *testing.T) {
	s, _ := NewStore(t.TempDir(), nil)
	defer s.Close()
	old, _ := s.CreateTarget(TargetCfg{Name: "old", Type: "icmp", Host: "192.0.2.1"})
	s.DeactivateTarget(old.ID)
	names, _ := s.TargetNames()
	p := mustPlan(t, `{"format":"netping-targets","version":1,"targets":[{"name":"old","type":"tcp","host":"192.0.2.9","port":22}]}`, names)
	if len(p.Add) != 1 || !p.Add[0].RestoresHistory {
		t.Fatalf("preview should say history is restored: %+v", p)
	}
	if err := s.ImportTargets(p.cfgs); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.ActiveTargets()
	if len(ts) != 1 || ts[0].ID != old.ID || ts[0].Type != "tcp" || ts[0].Port != 22 {
		t.Fatalf("deleted name should reactivate id %d with the file's settings: %+v", old.ID, ts)
	}
}
