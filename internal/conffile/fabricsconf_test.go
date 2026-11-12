package conffile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readFabricsSample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/fabrics/fabrics.conf.sample")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustFabrics(t *testing.T, s string) *FabricsConf {
	t.Helper()
	c := ParseFabricsConf([]byte(s))
	if len(c.Problems) > 0 {
		t.Fatalf("problems in a valid file: %v\n%s", c.Problems, s)
	}
	return c
}

func strp(s string) *string { return &s }

// TestFabricsSampleMatchesMhvtl fails when the fixture drifts from mhVTL's
// etc/fabrics.conf.sample, when that tree is at hand.
func TestFabricsSampleMatchesMhvtl(t *testing.T) {
	etc := mhvtlEtc(t)
	want, err := os.ReadFile(filepath.Join(etc, "fabrics.conf.sample"))
	if err != nil {
		t.Skipf("mhVTL tree has no fabrics.conf.sample: %v", err)
	}
	if got := readFabricsSample(t); got != string(want) {
		t.Errorf("testdata/fabrics/fabrics.conf.sample drifted from %s", etc)
	}
}

func TestFabricsRoundTrip(t *testing.T) {
	sample := readFabricsSample(t)
	valid := map[string]string{
		"sample":            sample,
		"sample CRLF":       strings.ReplaceAll(sample, "\n", "\r\n"),
		"sample no final":   strings.TrimSuffix(sample, "\n"),
		"empty":             "",
		"only comments":     "# nothing yet\n\n#\n",
		"blank in records":  "iSCSI:\n\n   \n Portal: 192.0.2.1\n\t\n\niSCSI target: 10\n\n Initiator name: iqn.2024-01.com.example:a\n",
		"odd case":          "version: 1\nIscsi :\n\tPORTAL :  192.0.2.1:3261  \n BASE   name: iqn.2024-01.com.example:vtl\nISCSI   TARGET: 10\n  initiator   NAME: iqn.2024-01.com.example:a\n  chap name: PBS\n  chap secret: Secret123456\n",
		"trailing comments": "VERSION: 1 # format\niSCSI: # all targets\n Portal: 192.0.2.1 # storage\n # indented comment\niSCSI target: 10 # lib 10\n Initiator name: iqn.2024-01.com.example:a\t# PBS\n CHAP name: pbs\n CHAP secret: abcdefghijkl#x\n",
	}
	for name, in := range valid {
		t.Run(name, func(t *testing.T) {
			c := mustFabrics(t, in)
			if got := c.Render(); !bytes.Equal(got, []byte(in)) {
				t.Errorf("render differs\ngot:  %q\nwant: %q", got, in)
			}
		})
	}
	// Files with problems round-trip too.
	for _, in := range []string{"FC:\n bogus\n", "iSCSI:\n Portal 1.2.3.4\r\n", " field: outside\nx", "a\x00b\n"} {
		c := ParseFabricsConf([]byte(in))
		if len(c.Problems) == 0 {
			t.Errorf("%q: no problems", in)
		}
		if string(c.Render()) != in {
			t.Errorf("%q: not round-tripped", in)
		}
	}
}

// Mirrors test_parse_full in mhvtl tests/test_fabrics.c.
func TestFabricsParseFull(t *testing.T) {
	c := mustFabrics(t, "VERSION: 1\n"+
		"# comment\n"+
		"iSCSI:\n"+
		" Portal: 192.0.2.10:3261\n"+
		" Portal: [2001:db8::1]\n"+
		" Base name: iqn.2024-01.com.example:vtl\n"+
		"\n"+
		"iSCSI target: 10\n"+
		" Initiator name: iqn.2024-01.com.example:pbs1   # PBS\n"+
		" Initiator name: iqn.2024-01.com.example:pbs2\n"+
		" CHAP name: pbs\n"+
		" CHAP secret: abcdefghijkl#x\n"+
		" Mutual CHAP name: vtl\n"+
		" Mutual CHAP secret: mnopqrstuvwx\n")
	portals, base, ok := c.ISCSI()
	if !ok || !reflect.DeepEqual(portals, []string{"192.0.2.10:3261", "[2001:db8::1]"}) || base != "iqn.2024-01.com.example:vtl" {
		t.Errorf("ISCSI() = %v %q %v", portals, base, ok)
	}
	want := []FabricsTarget{{
		ID: 10, Line: 8,
		Initiators: []string{"iqn.2024-01.com.example:pbs1", "iqn.2024-01.com.example:pbs2"},
		CHAPName:   "pbs", HasCHAP: true, MutualCHAPName: "vtl", HasMutualCHAP: true,
	}}
	if got := c.Targets(); !reflect.DeepEqual(got, want) {
		t.Errorf("Targets() = %+v, want %+v", got, want)
	}
	if !c.HasSecret() {
		t.Error("HasSecret false")
	}
	if !c.records[2].hasAuth("abcdefghijkl#x") {
		t.Error("'#' without a blank before it is part of the value")
	}
}

