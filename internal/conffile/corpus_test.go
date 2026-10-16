package conffile

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped corpus is what mhVTL's etc/Makefile builds with its default
// paths (config.mk: MHVTL_HOME_PATH /var/lib/mhvtl, MHVTL_CONFIG_PATH
// /etc/mhvtl).
const (
	corpusHome = "/var/lib/mhvtl"
	corpusConf = "/etc/mhvtl"
)

// mhvtlEtc locates mhVTL's etc/: $MHVTL_SRC, else the sibling submodule of
// the superproject.
func mhvtlEtc(t *testing.T) string {
	t.Helper()
	src := os.Getenv("MHVTL_SRC")
	if src == "" {
		src = "../../../mhvtl"
	}
	abs, err := filepath.Abs(filepath.Join(src, "etc"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "generate_device_conf.in")); err != nil {
		t.Skipf("mhvtl submodule not found at %s: %v", abs, err)
	}
	return abs
}

// substitute does what etc/Makefile's "%: %.in" sed rule does.
func substitute(b []byte) []byte {
	s := strings.NewReplacer("@CONF_PATH@", corpusConf, "@HOME_PATH@", corpusHome).Replace(string(b))
	return []byte(s)
}

// TestCorpusMatchesMhvtl fails when testdata/roundtrip/shipped drifts from
// what mhVTL's etc/ generates, so the round-trip corpus always covers the
// files mhVTL really installs. It reruns the generators the way etc/Makefile
// does (bash ./generate_device_conf --force --home-dir=... --override-home,
// then bash ./generate_library_contents --force --config=.).
func TestCorpusMatchesMhvtl(t *testing.T) {
	etc := mhvtlEtc(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is needed to run mhVTL's config generators: %v", err)
	}
	work := t.TempDir()
	for _, g := range []string{"generate_device_conf", "generate_library_contents"} {
		in, err := os.ReadFile(filepath.Join(etc, g+".in"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, g), substitute(in), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"./generate_device_conf", "--force", "--home-dir=" + corpusHome, "--override-home"},
		{"./generate_library_contents", "--force", "--config=."},
	} {
		cmd := exec.Command(bash, args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, out)
		}
	}
	sample, err := os.ReadFile(filepath.Join(etc, "library_contents.sample"))
	if err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(filepath.Join(etc, "mhvtl.conf.in"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		"library_contents.sample": sample,
		"mhvtl.conf":              substitute(conf),
	}
	for _, name := range []string{"device.conf", "library_contents.10", "library_contents.30"} {
		b, err := os.ReadFile(filepath.Join(work, name))
		if err != nil {
			t.Fatal(err)
		}
		want[name] = b
	}

	shipped, err := filepath.Glob("testdata/roundtrip/shipped/*")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped) != len(want) {
		t.Errorf("testdata/roundtrip/shipped has %d files, want %d: %v", len(shipped), len(want), shipped)
	}
	for name, w := range want {
		got, err := os.ReadFile(filepath.Join("testdata/roundtrip/shipped", name))
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		if !bytes.Equal(got, w) {
			t.Errorf("testdata/roundtrip/shipped/%s drifted from mhVTL's etc/ (%s); refresh it from the generated file", name, etc)
		}
	}
}
