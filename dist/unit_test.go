// Package dist holds the files the packages install. Its tests pin the
// confinement of mhvtl-control.service, so that a later edit cannot loosen it
// unnoticed.
package dist

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
)

// serviceSection returns the [Service] settings of a unit file, each key
// with all its values in file order (a key may be repeated).
func serviceSection(t *testing.T, path string) map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	kv := map[string][]string{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") {
			in = line == "[Service]"
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && in {
			kv[strings.TrimSpace(k)] = append(kv[strings.TrimSpace(k)], strings.TrimSpace(v))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return kv
}

// words splits every value of key into its space-separated entries.
func words(kv map[string][]string, key string) []string {
	var w []string
	for _, v := range kv[key] {
		w = append(w, strings.Fields(v)...)
	}
	return w
}

func TestServiceConfinement(t *testing.T) {
	kv := serviceSection(t, "mhvtl-control.service")

	exact := map[string]string{
		"EnvironmentFile":         "-/etc/mhvtl-control/mhvtl-control.env",
		"ProtectSystem":           "full",
		"CapabilityBoundingSet":   "CAP_DAC_OVERRIDE",
		"NoNewPrivileges":         "yes",
		"RestrictAddressFamilies": "AF_UNIX AF_INET AF_INET6",
	}
	for k, want := range exact {
		if got := kv[k]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want exactly %q", k, got, want)
		}
	}
	if v, ok := kv["ReadWritePaths"]; ok {
		t.Errorf("ReadWritePaths = %q, want none", v)
	}

	// Entries compare whole, so a '-' prefix (skip if missing) fails them.
	contains := map[string][]string{
		"InaccessiblePaths":   {"/run/dbus", "/run/mhvtl-control-apply", "/var/lib/mhvtl-control-apply"},
		"TemporaryFileSystem": {"/run/systemd:ro"},
		"BindReadOnlyPaths":   {"/run/systemd/journal"},
	}
	for k, wants := range contains {
		got := words(kv, k)
		for _, want := range wants {
			if !slices.Contains(got, want) {
				t.Errorf("%s = %q, want it to contain %q", k, got, want)
			}
		}
	}

	// The daemon reads its options and mhVTL's configuration: no mask may
	// cover them.
	masks := append(words(kv, "InaccessiblePaths"), words(kv, "TemporaryFileSystem")...)
	for _, read := range []string{"/etc/mhvtl-control/mhvtl-control.env", "/etc/mhvtl/device.conf"} {
		for _, m := range masks {
			m = strings.TrimPrefix(strings.TrimPrefix(m, "-"), "+")
			m, _, _ = strings.Cut(m, ":")
			if read == m || strings.HasPrefix(read, strings.TrimSuffix(m, "/")+"/") {
				t.Errorf("mask %q covers %s, which the daemon reads", m, read)
			}
		}
	}
}

func TestTmpfilesCreatesApplyDir(t *testing.T) {
	b, err := os.ReadFile("mhvtl-control.tmpfiles")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "d" && f[1] == "/run/mhvtl-control-apply" {
			if f[2] != "0700" {
				t.Errorf("/run/mhvtl-control-apply mode = %s, want 0700", f[2])
			}
			return
		}
	}
	t.Error("mhvtl-control.tmpfiles does not create /run/mhvtl-control-apply (type d)")
}
