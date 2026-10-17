// Package dist holds the files the packages install. Its tests pin the
// confinement of mhvtl-control.service and of the mhvtl-control-apply units,
// so that a later edit cannot loosen them unnoticed.
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
	return section(t, path, "Service")
}

// section returns the settings of one section of a unit file.
func section(t *testing.T, path, name string) map[string][]string {
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
			in = line == "["+name+"]"
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

// exactly checks that each key is set once, to want.
func exactly(t *testing.T, unit string, kv map[string][]string, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got := kv[k]; len(got) != 1 || got[0] != w {
			t.Errorf("%s: %s = %q, want exactly %q", unit, k, got, w)
		}
	}
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
	want := map[string]string{"/run/mhvtl-control-apply": "0700", "/run/mhvtl-control": "0755"}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "d" {
			if mode, ok := want[f[1]]; ok {
				if f[2] != mode {
					t.Errorf("%s mode = %s, want %s", f[1], f[2], mode)
				}
				delete(want, f[1])
			}
		}
	}
	for dir := range want {
		t.Errorf("mhvtl-control.tmpfiles does not create %s (type d)", dir)
	}
}

// helperSandbox is the confinement the helper and its boot recovery share
// (plan 3.1; spec, "Privilege split"; Phase 0 amendment 7 for the
// capability).
var helperSandbox = map[string]string{
	"CapabilityBoundingSet":    "CAP_CHOWN",
	"NoNewPrivileges":          "yes",
	"ProtectSystem":            "strict",
	"StateDirectory":           "mhvtl-control-apply",
	"StateDirectoryMode":       "0700",
	"RuntimeDirectory":         "mhvtl-control-apply",
	"RuntimeDirectoryMode":     "0700",
	"RuntimeDirectoryPreserve": "yes",
	"InaccessiblePaths":        "-/etc/mhvtl/mhvtl-control.env -/etc/mhvtl-control",
	"ProtectHome":              "yes",
	"PrivateTmp":               "yes",
	"ProtectKernelModules":     "yes",
	"ProtectKernelTunables":    "yes",
	"ProtectKernelLogs":        "yes",
	"ProtectControlGroups":     "yes",
	"ProtectClock":             "yes",
	"ProtectHostname":          "yes",
	"RestrictAddressFamilies":  "AF_UNIX",
	"RestrictNamespaces":       "yes",
	"RestrictRealtime":         "yes",
	"RestrictSUIDSGID":         "yes",
	"LockPersonality":          "yes",
	"MemoryDenyWriteExecute":   "yes",
	"SystemCallArchitectures":  "native",
	"SystemCallFilter":         "@system-service",
	"UMask":                    "0077",
}

// helperExceptions are the helper's departures from the spec's sandbox
// (or from systemd's defaults); each must carry a comment saying why.
var helperExceptions = []string{
	"CapabilityBoundingSet", "ReadWritePaths", "RuntimeDirectoryPreserve",
	"InaccessiblePaths", "PrivateDevices", "DevicePolicy", "DeviceAllow",
	"RuntimeMaxSec", "TimeoutStopSec", "StandardInput",
}

func TestApplyServiceConfinement(t *testing.T) {
	const unit = "mhvtl-control-apply@.service"
	kv := serviceSection(t, unit)
	exactly(t, unit, kv, helperSandbox)
	exactly(t, unit, kv, map[string]string{
		"ExecStart":      "/usr/libexec/mhvtl-control/mhvtl-control-apply --config-dir=/etc/mhvtl",
		"StandardInput":  "socket",
		"StandardOutput": "journal",
		"ReadWritePaths": "/etc/mhvtl /run/mhvtl-control",
		// D3: SG_IO to the changers, behind the helper's own opcode and
		// device-type allowlist.
		"PrivateDevices": "no",
		"DevicePolicy":   "closed",
		"DeviceAllow":    "char-sg rw",
		"RuntimeMaxSec":  "1800",
		"TimeoutStopSec": "60",
	})
	for _, k := range []string{"User", "DynamicUser", "AmbientCapabilities", "Type"} {
		if v, ok := kv[k]; ok {
			t.Errorf("%s: %s = %q, want unset", unit, k, v)
		}
	}
	commented(t, unit, helperExceptions)
	if v := section(t, unit, "Install"); len(v) != 0 {
		t.Errorf("%s: [Install] %v: the template is started by its socket only", unit, v)
	}
}