func (r fabRecord) hasAuth(secret string) bool { return r.auth[1] == secret }

// Mirrors test_parse_field_names_are_loose.
func TestFabricsLooseNames(t *testing.T) {
	c := mustFabrics(t, "iscsi:\n\tportal:\t192.0.2.1\nISCSI   TARGET: 10\n  initiator   NAME: iqn.2024-01.com.example:a\n  chap name: PBS\n  chap secret: Secret123456\n")
	ts := c.Targets()
	if len(ts) != 1 || ts[0].CHAPName != "PBS" || !ts[0].HasCHAP {
		t.Errorf("Targets() = %+v", ts)
	}
	if ParseFabricsConf([]byte("iSCSI:\n")).HasSecret() {
		t.Error("HasSecret on a file without secrets")
	}
}

func TestFabricsParseErrors(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	var portals, inits, targets strings.Builder
	portals.WriteString("iSCSI:\n")
	for i := 0; i < 17; i++ {
		portals.WriteString(" Portal: 192.0.2.1:" + itoa(3000+i) + "\n")
	}
	inits.WriteString("iSCSI:\niSCSI target: 10\n")
	for i := 0; i < 33; i++ {
		inits.WriteString(" Initiator name: iqn.2024-01.com.example:i" + itoa(i) + "\n")
	}
	targets.WriteString("iSCSI:\n")
	for i := 0; i < 33; i++ {
		targets.WriteString("iSCSI target: " + itoa(i) + "\n")
	}
	const tgt = "iSCSI:\niSCSI target: 10\n"
	cases := []struct {
		name, in string
		line     int
		msg      string
	}{
		// test_parse_errors, in order
		{"typo in field", "iSCSI:\n Portl: 1.2.3.4\n", 2, "unknown iSCSI field 'portl'"},
		{"typo in initiator field", tgt + " Initator name: iqn.2024-01.x:y\n", 3, "unknown iSCSI target field 'initator name'"},
		{"unknown record", "FC:\n", 1, "unknown record 'fc'"},
		{"port 0", "iSCSI:\n Portal: 1.2.3.4:0\n", 2, "is not ADDRESS[:PORT]"},
		{"port too large", "iSCSI:\n Portal: 1.2.3.4:70000\n", 2, "is not ADDRESS[:PORT]"},
		{"host name portal", "iSCSI:\n Portal: host.example:3260\n", 2, "is not ADDRESS[:PORT]"},
		{"IPv6 without brackets", "iSCSI:\n Portal: 2001:db8::1\n", 2, "is not ADDRESS[:PORT]"},
		{"target without iSCSI record", "iSCSI target: 10\n Initiator name: iqn.2024-01.x:y\n", 1, "need an iSCSI record"},
		{"non numeric id", "iSCSI:\niSCSI target: x\n", 2, "needs a device.conf Library or Drive ID"},
		{"duplicate target", "iSCSI:\niSCSI target: 10\niSCSI target: 10\n", 3, "second iSCSI target record for 10"},
		{"upper case iSCSI name", tgt + " Initiator name: IQN.2024-01.x:y\n", 3, "is not an iSCSI name"},
		{"slash in iSCSI name", tgt + " Initiator name: iqn.2024-01.x/../y\n", 3, "is not an iSCSI name"},
		{"duplicate initiator", tgt + " Initiator name: iqn.2024-01.x:y\n Initiator name: iqn.2024-01.x:y\n", 4, "listed twice"},
		{"CHAP name without secret", tgt + " CHAP name: a\n", 2, "CHAP name and CHAP secret go together"},
		{"blank in secret", tgt + " CHAP secret: a b\n", 3, "chap secret must be printable"},
		{"NULL prefix", tgt + " CHAP name: NULLuser\n CHAP secret: abcdefghijkl\n", 3, "chap name must be printable"},
		{"same secret both directions", tgt + " CHAP name: a\n CHAP secret: abcdefghijkl\n Mutual CHAP name: b\n Mutual CHAP secret: abcdefghijkl\n", 2, "must differ from the CHAP secret"},
		{"mutual without CHAP", tgt + " Mutual CHAP name: b\n Mutual CHAP secret: abcdefghijkl\n", 2, "mutual CHAP needs CHAP"},
		{"future version", "VERSION: 2\n", 1, "unsupported VERSION 2"},
		{"field outside a record", " Portal: 1.2.3.4\n", 1, "field 'portal' outside a record"},
		{"no colon", "iSCSI:\n Portal 1.2.3.4\n", 2, "expected 'Name: value'"},
		{"same portal twice", "iSCSI:\n Portal: 1.2.3.4\n Portal: 1.2.3.4:3260\n", 3, "repeats or overlaps another portal on port 3260"},
		{"wildcard and specific", "iSCSI:\n Portal: 0.0.0.0\n Portal: 192.0.2.1\n", 3, "repeats or overlaps"},
		{"IPv6 wildcard and specific", "iSCSI:\n Portal: [2001:db8::1]\n Portal: [::]:3260\n", 3, "repeats or overlaps"},
		// further rules of fab_conf_parse
		{"second iSCSI record", "iSCSI:\niSCSI:\n", 2, "second iSCSI record"},
		{"iSCSI with a value", "iSCSI: x\n", 1, "unknown record 'iscsi'"},
		{"VERSION ends a record", "iSCSI:\nVERSION: 1\n Portal: 1.2.3.4\n", 3, "outside a record"},
		{"negative id", "iSCSI:\niSCSI target: -1\n", 2, "needs a device.conf"},
		{"id with junk", "iSCSI:\niSCSI target: 10x\n", 2, "needs a device.conf"},
		{"empty target field", tgt + " Target name:\n", 3, "target name has no value"},
		{"target name twice", "iSCSI:\niSCSI target: 10\n Target name: iqn.2024-01.x:t\niSCSI target: 30\n Target name: iqn.2024-01.x:t\n", 4, "target name iqn.2024-01.x:t is used twice"},
		{"mutual name alone", tgt + " CHAP name: a\n CHAP secret: abcdefghijkl\n Mutual CHAP name: b\n", 2, "mutual CHAP name and secret go together"},
		{"secret too long", tgt + " CHAP name: a\n CHAP secret: " + long(256) + "\n", 4, "chap secret is longer than 255 bytes"},
		{"name too long", tgt + " Target name: iqn." + long(220) + "\n", 3, "is not an iSCSI name"},
		{"bare prefix name", tgt + " Initiator name: iqn.\n", 3, "is not an iSCSI name"},
		{"bad base name", "iSCSI:\n Base name: IQN.2024-01.x\n", 2, "base name 'IQN.2024-01.x' is not an iSCSI name"},
		{"empty base name", "iSCSI:\n Base name:\n", 2, "base name '' is not an iSCSI name"},
		{"empty portal", "iSCSI:\n Portal:\n", 2, "is not ADDRESS"},
		{"portal too long", "iSCSI:\n Portal: [" + strings.Repeat("0:", 34) + ":1]\n", 2, "is not ADDRESS"},
		{"junk after bracket", "iSCSI:\n Portal: [2001:db8::1]x\n", 2, "is not ADDRESS"},
		{"IPv6 zone", "iSCSI:\n Portal: [fe80::1%eth0]\n", 2, "is not ADDRESS"},
		{"IPv4 in brackets", "iSCSI:\n Portal: [192.0.2.1]\n", 2, "is not ADDRESS"},
		{"IPv4 leading zero", "iSCSI:\n Portal: 192.0.2.01\n", 2, "is not ADDRESS"},
		{"empty port", "iSCSI:\n Portal: 192.0.2.1:\n", 2, "is not ADDRESS"},
		{"too many portals", portals.String(), 18, "more than 16 portals"},
		{"too many initiators", inits.String(), 35, "more than 32 initiators"},
		{"too many targets", targets.String(), 34, "more than 32 targets"},
		{"line too long", "iSCSI:\n#" + long(1022) + "\n", 2, "line too long"},
		{"last line too long", "iSCSI:\n#" + long(1022), 2, "line too long"},
		{"NUL", "iSCSI:\n Portal: 1.2.3.4\x00\n", 2, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseFabricsConf([]byte(tc.in))
			if len(c.Problems) == 0 {
				t.Fatal("no problems")
			}
			p := c.Problems[0]
			if p.Line != tc.line || !strings.Contains(p.Msg, tc.msg) {
				t.Errorf("first problem %v, want %q on line %d", p, tc.msg, tc.line)
			}
			if err := c.Writable(); err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("Writable() = %v", err)
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// Forms upstream accepts, some of them only because it mirrors strtol and
// compares addresses as text.
func TestFabricsParseAccepts(t *testing.T) {
	for name, in := range map[string]string{
		"portals that coexist":  "iSCSI:\n Portal: 0.0.0.0:3260\n Portal: 192.0.2.1:3261\n Portal: [2001:db8::1]:3260\n",
		"[0::0] is no wildcard": "iSCSI:\n Portal: [0::0]\n Portal: [2001:db8::1]\n",
		"blank before the port": "iSCSI:\n Portal: 192.0.2.1: 3260\n",
		"plus sign id":          "iSCSI:\niSCSI target: +5\n",
		"longest line":          "iSCSI:\n#" + strings.Repeat("a", 1021) + "\n",
		"longest last line":     "iSCSI:\n#" + strings.Repeat("a", 1021),
		"longest name":          "iSCSI:\n Base name: iqn." + strings.Repeat("a", 219) + "\n",
		"longest secret":        "iSCSI:\niSCSI target: 1\n CHAP name: a\n CHAP secret: " + strings.Repeat("s", 255) + "\n",
		"repeated base name":    "iSCSI:\n Base name: iqn.2024-01.x:a\n Base name: iqn.2024-01.x:b\n",
	} {
		t.Run(name, func(t *testing.T) { mustFabrics(t, in) })
	}
	_, base, _ := mustFabrics(t, "iSCSI:\n Base name: iqn.2024-01.x:a\n Base name: iqn.2024-01.x:b\n").ISCSI()
	if base != "iqn.2024-01.x:b" {
		t.Errorf("repeated Base name: got %q, upstream keeps the last", base)
	}
}

// Mirrors test_secret_never_in_error.
func TestFabricsSecretNeverInProblem(t *testing.T) {
	for _, in := range []string{
		"iSCSI:\niSCSI target: 10\n CHAP secret: hunter2 x\n",
		"iSCSI:\niSCSI target: 10\n CHAP name: a\n CHAP secret: hunter2\n Mutual CHAP name: b\n Mutual CHAP secret: hunter2\n",
		"iSCSI:\niSCSI target: 10\n CHAP name: hunter2\n",
	} {
		c := ParseFabricsConf([]byte(in))
		if err := c.Writable(); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Writable() = %v", err)
		}
		for _, p := range c.Problems {
			if strings.Contains(p.Msg, "hunter2") {
				t.Errorf("problem leaks a CHAP value: %v", p)
			}
		}
	}
}

func TestValidISCSIName(t *testing.T) {
	for s, want := range map[string]bool{
		"iqn.2024-01.com.example:pbs1":    true,
		"eui.02004567a425678d":            true,
		"naa.52004567ba64678d":            true,
		"iqn.":                            false,
		"iqn.x":                           true,
		"IQN.2024-01.x":                   false,
		"iqn.2024-01.X":                   false,
		"iqn.2024-01.x/y":                 false,
		"iqn.2024-01.x_y":                 false,
		"iqn.2024-01.x y":                 false,
		"foo.bar":                         false,
		"iqn." + strings.Repeat("a", 219): true,
		"iqn." + strings.Repeat("a", 220): false,
	} {
		if got := ValidISCSIName(s); got != want {
			t.Errorf("ValidISCSIName(%q) = %v", s, got)
		}
	}
}

// The device.conf of mhvtl tests/test_fabrics.c.
const fabDeviceConf = "VERSION: 5\n\n" +
	"Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n Vendor identification: STK\n NAA: 10:22:33:44:ab:00:00:00\n\n" +
	"Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 01\n\n" +
	"Drive: 12 CHANNEL: 00 TARGET: 02 LUN: 00\n Library ID: 10 Slot: 02\n\n" +
	"Library: 30 CHANNEL: 00 TARGET: 08 LUN: 00\n\n" +
	"Drive: 31 CHANNEL: 00 TARGET: 09 LUN: 00\n Library ID: 30 Slot: 04\n\n" +
	"Drive: 50 CHANNEL: 00 TARGET: 10 LUN: 00\n Vendor identification: IBM\n"

const (
	fabBase = "iqn.2003-01.org.linux-iscsi.host-a:mhvtl"
	fabW10  = fabBase + ".lib10"
)

// Mirrors test_plan_library, test_plan_errors and test_plan_base_name_required.
func TestFabricsCheckPlan(t *testing.T) {
	dev := ParseDeviceConf([]byte(fabDeviceConf))
	cases := []struct {
		name, in string
		line     int
		msg      string // "" when the plan succeeds
	}{
		{"library", "iSCSI:\n Base name: " + fabBase + "\n" +
			"iSCSI target: 10\n Initiator name: iqn.2024-01.com.example:pbs1\n" +
			"iSCSI target: 30\n Target name: iqn.2024-01.com.example:custom\n Initiator name: iqn.2024-01.com.example:pbs1\n" +
			"iSCSI target: 50\n Initiator name: iqn.2024-01.com.example:pbs1\n", 0, ""},
		{"no such device", "iSCSI:\niSCSI target: 99\n", 2, "no Library or Drive 99 in device.conf"},
		{"drive inside a library", "iSCSI:\niSCSI target: 11\n", 2, "drive belongs to library 10; export the library"},
		{"same name twice", "iSCSI:\n Base name: " + fabBase + "\niSCSI target: 10\niSCSI target: 30\n Target name: " + fabW10 + "\n", 4, "target name " + fabW10 + " is used twice"},
		{"same name, standalone later", "iSCSI:\n Base name: " + fabBase + "\niSCSI target: 10\n Target name: " + fabBase + ".tape50\niSCSI target: 50\n", 5, "is used twice"},
		{"base name required", "iSCSI:\niSCSI target: 10\n", 2, "Base name"},
		{"not needed when named", "iSCSI:\niSCSI target: 10\n Target name: " + fabW10 + "\n", 0, ""},
		{"derived name too long", "iSCSI:\n Base name: iqn." + strings.Repeat("a", 214) + "\niSCSI target: 10\n", 3, "derived name is too long or invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := mustFabrics(t, tc.in).CheckPlan(dev)
			switch {
			case tc.msg == "" && len(ps) > 0:
				t.Errorf("plan failed: %v", ps)
			case tc.msg != "" && (len(ps) == 0 || ps[0].Line != tc.line || !strings.Contains(ps[0].Msg, tc.msg)):
				t.Errorf("plan problems %v, want %q on line %d", ps, tc.msg, tc.line)
			}
		})
	}

	slots := ParseDeviceConf([]byte("Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n\n" +
		"Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 01\n\n" +
		"Drive: 12 CHANNEL: 00 TARGET: 02 LUN: 00\n Library ID: 10 Slot: 01\n"))
	ps := mustFabrics(t, "iSCSI:\n Base name: "+fabBase+"\niSCSI target: 10\n").CheckPlan(slots)
	if len(ps) != 1 || !strings.Contains(ps[0].Msg, "two devices on LUN 1") {
		t.Errorf("duplicate slot: %v", ps)
	}
	noSlot := ParseDeviceConf([]byte("Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00\n\n" +
		"Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00\n Library ID: 10 Slot: 0\n"))
	ps = mustFabrics(t, "iSCSI:\n Base name: "+fabBase+"\niSCSI target: 10\n").CheckPlan(noSlot)
	if len(ps) != 1 || !strings.Contains(ps[0].Msg, "drive 11 in library 10 has no Slot") {
		t.Errorf("slot 0: %v", ps)
	}
}

func TestFabricsEditCreate(t *testing.T) {
	const want = "VERSION: 1\n\n" +
		"iSCSI:\n Portal: 192.0.2.10:3260\n Base name: iqn.2003-01.org.linux-iscsi.vtl1:mhvtl\n\n" +
		"iSCSI target: 10\n Initiator name: iqn.2024-01.com.example:pbs1\n CHAP name: pbs1\n CHAP secret: change-me-0123\n"
	for name, in := range map[string]string{"empty": "", "VERSION only": "VERSION: 1\n"} {
		t.Run(name, func(t *testing.T) {
			c := ParseFabricsConf([]byte(in))
			if err := c.SetISCSI([]string{"192.0.2.10:3260"}, strp("iqn.2003-01.org.linux-iscsi.vtl1:mhvtl")); err != nil {
				t.Fatal(err)
			}
			if err := c.UpsertTarget(10, TargetEdit{
				Initiators: []string{"iqn.2024-01.com.example:pbs1"},
				CHAP:       &CHAPCreds{Name: "pbs1", Secret: "change-me-0123"},
			}); err != nil {
				t.Fatal(err)
			}
			w := want
			if in == "" {
				w = strings.TrimPrefix(want, "VERSION: 1\n\n")
			}
			if got := string(c.Render()); got != w {
				t.Errorf("got:\n%s\nwant:\n%s", got, w)
			}
		})
	}

	// Only comments: appended after them.
	c := mustFabrics(t, "# header\n")
	if err := c.SetISCSI(nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := string(c.Render()); got != "# header\n\niSCSI:\n" {
		t.Errorf("got %q", got)
	}
}

const fabEditBase = "VERSION: 1\n" +
	"iSCSI:\n" +
	" Portal: 192.0.2.10   # storage net\n" +
	" # old: 192.0.2.9\n" +
	" Base name: iqn.2024-01.com.example:vtl # main\n" +
	"\n" +
	"# PBS\n" +
	"iSCSI target: 10\n" +
	" initiator NAME: iqn.2024-01.com.example:pbs1   # first\n" +
	" Initiator name: iqn.2024-01.com.example:pbs2\n" +
	" CHAP name: pbs\n" +
	" CHAP secret: abcdefghijkl\n" +
	" # rotate yearly\n" +
	" Mutual CHAP name: vtl\n" +
	" Mutual CHAP secret: mnopqrstuvwx\n" +
	"\n" +
	"# tape 50\n" +
	"iSCSI target: 50\n" +
	" Initiator name: iqn.2024-01.com.example:pbs1\n"

func TestFabricsEditExisting(t *testing.T) {
	r := strings.NewReplacer
	cases := []struct {
		name string
		edit func(c *FabricsConf) error
		want string
	}{
		{"portals replaced", func(c *FabricsConf) error {
			return c.SetISCSI([]string{"192.0.2.11", "[2001:db8::1]"}, nil)
		}, r(" Portal: 192.0.2.10   # storage net\n", " Portal: 192.0.2.11\n Portal: [2001:db8::1]\n").Replace(fabEditBase)},
		{"portal kept, one added", func(c *FabricsConf) error {
			return c.SetISCSI([]string{"192.0.2.10", "192.0.2.11"}, nil)
		}, r("# storage net\n", "# storage net\n Portal: 192.0.2.11\n").Replace(fabEditBase)},
		{"portals removed", func(c *FabricsConf) error { return c.SetISCSI([]string{}, nil) },
			r(" Portal: 192.0.2.10   # storage net\n", "").Replace(fabEditBase)},
		{"base name rewritten in place", func(c *FabricsConf) error {
			return c.SetISCSI(nil, strp("iqn.2024-01.com.example:vtl2"))
		}, r(":vtl # main", ":vtl2 # main").Replace(fabEditBase)},
		{"base name removed", func(c *FabricsConf) error { return c.SetISCSI(nil, strp("")) },
			r(" Base name: iqn.2024-01.com.example:vtl # main\n", "").Replace(fabEditBase)},
		{"initiators, CHAP kept", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{Initiators: []string{"iqn.2024-01.com.example:pbs1", "iqn.2024-01.com.example:pbs3"}})
		}, r(":pbs2\n", ":pbs3\n").Replace(fabEditBase)},
		{"CHAP rewritten", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{CHAP: &CHAPCreds{Name: "pbs-new", Secret: "s3cret-012345"}})
		}, r(" CHAP name: pbs\n", " CHAP name: pbs-new\n", " CHAP secret: abcdefghijkl\n", " CHAP secret: s3cret-012345\n").Replace(fabEditBase)},
		{"mutual cleared", func(c *FabricsConf) error { return c.UpsertTarget(10, TargetEdit{ClearMutualCHAP: true}) },
			r(" Mutual CHAP name: vtl\n Mutual CHAP secret: mnopqrstuvwx\n", "").Replace(fabEditBase)},
		{"CHAP cleared clears mutual", func(c *FabricsConf) error { return c.UpsertTarget(10, TargetEdit{ClearCHAP: true}) },
			r(" CHAP name: pbs\n CHAP secret: abcdefghijkl\n", "", " Mutual CHAP name: vtl\n Mutual CHAP secret: mnopqrstuvwx\n", "").Replace(fabEditBase)},
		{"CHAP added to a target", func(c *FabricsConf) error {
			return c.UpsertTarget(50, TargetEdit{CHAP: &CHAPCreds{Name: "t50", Secret: "abcdefghijkl"}, TargetName: strp("iqn.2024-01.com.example:t50")})
		}, fabEditBase + " Target name: iqn.2024-01.com.example:t50\n CHAP name: t50\n CHAP secret: abcdefghijkl\n"},
		{"target name removed", func(c *FabricsConf) error { return c.UpsertTarget(50, TargetEdit{TargetName: strp("")}) }, fabEditBase},
		{"new target appended", func(c *FabricsConf) error {
			return c.UpsertTarget(30, TargetEdit{Initiators: []string{"iqn.2024-01.com.example:pbs2"}})
		}, fabEditBase + "\niSCSI target: 30\n Initiator name: iqn.2024-01.com.example:pbs2\n"},
		{"empty edit", func(c *FabricsConf) error { return c.UpsertTarget(10, TargetEdit{}) }, fabEditBase},
		// Removing a target keeps the comments above it.
		{"remove last target", func(c *FabricsConf) error { return c.RemoveTarget(50) },
			strings.TrimSuffix(fabEditBase, "iSCSI target: 50\n Initiator name: iqn.2024-01.com.example:pbs1\n")},
		{"remove middle target", func(c *FabricsConf) error { return c.RemoveTarget(10) },
			fabEditBase[:strings.Index(fabEditBase, "# PBS\n")+len("# PBS\n")] + "\n# tape 50\niSCSI target: 50\n Initiator name: iqn.2024-01.com.example:pbs1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustFabrics(t, fabEditBase)
			if err := tc.edit(c); err != nil {
				t.Fatal(err)
			}
			if got := string(c.Render()); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			mustFabrics(t, tc.want)
		})
	}

	c := mustFabrics(t, fabEditBase)
	if err := c.UpsertTarget(10, TargetEdit{Initiators: []string{"iqn.2024-01.com.example:pbs1"}}); err != nil {
		t.Fatal(err)
	}
	if ts := c.Targets(); !ts[0].HasCHAP || !ts[0].HasMutualCHAP || len(ts[0].Initiators) != 1 {
		t.Errorf("CHAP not kept: %+v", ts[0])
	}
	if err := c.RemoveTarget(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveTarget(99) = %v", err)
	}
}

