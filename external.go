package main

// External probers: a program in any language that speaks fogping-probe/1
// (docs/PROBE-PROTOCOL.md). Off unless fogping runs with --probes. Then each
// directory under ./probes that holds a manifest.json and an executable file named
// `probe` registers one more probe type, through the same registry the built-in
// ones use. A broken plugin is skipped with a log line; it never stops fogping.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	probesDir       = "probes"
	manifestFile    = "manifest.json"
	probeExecutable = "probe"

	maxStderrLog  = 2048
	maxConcurrent = 8 // external processes running at once, across all targets
)

// runMargin is allowed on top of packets*(gap+timeout) before a prober is stopped.
var runMargin = 5 * time.Second

// One empty directory is every external prober's working directory, so a plugin
// that writes to "." cannot touch anything of fogping's. It is created when the
// first plugin loads and removed by cleanupProbes at shutdown.
var (
	workdirMu  sync.Mutex
	extWorkdir string
)

func emptyWorkdir() (string, error) {
	workdirMu.Lock()
	defer workdirMu.Unlock()
	if extWorkdir == "" {
		d, err := os.MkdirTemp("", "fogping-probe-")
		if err != nil {
			return "", err
		}
		extWorkdir = d
	}
	return extWorkdir, nil
}

func cleanupProbes() {
	workdirMu.Lock()
	defer workdirMu.Unlock()
	if extWorkdir != "" {
		os.RemoveAll(extWorkdir)
		extWorkdir = ""
	}
}

var extSlots = make(chan struct{}, maxConcurrent)

// loadProbes registers every valid plugin under dir and logs the rest. It returns
// the types it registered.
func loadProbes(dir string) []string {
	// The directory holds programs fogping will run, so whoever can write to it can
	// run code as fogping. Refuse a directory that others can write to or that
	// another account owns, rather than load whatever they put there.
	if fi, err := os.Stat(dir); err == nil {
		if why := untrusted(fi); why != "" {
			log.Printf("probes: not loading from %s: the directory is %s", dir, why)
			return nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("probes: --probes is on but %s/ does not exist; no external probers loaded", dir)
		} else {
			log.Printf("probes: cannot read %s: %v", dir, err)
		}
		return nil
	}
	var loaded []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, err := loadProbe(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Printf("probes: skipping %s: %v", e.Name(), err)
			continue
		}
		loaded = append(loaded, name)
	}
	sort.Strings(loaded)
	if len(loaded) > 0 {
		log.Printf("probes: loaded %s", strings.Join(loaded, ", "))
	}
	return loaded
}

// untrusted says why a file or directory cannot be trusted to hold code that
// fogping runs, or "" when it can: writable by group or others, or owned by an
// account other than fogping's own and root.
func untrusted(fi os.FileInfo) string {
	if fi.Mode().Perm()&0o022 != 0 {
		return "writable by group or others"
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		return "owned by another account"
	}
	return ""
}

