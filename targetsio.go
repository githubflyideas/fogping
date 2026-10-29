package main

// Target lists as files: the "netping-targets" format, version 1.
//
// pingping and fogping carry this file byte for byte, so a list exported from one
// imports into the other. The format is specified in docs/targets-format.md and
// docs/targets.schema.json; keep all three in step.
//
// Import is add-only. It never edits or removes an existing target, and every row
// goes through normalizeTarget — the same gate as the editor — so a file cannot
// create anything the console could not. All accepted rows are written in one
// transaction and the runner reloads once: either the whole batch is probing, or
// none of it exists.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	targetsFormat        = "netping-targets"
	targetsFormatVersion = 1
	maxImportBytes       = 1 << 20 // 1 MB: ~10k rows of headroom over the count cap
	maxImportTargets     = 500
)

// fileTarget is one row of the file. Only these fields travel: no ids, history,
// accounts or settings.
type fileTarget struct {
	Name     string `json:"name,omitempty"`
	Type     string `json:"type"` // "ping" | "tcp" ("icmp" accepted on import)
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"` // tcp only
	Pace     string `json:"pace,omitempty"` // fast | normal | slow
	Interval int    `json:"interval,omitempty"`
}

type targetsFile struct {
	Format      string       `json:"format"`
	Version     int          `json:"version"`
	Name        string       `json:"name,omitempty"`
	Description string       `json:"description,omitempty"`
	Targets     []fileTarget `json:"targets"`
}

func toFileTarget(t TargetCfg) fileTarget {
	f := fileTarget{Name: t.Name, Type: "ping", Host: t.Host, Pace: t.Pace, Interval: t.IntervalSec}
	if t.Type == "tcp" {
		f.Type, f.Port = "tcp", t.Port
	}
	if f.Pace == "" {
		f.Pace = "normal"
	}
	return f
}

// exportTargets renders the active targets in the order the database keeps them
// (by id, i.e. creation order), so two exports of an unchanged list are identical.
func exportTargets(ts []TargetCfg, app string, now time.Time) targetsFile {
	out := targetsFile{
		Format:      targetsFormat,
		Version:     targetsFormatVersion,
		Name:        app + " targets",
		Description: "Exported from " + app + " on " + now.Format("2006-01-02"),
		Targets:     make([]fileTarget, 0, len(ts)),
	}
	for _, t := range ts {
		out.Targets = append(out.Targets, toFileTarget(t))
	}
	return out
}

// fromFileRow decodes one row on its own, so a bad row is reported against its
// number instead of rejecting the file, then applies the editor's validation.
func fromFileRow(raw json.RawMessage) (TargetCfg, string, error) {
	var t TargetCfg
	if b := bytes.TrimSpace(raw); len(b) == 0 || b[0] != '{' {
		return t, "", errors.New("row is not a JSON object")
	}
	var f fileTarget
	err := json.Unmarshal(raw, &f)
	name := strings.TrimSpace(f.Name)
	if err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return t, name, fmt.Errorf("field %q has the wrong type (%s)", te.Field, te.Value)
		}
		return t, name, err
	}
	switch strings.ToLower(strings.TrimSpace(f.Type)) {
	case "ping", "icmp":
		t.Type = "icmp"
	case "tcp":
		t.Type = "tcp"
	case "":
		return t, name, errors.New("type is required (ping|tcp)")
	default:
		return t, name, fmt.Errorf("unknown type %q (ping|tcp)", f.Type)
	}
	t.Name, t.Host, t.Port, t.Pace, t.IntervalSec = f.Name, f.Host, f.Port, f.Pace, f.Interval
	if err := normalizeTarget(&t); err != nil {
		return t, name, err
	}
	return t, t.Name, nil
}

// importError is a whole-file refusal: nothing in the file is looked at further.
type importError struct {
	code int
	msg  string
}

func (e *importError) Error() string { return e.msg }

type importAdd struct {
	Row int `json:"row"`
	fileTarget
	// RestoresHistory: a deleted target had this name. Adding it reactivates that
	// row — the same thing the editor does — so its old history comes back.
	RestoresHistory bool `json:"restores_history,omitempty"`
}

type importSkip struct {
	Row    int    `json:"row"`
	Name   string `json:"name"`
	Code   string `json:"code"` // "exists" | "duplicate"
	Reason string `json:"reason"`
}

type importInvalid struct {
	Row   int    `json:"row"`
	Name  string `json:"name,omitempty"`
	Error string `json:"error"`
}

type importPlan struct {
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Total       int             `json:"total"`
	Add         []importAdd     `json:"add"`
	Skip        []importSkip    `json:"skip"`
	Invalid     []importInvalid `json:"invalid"`
	DryRun      bool            `json:"dry_run"`
	Imported    int             `json:"imported"`
	cfgs        []TargetCfg
}