func TestFabricsEditSample(t *testing.T) {
	sample := readFabricsSample(t)
	const init = " Initiator name: iqn.2024-01.com.example:pbs1\n"

	c := mustFabrics(t, sample)
	if err := c.UpsertTarget(10, TargetEdit{CHAP: &CHAPCreds{Name: "pbs1", Secret: "s3cret-012345"}}); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sample, init, init+" CHAP name: pbs1\n CHAP secret: s3cret-012345\n", 1)
	if got := string(c.Render()); got != want {
		t.Errorf("CHAP added:\n%s", got)
	}

	// Removing the only target takes the commented-out CHAP lines after it
	// but keeps the comment block above it, which documents the record
	// format and its SECURITY warning.
	c = mustFabrics(t, sample)
	if err := c.RemoveTarget(10); err != nil {
		t.Fatal(err)
	}
	end := strings.Index(sample, "iSCSI target: 10\n")
	got := string(c.Render())
	if got != sample[:end] {
		t.Errorf("target removed:\n%s", got)
	}
	if !strings.Contains(got, "An Initiator name is an identity, not a credential") {
		t.Error("the sample's SECURITY comment was removed with the target")
	}
}

func TestFabricsEditLineEndings(t *testing.T) {
	c := mustFabrics(t, "iSCSI:\n Portal: 192.0.2.1")
	if err := c.UpsertTarget(10, TargetEdit{Initiators: []string{"iqn.2024-01.x:a"}}); err != nil {
		t.Fatal(err)
	}
	if got, w := string(c.Render()), "iSCSI:\n Portal: 192.0.2.1\n\niSCSI target: 10\n Initiator name: iqn.2024-01.x:a"; got != w {
		t.Errorf("no final newline: got %q", got)
	}
	if err := c.RemoveTarget(10); err != nil {
		t.Fatal(err)
	}
	if got := string(c.Render()); got != "iSCSI:\n Portal: 192.0.2.1" {
		t.Errorf("no final newline after remove: got %q", got)
	}

	c = mustFabrics(t, "iSCSI:\r\n Portal: 192.0.2.1\r\n")
	if err := c.SetISCSI(nil, strp("iqn.2024-01.x:vtl")); err != nil {
		t.Fatal(err)
	}
	if got, w := string(c.Render()), "iSCSI:\r\n Portal: 192.0.2.1\r\n Base name: iqn.2024-01.x:vtl\r\n"; got != w {
		t.Errorf("CRLF: got %q", got)
	}
}