func loadProbe(dir string) (string, error) {
	if fi, err := os.Lstat(dir); err != nil {
		return "", err
	} else if why := untrusted(fi); why != "" {
		return "", fmt.Errorf("the directory is %s", why)
	}
	mpath := filepath.Join(dir, manifestFile)
	fi, err := os.Lstat(mpath)
	if err != nil {
		return "", fmt.Errorf("no %s", manifestFile)
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxManifestBytes {
		return "", fmt.Errorf("%s must be a regular file of at most %d bytes", manifestFile, maxManifestBytes)
	}
	raw, err := os.ReadFile(mpath)
	if err != nil {
		return "", err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", manifestFile, err)
	}
	if _, taken := proberFor(m.Type); taken {
		return "", fmt.Errorf("type %q is already taken", m.Type)
	}
	// The plugin's directory name must match its type, so what is on disk is what
	// the UI shows and nothing can be registered under a surprising name.
	if base := filepath.Base(dir); base != m.Type {
		return "", fmt.Errorf("directory is named %q but the manifest's type is %q", base, m.Type)
	}
	exe, err := filepath.Abs(filepath.Join(dir, probeExecutable))
	if err != nil {
		return "", err
	}
	efi, err := os.Lstat(exe)
	if err != nil {
		return "", fmt.Errorf("no executable file named %q", probeExecutable)
	}
	if !efi.Mode().IsRegular() || efi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%q must be a regular file with the execute bit set", probeExecutable)
	}
	if why := untrusted(efi); why != "" {
		return "", fmt.Errorf("%q is %s", probeExecutable, why)
	}
	workdir, err := emptyWorkdir()
	if err != nil {
		return "", err
	}
	if err := registerProber(&prober{
		name:     m.Type,
		label:    m.Label,
		usesPort: m.UsesPort,
		options:  m.Options,
		validate: func(t *TargetCfg) error {
			if !m.UsesPort {
				t.Port = 0
			} else if t.Port < 1 || t.Port > 65535 {
				return fmt.Errorf("%s target needs a port 1-65535", m.Type)
			}
			return nil
		},
		round: func(ctx context.Context, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error) {
			return runExternal(ctx, exe, workdir, t, packets, gap, timeout)
		},
	}); err != nil {
		return "", err // refused: the caller logs it and skips this plugin
	}
	return m.Type, nil
}

type probeRequest struct {
	Protocol  string         `json:"protocol"`
	Type      string         `json:"type"`
	Host      string         `json:"host"`
	Port      int            `json:"port"`
	Packets   int            `json:"packets"`
	GapMs     int            `json:"gap_ms"`
	TimeoutMs int            `json:"timeout_ms"`
	Options   map[string]any `json:"options"`
}

// runExternal runs one round: the request goes in on standard input, the answer
// comes back on standard output and is judged by parseRoundResult. Nothing is on
// the command line and the environment holds only a fixed PATH, so a value can
// never become part of a shell command line or be read from ps.
func runExternal(ctx context.Context, exe, workdir string, t TargetCfg, packets int, gap, timeout time.Duration) (Round, error) {
	var zero Round
	select {
	case extSlots <- struct{}{}:
		defer func() { <-extSlots }()
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	opts := t.Options
	if opts == nil {
		opts = map[string]any{}
	}
	req, err := json.Marshal(probeRequest{Protocol: probeProtocol, Type: t.Type, Host: t.Host, Port: t.Port,
		Packets: packets, GapMs: int(gap / time.Millisecond), TimeoutMs: int(timeout / time.Millisecond), Options: opts})
	if err != nil {
		return zero, err
	}
	deadline := time.Duration(packets)*(gap+timeout) + runMargin
	cctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	cmd := exec.CommandContext(cctx, exe)
	cmd.Dir = workdir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Stdin = bytes.NewReader(req)
	out := &capped{max: maxResultBytes}
	errOut := &capped{max: maxStderrLog}
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so the whole tree can be stopped
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second // a child holding the pipes open cannot stall the round

	started := time.Now()
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return zero, ctx.Err() // the target was stopped
	}
	if cctx.Err() != nil {
		return zero, fmt.Errorf("prober did not answer within %s and was stopped", deadline.Round(time.Second))
	}
	if runErr != nil {
		return zero, fmt.Errorf("prober failed: %v%s", runErr, stderrNote(errOut))
	}
	if out.over {
		return zero, coded(codeBadResult, "result is larger than %d bytes", maxResultBytes)
	}
	r, err := parseRoundResult(out.buf.Bytes(), started.Unix(), packets)
	if err != nil {
		return zero, fmt.Errorf("prober answered badly: %w%s", err, stderrNote(errOut))
	}
	return r, nil
}

func stderrNote(c *capped) string {
	s := strings.Join(strings.Fields(c.buf.String()), " ")
	if s == "" {
		return ""
	}
	return " (stderr: " + s + ")"
}

// capped keeps at most max bytes and notes when more was written, so a prober
// cannot fill memory by printing forever. Writes always succeed: the process is
// not blocked on a full pipe, it is just not believed.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

var _ io.Writer = (*capped)(nil)
