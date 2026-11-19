package main

import (
	"os"
	"strings"
	"testing"
)

// The test binary stands in for fogping when a prober is launched (external.go
// starts os.Executable() with probeExecArg), so it must act as the launcher too.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == probeExecArg {
		probeExec(os.Args[2:])
		return
	}
	os.Exit(m.Run())
}

// A prober runs with the limits set: it can see them, and cannot raise them.
func TestProberRunsWithResourceLimits(t *testing.T) {
	root := t.TempDir()
	plugin(t, root, "limits", "", `
nofile=$(ulimit -n); fsize=$(ulimit -f); core=$(ulimit -c); mem=$(ulimit -v)
echo "{\"protocol\":\"fogping-probe/1\",\"error\":\"nofile=$nofile fsize=$fsize core=$core mem=$mem\"}"
`, 0o755)
	loadProbes(root)
	r, err := runPlugin(t, "limits", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// bash counts ulimit -f in 512-byte blocks and -v in 1024-byte blocks:
	// 32768 x 512 = 16 MiB, 1048576 x 1024 = 1 GiB.
	for _, want := range []string{"nofile=256", "fsize=32768", "core=0", "mem=1048576"} {
		if !strings.Contains(r.E, want) {
			t.Errorf("missing %s in %q", want, r.E)
		}
	}
}

// A prober that tries to fill the disk is stopped at the file-size limit, and the
// round is a failure, not a measurement.
func TestProberCannotFillTheDisk(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir() + "/big"
	plugin(t, root, "filler", "", `
head -c 40000000 /dev/zero > '`+out+`' || exit 1
echo '{"protocol":"fogping-probe/1","error":"wrote it all"}'
`, 0o755)
	loadProbes(root)
	if r, err := runPlugin(t, "filler", 1, nil); err == nil {
		t.Fatalf("a prober wrote 40 MB: %+v", r)
	}
	if fi, err := os.Stat(out); err == nil && fi.Size() > 16<<20 {
		t.Fatalf("file grew to %d bytes", fi.Size())
	}
}