func TestFabricsEditRefused(t *testing.T) {
	cases := []struct {
		name string
		edit func(c *FabricsConf) error
		msg  string
	}{
		{"bad portal", func(c *FabricsConf) error { return c.SetISCSI([]string{"host.example"}, nil) }, "is not ADDRESS"},
		{"portal conflict", func(c *FabricsConf) error { return c.SetISCSI([]string{"192.0.2.10", "0.0.0.0"}, nil) }, "repeats or overlaps"},
		{"portal twice", func(c *FabricsConf) error { return c.SetISCSI([]string{"192.0.2.1", "192.0.2.1"}, nil) }, "listed twice"},
		{"comment in value", func(c *FabricsConf) error { return c.SetISCSI([]string{"192.0.2.1 #x"}, nil) }, "starts a comment"},
		{"newline in value", func(c *FabricsConf) error { return c.SetISCSI(nil, strp("iqn.2024-01.x:a\nFC:")) }, "control character"},
		{"bad base name", func(c *FabricsConf) error { return c.SetISCSI(nil, strp("IQN.x")) }, "is not an iSCSI name"},
		{"upper-case initiator", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{Initiators: []string{"IQN.2024-01.x:a"}})
		}, "is not an iSCSI name"},
		{"initiator twice", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{Initiators: []string{"iqn.2024-01.x:a", "iqn.2024-01.x:a"}})
		}, "listed twice"},
		{"blank in secret", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{CHAP: &CHAPCreds{Name: "a", Secret: "hunt er2"}})
		}, "chap secret must be printable"},
		{"secret without name", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{CHAP: &CHAPCreds{Secret: "abcdefghijkl"}})
		}, "needs both a name and a secret"},
		{"same mutual secret", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{MutualCHAP: &CHAPCreds{Name: "m", Secret: "abcdefghijkl"}})
		}, "must differ"},
		{"mutual without CHAP", func(c *FabricsConf) error {
			return c.UpsertTarget(50, TargetEdit{MutualCHAP: &CHAPCreds{Name: "m", Secret: "mnopqrstuvwx"}})
		}, "mutual CHAP needs CHAP"},
		{"mutual set while CHAP cleared", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{ClearCHAP: true, MutualCHAP: &CHAPCreds{Name: "m", Secret: "mnopqrstuvwx"}})
		}, "both set and cleared"},
		{"CHAP set and cleared", func(c *FabricsConf) error {
			return c.UpsertTarget(10, TargetEdit{ClearCHAP: true, CHAP: &CHAPCreds{Name: "a", Secret: "abcdefghijkl"}})
		}, "both set and cleared"},
		{"negative id", func(c *FabricsConf) error { return c.UpsertTarget(-1, TargetEdit{}) }, "needs a device.conf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustFabrics(t, fabEditBase)
			before := string(c.Render())
			err := tc.edit(c)
			if err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want %q", err, tc.msg)
			}
			if strings.Contains(err.Error(), "hunt er2") || strings.Contains(err.Error(), "mnopqrstuvwx") {
				t.Errorf("error leaks a secret: %v", err)
			}
			if got := string(c.Render()); got != before {
				t.Errorf("file changed by a refused edit:\n%s", got)
			}
			if c.Writable() != nil {
				t.Errorf("model left with problems: %v", c.Problems)
			}
		})
	}

	// A Target name another target has.
	c := mustFabrics(t, fabEditBase+" Target name: iqn.2024-01.x:t\n")
	if err := c.UpsertTarget(10, TargetEdit{TargetName: strp("iqn.2024-01.x:t")}); err == nil || !strings.Contains(err.Error(), "is used twice") {
		t.Errorf("duplicate target name: %v", err)
	}
	// A target needs an iSCSI record.
	c = mustFabrics(t, "VERSION: 1\n")
	if err := c.UpsertTarget(10, TargetEdit{}); err == nil || !strings.Contains(err.Error(), "need an iSCSI record") {
		t.Errorf("target without iSCSI record: %v", err)
	}
	// No edit on a file with problems.
	bad := ParseFabricsConf([]byte("iSCSI:\n Portl: 1.2.3.4\niSCSI target: 10\n"))
	for _, err := range []error{
		bad.SetISCSI(nil, nil),
		bad.UpsertTarget(10, TargetEdit{}),
		bad.RemoveTarget(10),
	} {
		if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "portl") {
			t.Errorf("edit on a bad file: %v", err)
		}
	}
}