// planImport parses a file and sorts every row into add, skip or invalid against
// the names already in the database (name -> active). Rows are numbered from 1.
func planImport(body []byte, existing map[string]bool) (*importPlan, error) {
	var head struct {
		Format      string          `json:"format"`
		Version     json.RawMessage `json:"version"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Targets     json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, &importError{http.StatusBadRequest, "not a targets file: " + err.Error()}
	}
	if head.Format != targetsFormat {
		return nil, &importError{http.StatusBadRequest,
			fmt.Sprintf("not a targets file: format is %q, expected %q", head.Format, targetsFormat)}
	}
	var version int
	if err := json.Unmarshal(head.Version, &version); err != nil || version < 1 {
		return nil, &importError{http.StatusBadRequest, "targets file has no valid version"}
	}
	if version > targetsFormatVersion {
		return nil, &importError{http.StatusBadRequest, fmt.Sprintf(
			"targets file version %d is newer than this build understands (%d); upgrade to import it",
			version, targetsFormatVersion)}
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(head.Targets, &rows); err != nil || rows == nil {
		return nil, &importError{http.StatusBadRequest, `targets file needs a "targets" array`}
	}
	if len(rows) > maxImportTargets {
		return nil, &importError{http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"file has %d targets; at most %d can be imported at once", len(rows), maxImportTargets)}
	}
	p := &importPlan{Name: head.Name, Description: head.Description, Total: len(rows),
		Add: []importAdd{}, Skip: []importSkip{}, Invalid: []importInvalid{}}
	seen := map[string]int{}
	for i, raw := range rows {
		row := i + 1
		t, name, err := fromFileRow(raw)
		if err != nil {
			p.Invalid = append(p.Invalid, importInvalid{Row: row, Name: name, Error: err.Error()})
			continue
		}
		if first, dup := seen[t.Name]; dup {
			p.Skip = append(p.Skip, importSkip{Row: row, Name: t.Name, Code: "duplicate",
				Reason: fmt.Sprintf("same name as row %d of this file", first)})
			continue
		}
		seen[t.Name] = row
		active, known := existing[t.Name]
		if known && active {
			p.Skip = append(p.Skip, importSkip{Row: row, Name: t.Name, Code: "exists",
				Reason: "a target with this name already exists"})
			continue
		}
		p.Add = append(p.Add, importAdd{Row: row, fileTarget: toFileTarget(t), RestoresHistory: known})
		p.cfgs = append(p.cfgs, t)
	}
	return p, nil
}

func exportFilename(app string, now time.Time) string {
	return app + "-targets-" + now.Format("20060102") + ".json"
}

// serveTargetsExport answers GET /api/targets/export. The caller wraps it in the
// same gate as GET /api/targets: the file says nothing that list does not.
func serveTargetsExport(w http.ResponseWriter, store *Store, app string) {
	ts, err := store.ActiveTargets()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename(app, now)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(exportTargets(ts, app, now))
}

// serveTargetsImport answers POST /api/targets/import[?dry_run=1]. The caller has
// already applied the write gate (session, role or --edit, same origin).
func serveTargetsImport(w http.ResponseWriter, r *http.Request, store *Store, run *Runner) {
	// A JSON content type forces a CORS preflight that is never answered, exactly
	// as for the other write endpoints.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		jsonErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file is larger than %d KB", maxImportBytes>>10))
			return
		}
		jsonErr(w, http.StatusBadRequest, "could not read the upload")
		return
	}
	existing, err := store.TargetNames()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, err := planImport(body, existing)
	if err != nil {
		var ie *importError
		if errors.As(err, &ie) {
			jsonErr(w, ie.code, ie.msg)
			return
		}
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch strings.ToLower(r.URL.Query().Get("dry_run")) {
	case "1", "true", "yes":
		p.DryRun = true
		writeJSON(w, p)
		return
	}
	if len(p.cfgs) > 0 {
		if err := store.ImportTargets(p.cfgs); err != nil {
			if errors.Is(err, errNameTaken) {
				jsonErr(w, http.StatusConflict,
					"the target list changed while importing; nothing was imported — preview again")
				return
			}
			log.Printf("targets: import failed, nothing applied: %v", err)
			jsonErr(w, http.StatusInternalServerError, "import failed, nothing was applied: "+err.Error())
			return
		}
		if err := run.Reload(); err != nil {
			log.Printf("reload after import: %v", err)
		}
	}
	p.Imported = len(p.cfgs)
	log.Printf("targets: imported %d (skipped %d, invalid %d) from %s",
		p.Imported, len(p.Skip), len(p.Invalid), r.RemoteAddr)
	writeJSON(w, p)
}
