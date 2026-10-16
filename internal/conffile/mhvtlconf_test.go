package conffile

import (
	"strings"
	"testing"
)

// TestMhvtlConfCReader covers MHVTL_CONFIG_PATH lines mhVTL's sscanf reads
// differently from the shell: each is a Problem, Set refuses them, and the
// file round-trips.
func TestMhvtlConfCReader(t *testing.T) {
	cases := map[string]string{
		"MHVTL_CONFIG_PATH=\"/etc/mhvtl\"\n":           "quoted",
		"export MHVTL_CONFIG_PATH=/etc/mhvtl\n":        "export",
		"  MHVTL_CONFIG_PATH=/etc/mhvtl\n":             "indented",
		"MHVTL_CONFIG_PATH=/etc/mhvtl # where\n":       "text after the value",
		"MHVTL_CONFIG_PATH=/etc/mhvtl \n":              "text after the value",
		"MHVTL_CONFIG_PATH=/etc/mhvtl\r\n":             "CRLF",
		"MHVTL_CONFIG_PATH=/a\nMHVTL_CONFIG_PATH=/b\n": "the shell the last",
		"MHVTL_CONFIG_PATH=/a\x00b\n":                  "NUL",
	}
	for in, want := range cases {
		c := ParseMhvtlConf([]byte(in))
		if string(c.Render()) != in {
			t.Errorf("%q: not round-tripped", in)
		}
		if len(c.Problems) != 1 || !strings.Contains(c.Problems[0].Msg, want) || !c.Problems[0].Ambiguous {
			t.Errorf("%q: problems %v, want one %q", in, c.Problems, want)
		}
		if want == "NUL" {
			continue
		}
		if err := c.Set("MHVTL_CONFIG_PATH", "/srv/mhvtl"); err == nil {
			t.Errorf("%q: Set accepted", in)
		}
		if string(c.Render()) != in {
			t.Errorf("%q: refused Set changed the file", in)
		}
	}

	// The same shapes on a key only the shell reads are not mhVTL's concern.
	if c := ParseMhvtlConf([]byte("export CAPACITY=\"500\" # MB\n")); len(c.Problems) != 0 {
		t.Errorf("shell-only key flagged: %v", c.Problems)
	}
	// A plain line is fine, and a new one is refused only in a CRLF file.
	c := ParseMhvtlConf([]byte("MHVTL_CONFIG_PATH=/etc/mhvtl\n"))
	if len(c.Problems) != 0 {
		t.Errorf("plain line flagged: %v", c.Problems)
	}
	if err := c.Set("MHVTL_CONFIG_PATH", "/srv/mhvtl"); err != nil || string(c.Render()) != "MHVTL_CONFIG_PATH=/srv/mhvtl\n" {
		t.Errorf("Set: %v, %q", err, c.Render())
	}
	if err := ParseMhvtlConf([]byte("CAPACITY=500\r\n")).Set("MHVTL_CONFIG_PATH", "/x"); err == nil {
		t.Error("new MHVTL_CONFIG_PATH in a CRLF file accepted")
	}
}