func TestRedactFabrics(t *testing.T) {
	in := "iSCSI target: 10\n" +
		" chap SECRET: abcdefghijkl\n" +
		"\tMutual  CHAP Secret:mnopqrstuvwx # c\r\n" +
		"# CHAP secret: change-me-0123\n" +
		" CHAP name: pbs\n" +
		" CHAP secret hunter2\n" +
		" CHAP secret:\n" +
		"#   CHAP name, CHAP secret:\n" +
		"# Keep this file mode 0600 when it holds a CHAP secret; mhvtl-fabrics refuses\n" +
		"garbage line\n" +
		"CHAP Secret: col1secret"
	want := "iSCSI target: 10\n" +
		" chap SECRET: ********\n" +
		"\tMutual  CHAP Secret:********\r\n" +
		"# CHAP secret: ********\n" +
		" CHAP name: pbs\n" +
		" CHAP secret ********\n" +
		" CHAP secret:\n" +
		"#   CHAP name, CHAP secret:\n" +
		"# Keep this file mode 0600 when it holds a CHAP secret; mhvtl-fabrics refuses\n" +
		"garbage line\n" +
		"CHAP Secret: ********"
	if got := string(RedactFabrics([]byte(in))); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}

	// The sample: only its commented-out secret changes.
	sample := readFabricsSample(t)
	if got, w := string(RedactFabrics([]byte(sample))), strings.Replace(sample, "# CHAP secret: change-me-0123", "# CHAP secret: ********", 1); got != w {
		t.Errorf("sample redaction changed other lines:\n%s", got)
	}
	// A file without secrets is unchanged.
	if got := RedactFabrics([]byte(fabEditBase[:40])); string(got) != fabEditBase[:40] {
		t.Errorf("changed: %q", got)
	}
	if got := string(RedactFabrics([]byte(fabEditBase))); strings.Contains(got, "abcdefghijkl") || strings.Contains(got, "mnopqrstuvwx") {
		t.Errorf("secret left: %s", got)
	}
}

