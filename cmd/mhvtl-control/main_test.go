package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"":          false, // ":8080" binds the wildcard, not loopback
		"localhost": true,
		"127.0.0.1": true,
		"127.0.0.5": true,
		"::1":       true,
		"0.0.0.0":   false,
		"10.2.0.1":  false,
		"::":        false,
	}
	for host, want := range cases {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestRequireLoopbackHost(t *testing.T) {
	cases := []struct {
		host, token string
		want        bool
	}{
		{"127.0.0.1", "", true},
		{"localhost", "", true},
		{"::1", "", true},
		{"127.0.0.1", "tok", false},
		{"0.0.0.0", "", false},
		{"", "", false},
		{"192.168.1.10", "", false},
	}
	for _, c := range cases {
		if got := requireLoopbackHost(c.host, c.token); got != c.want {
			t.Errorf("requireLoopbackHost(%q,%q) = %v, want %v", c.host, c.token, got, c.want)
		}
	}
}

func TestBindRefused(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		token    string
		insecure bool
		want     bool
	}{
		{"non-loopback no token no insecure", "0.0.0.0", "", false, true},
		{"non-loopback with token", "0.0.0.0", "tok", false, false},
		{"non-loopback with insecure", "0.0.0.0", "", true, false},
		{"loopback no token", "127.0.0.1", "", false, false},
		{"wildcard empty host no token refused", "", "", false, true},
		{"wildcard empty host with token", "", "tok", false, false},
		{"wildcard empty host with insecure", "", "", true, false},
		{"localhost no token", "localhost", "", false, false},
	}
	for _, c := range cases {
		if got := bindRefused(c.host, c.token, c.insecure); got != c.want {
			t.Errorf("%s: bindRefused(%q,%q,%v) = %v, want %v", c.name, c.host, c.token, c.insecure, got, c.want)
		}
	}
}

func TestParseChangerFlag(t *testing.T) {
	m := map[int]string{}
	for _, v := range []string{"10=/dev/sg3", "30=/dev/sg7"} {
		if err := parseChangerFlag(m, v); err != nil {
			t.Fatalf("parseChangerFlag(%q): %v", v, err)
		}
	}
	if m[10] != "/dev/sg3" || m[30] != "/dev/sg7" || len(m) != 2 {
		t.Errorf("overrides = %v", m)
	}
	for _, bad := range []string{"/dev/sg3", "10=", "x=/dev/sg3", "-1=/dev/sg3"} {
		if err := parseChangerFlag(map[int]string{}, bad); err == nil {
			t.Errorf("parseChangerFlag(%q) accepted", bad)
		}
	}
}

func TestLegacyOptions(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "mhvtl-control", "mhvtl-control.env")
	legacy := filepath.Join(dir, "mhvtl", "mhvtl-control.env")
	noEnv := func(string) string { return "" }

	// Neither file: nothing to read.
	if _, _, ok, err := legacyOptions(cur, legacy, noEnv); ok || err != nil {
		t.Fatalf("no files: ok=%v err=%v", ok, err)
	}

	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `# Options for mhvtl-control.service.
MHVTL_CONTROL_OPTS="--listen 0.0.0.0:8443  --tls-cert /etc/mhvtl/c.crt --tls-key /etc/mhvtl/c.key"

; a comment
MHVTL_CONTROL_API_TOKEN='s3cret'
#MHVTL_CONTROL_API_TOKEN=commented-out
`
	if err := os.WriteFile(legacy, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	args, token, ok, err := legacyOptions(cur, legacy, noEnv)
	if err != nil || !ok {
		t.Fatalf("legacy only: ok=%v err=%v", ok, err)
	}
	if got, want := strings.Join(args, "|"), "--listen|0.0.0.0:8443|--tls-cert|/etc/mhvtl/c.crt|--tls-key|/etc/mhvtl/c.key"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if token != "s3cret" {
		t.Errorf("token = %q, want s3cret", token)
	}

	// systemd passed options from the current file, or the environment
	// carries them: the legacy file is ignored.
	for _, k := range []string{"MHVTL_CONTROL_OPTS", "MHVTL_CONTROL_API_TOKEN"} {
		env := func(name string) string {
			if name == k {
				return "set"
			}
			return ""
		}
		if _, _, ok, _ := legacyOptions(cur, legacy, env); ok {
			t.Errorf("%s set: legacy file read", k)
		}
	}

	// The current file exists: the legacy one is ignored, even when the
	// current one sets nothing.
	if err := os.MkdirAll(filepath.Dir(cur), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cur, []byte("# empty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := legacyOptions(cur, legacy, noEnv); ok {
		t.Error("current file present: legacy file read")
	}
}