func TestApplySocket(t *testing.T) {
	const unit = "mhvtl-control-apply.socket"
	exactly(t, unit, section(t, unit, "Socket"), map[string]string{
		"ListenStream":   "/run/mhvtl-control-apply.sock",
		"SocketMode":     "0600",
		"Accept":         "yes",
		"MaxConnections": "4",
		"RemoveOnStop":   "yes",
	})
	exactly(t, unit, section(t, unit, "Install"), map[string]string{"WantedBy": "sockets.target"})
	// The daemon masks the helper's directories, so the socket must live
	// outside them.
	for _, d := range []string{"/run/mhvtl-control-apply/", "/var/lib/mhvtl-control-apply/"} {
		if strings.HasPrefix("/run/mhvtl-control-apply.sock", d) {
			t.Errorf("socket under %s", d)
		}
	}
}

func TestApplyRecoverUnit(t *testing.T) {
	const unit = "mhvtl-control-apply-recover.service"
	kv := serviceSection(t, unit)
	exactly(t, unit, kv, helperSandbox)
	exactly(t, unit, kv, map[string]string{
		"Type":           "oneshot",
		"ExecStart":      "/usr/libexec/mhvtl-control/mhvtl-control-apply --config-dir=/etc/mhvtl --recover-boot",
		"ReadWritePaths": "/etc/mhvtl",
		"PrivateDevices": "yes",
	})
	for _, k := range []string{"DeviceAllow", "DevicePolicy"} {
		if v, ok := kv[k]; ok {
			t.Errorf("%s: %s = %q: boot recovery opens no device", unit, k, v)
		}
	}
	u := section(t, unit, "Unit")
	exactly(t, unit, u, map[string]string{
		"ConditionPathExists": "/var/lib/mhvtl-control-apply/intent.json",
		"DefaultDependencies": "no",
		"After":               "local-fs.target",
	})
	before := strings.Join(u["Before"], " ")
	for _, w := range []string{"mhvtl.target", "mhvtl-media.service"} {
		if !slices.Contains(strings.Fields(before), w) {
			t.Errorf("%s: Before=%q, want %s in it", unit, before, w)
		}
	}
	exactly(t, unit, section(t, unit, "Install"), map[string]string{"WantedBy": "mhvtl.target"})
	commented(t, unit, []string{"CapabilityBoundingSet", "ReadWritePaths", "RuntimeDirectoryPreserve", "InaccessiblePaths"})
}

// TestRecoverDropIns: mhVTL's instances are ordered after boot recovery,
// and never pull it in (a pull-in would run it during a live apply).
func TestRecoverDropIns(t *testing.T) {
	for _, path := range []string{
		"vtllibrary@.service.d/50-mhvtl-control-apply-recover.conf",
		"vtltape@.service.d/50-mhvtl-control-apply-recover.conf",
	} {
		u := section(t, path, "Unit")
		if len(u) != 1 {
			t.Errorf("%s: [Unit] %v, want After= alone", path, u)
		}
		exactly(t, path, u, map[string]string{"After": "mhvtl-control-apply-recover.service"})
		for _, s := range []string{"Service", "Install"} {
			if v := section(t, path, s); len(v) != 0 {
				t.Errorf("%s: [%s] %v", path, s, v)
			}
		}
	}
}

// commented fails for each key whose line is not directly under a comment,
// or under another listed key that is (one comment may explain a group).
// Whether the key is set at all is exactly's to check.
func commented(t *testing.T, path string, keys []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	ok := map[int]bool{}
	for i, l := range lines {
		k, _, isKV := strings.Cut(l, "=")
		if !isKV || !slices.Contains(keys, k) {
			continue
		}
		prev := ""
		if i > 0 {
			prev = lines[i-1]
		}
		ok[i] = strings.HasPrefix(prev, "#") || ok[i-1]
		if !ok[i] {
			t.Errorf("%s:%d: %s has no comment saying why", path, i+1, k)
		}
	}
}