// TestFabricsDiff: the diff masks secrets and still shows a secret-only
// change, as a masked line removed and added.
func TestFabricsDiff(t *testing.T) {
	a := "iSCSI target: 10\n Initiator name: iqn.2024-01.com.example:a\n CHAP name: a\n CHAP secret: old-secret-0123\n"
	b := strings.Replace(a, "old-secret-0123", "new-secret-4567", 1)
	got := FabricsDiff([]byte(a), []byte(b))
	want := "--- fabrics.conf\n+++ fabrics.conf\n@@ line 4 @@\n- CHAP secret: ********\n+ CHAP secret: ********\n"
	if got != want {
		t.Errorf("FabricsDiff = %q, want %q", got, want)
	}
}

// TestRedactFabricsAnySpace: split_field collapses any isspace() byte in a
// field name, so " CHAP\vsecret:" names a secret and is masked.
func TestRedactFabricsAnySpace(t *testing.T) {
	for _, sep := range []string{" ", "\t", "\v", "\f", "\r", " \v "} {
		in := "iSCSI target: 10\n Mutual" + sep + "CHAP" + sep + "secret: OLDSECRET11\n"
		got := string(RedactFabrics([]byte(in)))
		if strings.Contains(got, "OLDSECRET11") {
			t.Errorf("sep %q: not masked: %q", sep, got)
		}
		if s := FabricsSecrets([]byte(in)); len(s) == 0 || s[0] != "OLDSECRET11" {
			t.Errorf("sep %q: FabricsSecrets = %q", sep, s)
		}
	}
}
