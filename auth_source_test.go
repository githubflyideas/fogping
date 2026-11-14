package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveAuthSources(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	os.WriteFile(pwFile, []byte("alice-pw\nbob-pw\n"), 0600)

	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    map[string]string
		cliPw   bool
		wantErr string
	}{
		{"none", nil, nil, nil, false, ""},
		{"cli", []string{"user=a", "passwd=x"}, nil, map[string]string{"a": "x"}, true, ""},
		{"env", nil, map[string]string{"FOGPING_USER": "a", "FOGPING_PASSWD": "x"}, map[string]string{"a": "x"}, false, ""},
		{"cli user + env password", []string{"user=a"}, map[string]string{"FOGPING_PASSWD": "x"}, map[string]string{"a": "x"}, false, ""},
		{"passwd-file", []string{"user=alice,bob", "passwd-file=" + pwFile}, nil, map[string]string{"alice": "alice-pw", "bob": "bob-pw"}, false, ""},
		{"env file", nil, map[string]string{"FOGPING_USER": "alice,bob", "FOGPING_PASSWD_FILE": pwFile}, map[string]string{"alice": "alice-pw", "bob": "bob-pw"}, false, ""},
		{"cli beats env", []string{"user=a", "passwd=x"}, map[string]string{"FOGPING_USER": "b", "FOGPING_PASSWD": "y"}, map[string]string{"a": "x"}, true, ""},
		{"both env", nil, map[string]string{"FOGPING_USER": "a", "FOGPING_PASSWD": "x", "FOGPING_PASSWD_FILE": pwFile}, nil, false, "not both"},
		{"both cli", []string{"user=a", "passwd=x", "passwd-file=" + pwFile}, nil, nil, true, "not both"},
		{"missing file", []string{"user=a", "passwd-file=" + filepath.Join(dir, "nope")}, nil, nil, false, "passwd-file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, cliPw, err := resolveAuth(c.args, envOf(c.env))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cliPw != c.cliPw {
				t.Errorf("cliPasswd = %v, want %v", cliPw, c.cliPw)
			}
			if len(got) != len(c.want) {
				t.Fatalf("users = %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("users = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// The container entrypoint must not turn the password into an argument.
func TestEntrypointKeepsPasswordOutOfArgv(t *testing.T) {
	b, err := os.ReadFile("docker/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "passwd=") {
			t.Fatalf("entrypoint puts the password on the command line: %s", line)
		}
	}
}
