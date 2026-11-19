package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The official probers in plugins/ are tested here the way fogping uses them: they
// are installed with install.sh, loaded by loadProbes, and run through the real
// round function, so the manifest, the stdin/stdout contract and the strict
// judgement of their answers are all exercised together. A prober whose language
// toolchain is missing is skipped, not failed.

func installPlugin(t *testing.T, name string, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dest := t.TempDir()
	cmd := exec.Command("plugins/install.sh", "-d", dest, name)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install %s: %v\n%s", name, err, out)
	}
	if got := loadProbes(dest); len(got) != 1 || got[0] != name {
		t.Fatalf("loaded %v, want [%s]", got, name)
	}
	t.Cleanup(func() { unregisterProber(name); cleanupProbes() })
}

func pluginRound(t *testing.T, typ, host string, port int, opts map[string]any) Round {
	t.Helper()
	tg := TargetCfg{Type: typ, Host: host, Port: port, Options: opts}
	if err := normalizeTarget(&tg); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	p, _ := proberFor(typ)
	r, err := p.round(context.Background(), tg, 3, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("%s round: %v", typ, err)
	}
	return r
}

func wantAllReceived(t *testing.T, typ string, r Round) {
	t.Helper()
	if r.E != "" || r.S != 3 || r.R != 3 || len(r.MS) != 3 {
		t.Fatalf("%s: %+v", typ, r)
	}
}

func listenTCP(t *testing.T, handle func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); handle(c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestPluginHTTP(t *testing.T) {
	installPlugin(t, "http", "go", "make")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	host, ps, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	wantAllReceived(t, "http", pluginRound(t, "http", host, port, map[string]any{"scheme": "http"}))
	// Only an HTTPS URL with a valid certificate may succeed: the test server's own
	// certificate is not trusted, so every probe is lost, not accepted.
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tls.Close()
	host, ps, _ = net.SplitHostPort(tls.Listener.Addr().String())
	port, _ = strconv.Atoi(ps)
	if r := pluginRound(t, "http", host, port, nil); r.R != 0 {
		t.Fatalf("an untrusted certificate was accepted: %+v", r)
	}
}

func TestPluginDNS(t *testing.T) {
	installPlugin(t, "dns", "python3")
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			// Reply: same ID and question, QR+RD+RA flags, one answer counted.
			reply := append([]byte{}, buf[:n]...)
			reply[2], reply[3] = 0x81, 0x80
			reply[6], reply[7] = 0, 1
			pc.WriteTo(reply, peer)
		}
	}()
	port := pc.LocalAddr().(*net.UDPAddr).Port
	wantAllReceived(t, "dns", pluginRound(t, "dns", "127.0.0.1", port, map[string]any{"query": "example.com"}))
}

func TestPluginSMTP(t *testing.T) {
	installPlugin(t, "smtp", "ruby")
	port := listenTCP(t, func(c net.Conn) {
		c.Write([]byte("220 fake ESMTP\r\n"))
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			switch {
			case strings.HasPrefix(sc.Text(), "EHLO"):
				c.Write([]byte("250-fake\r\n250 8BITMIME\r\n"))
			case strings.HasPrefix(sc.Text(), "QUIT"):
				c.Write([]byte("221 bye\r\n"))
				return
			}
		}
	})
	wantAllReceived(t, "smtp", pluginRound(t, "smtp", "127.0.0.1", port, nil))
}

func TestPluginSSH(t *testing.T) {
	installPlugin(t, "ssh", "bash", "timeout")
	port := listenTCP(t, func(c net.Conn) {
		c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		c.Read(make([]byte, 1))
	})
	wantAllReceived(t, "ssh", pluginRound(t, "ssh", "127.0.0.1", port, nil))
}

func TestPluginTraceroute(t *testing.T) {
	installPlugin(t, "traceroute", "cargo", "make")
	if _, err := os.Stat("/proc/net/udp"); err != nil {
		t.Skip("Linux only")
	}
	wantAllReceived(t, "traceroute", pluginRound(t, "traceroute", "127.0.0.1", 0, nil))
}
