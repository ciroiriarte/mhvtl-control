package transports

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mhvtl-control/internal/config"
)

const iqn = "iqn.2003-01.org.linux-iscsi.vtl:mhvtl"

// fakeTree writes files (path -> content) and symlinks (path -> target)
// under a new root.
func fakeTree(t *testing.T, files, links map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "target")
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for p, dst := range links {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dst, full); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// sessionInfo is an ACL info file as lio_target_nacl_info_show writes it.
const sessionInfo = `InitiatorName: iqn.1993-08.org.debian:01:backup1
InitiatorAlias: backup1
LIO Session ID: 3   ISID: 0x00 02 3d 00 00 01  TSIH: 3  SessionType: Normal
Session State: TARG_SESS_STATE_LOGGED_IN
---------------------[iSCSI Session Values]-----------------------
  CmdSN/WR  :  CmdSN/WC  :  ExpCmdSN  :  MaxCmdSN  :     ITT    :     TTT
 0x00000040   0x00000040   0x00000123   0x00000162   0x00000099   0xffffffff
----------------------[iSCSI Connections]-------------------------
CID: 0  Connection State: TARG_CONN_STATE_LOGGED_IN
   Address 192.0.2.10 TCP  StatSN: 0x00000124
`

// iscsiTree is one iSCSI target with a changer on LUN 0 and four drives on
// LUNs 1-4, two ACLs (one logged in) and the loopback presenting LUN 0.
func iscsiTree(t *testing.T) string {
	files := map[string]string{
		"version": "Target Engine Core ConfigFS Infrastructure v5.0\n",
		"loopback/naa.5001405000000001/tpgt_1/nexus":     "naa.50014050000000ff\n",
		"iscsi/discovery_auth/enforce_discovery_auth":    "0\n",
		"loopback/discovery_auth/enforce_discovery_auth": "0\n",
		"iscsi/" + iqn + "/tpgt_1/enable":                "1\n",
		"iscsi/" + iqn + "/tpgt_1/dynamic_sessions":      "",
		"iscsi/" + iqn + "/tpgt_1/np/0.0.0.0:3260/iser":  "0\n",
	}
	links := map[string]string{
		"loopback/naa.5001405000000001/tpgt_1/lun/lun_0/b1f2c3": "../../../../../../target/core/user_0/lib10",
	}
	backstores := []string{"lib10", "tape11", "tape12", "tape13", "tape14"}
	for i, bs := range backstores {
		minor := strings.TrimLeft(bs, "libtape")
		files["core/user_0/"+bs+"/enable"] = "1\n"
		files["core/user_0/"+bs+"/attrib/dev_config"] = "mhvtl/" + minor + "\n"
		files["core/user_0/"+bs+"/wwn/vpd_unit_serial"] = "T10 VPD Unit Serial Number: SER" + minor + "\n"
		lun := "lun_" + string(rune('0'+i))
		links["iscsi/"+iqn+"/tpgt_1/lun/"+lun+"/"+bs+"link"] = "../../../../../../target/core/user_0/" + bs
		files["iscsi/"+iqn+"/tpgt_1/lun/"+lun+"/alua_tg_pt_gp"] = "default_tg_pt_gp\n"
	}
	for _, init := range []string{"iqn.1993-08.org.debian:01:backup1", "iqn.1993-08.org.debian:01:backup2"} {
		acl := "iscsi/" + iqn + "/tpgt_1/acls/" + init
		for i := 0; i < 5; i++ {
			lun := "lun_" + string(rune('0'+i))
			files[acl+"/"+lun+"/write_protect"] = "0\n"
			links[acl+"/"+lun+"/m"+lun] = "../../../../../../iscsi/" + iqn + "/tpgt_1/lun/" + lun
		}
		files[acl+"/attrib/dataout_timeout"] = "3\n"
		files[acl+"/info"] = "No active iSCSI Session for Initiator Endpoint: " + init + "\n"
	}
	files["iscsi/"+iqn+"/tpgt_1/acls/iqn.1993-08.org.debian:01:backup1/info"] = sessionInfo
	return fakeTree(t, files, links)
}

func TestReadISCSI(t *testing.T) {
	m := Read(iscsiTree(t))
	if !m.ConfigFS {
		t.Fatal("configfs = false")
	}
	if len(m.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", m.Warnings)
	}
	if len(m.Backstores) != 5 {
		t.Fatalf("backstores = %d, want 5: %+v", len(m.Backstores), m.Backstores)
	}
	b := m.Backstores[0]
	if b.HBA != "user_0" || b.Name != "lib10" || b.Enabled == nil || !*b.Enabled ||
		b.DevConfig != "mhvtl/10" || b.UnitSerial != "SER10" || b.Minor == nil || *b.Minor != 10 {
		t.Errorf("backstore 0 = %+v", b)
	}
	if len(m.Fabrics) != 2 || m.Fabrics[0].Name != "loopback" || m.Fabrics[1].Name != "iscsi" {
		t.Fatalf("fabrics = %+v, want loopback and iscsi", m.Fabrics)
	}
	if lb := m.Fabrics[0]; len(lb.Targets) != 1 || lb.Targets[0].WWN != "naa.5001405000000001" {
		t.Fatalf("loopback targets = %+v, want only naa.5001405000000001 (not discovery_auth)", lb.Targets)
	}
	isc := m.Fabrics[1]
	if len(isc.Targets) != 1 || isc.Targets[0].WWN != iqn {
		t.Fatalf("iscsi targets = %+v, want only %s (not discovery_auth)", isc.Targets, iqn)
	}
	tpg := isc.Targets[0].TPGs[0]
	if tpg.Tag != 1 || tpg.Enabled == nil || !*tpg.Enabled {
		t.Errorf("tpg = %+v", tpg)
	}
	if !reflect.DeepEqual(tpg.Portals, []string{"0.0.0.0:3260"}) {
		t.Errorf("portals = %q", tpg.Portals)
	}
	if len(tpg.LUNs) != 5 {
		t.Fatalf("luns = %+v, want 5", tpg.LUNs)
	}
	for i, want := range []string{"lib10", "tape11", "tape12", "tape13", "tape14"} {
		if l := tpg.LUNs[i]; l.LUN != i || l.HBA != "user_0" || l.Backstore != want {
			t.Errorf("lun %d = %+v, want user_0/%s", i, l, want)
		}
	}
	if len(tpg.ACLs) != 2 {
		t.Fatalf("acls = %+v, want 2", tpg.ACLs)
	}
	a := tpg.ACLs[0]
	if len(a.MappedLUNs) != 5 || a.MappedLUNs[4].LUN != 4 || a.MappedLUNs[4].TPGLUN == nil ||
		*a.MappedLUNs[4].TPGLUN != 4 || a.MappedLUNs[4].WriteProtect == nil || *a.MappedLUNs[4].WriteProtect {
		t.Errorf("acl 0 mapped luns = %+v", a.MappedLUNs)
	}
	want := &Session{Alias: "backup1", Type: "Normal", State: "TARG_SESS_STATE_LOGGED_IN",
		Connections: []Connection{{CID: 0, State: "TARG_CONN_STATE_LOGGED_IN", Address: "192.0.2.10"}}}
	if !reflect.DeepEqual(a.Session, want) {
		t.Errorf("session = %+v, want %+v", a.Session, want)
	}
	if tpg.ACLs[1].Session != nil {
		t.Errorf("acl 1 session = %+v, want none", tpg.ACLs[1].Session)
	}
	for _, a := range tpg.ACLs {
		if !a.SessionReported {
			t.Errorf("acl %s: session_reported = false, want true (it has info)", a.Initiator)
		}
	}
	if len(tpg.DynamicSessions) != 0 {
		t.Errorf("dynamic sessions = %q, want none", tpg.DynamicSessions)
	}
	if got := m.Active(); !reflect.DeepEqual(got, []string{"local", "iscsi"}) {
		t.Errorf("active = %q, want local, iscsi", got)
	}
}

func TestResolve(t *testing.T) {
	m := Read(iscsiTree(t))
	m.Resolve(config.Inventory{
		Libraries:  []config.Library{{ID: 10, Drives: []config.Drive{{ID: 11, LibraryID: 10}, {ID: 12, LibraryID: 10}}}},
		Standalone: []config.Drive{{ID: 13, LibraryID: 99}},
	})
	lib := 10
	want := []*Device{
		{Kind: "library", ID: 10},
		{Kind: "drive", ID: 11, LibraryID: &lib},
		{Kind: "drive", ID: 12, LibraryID: &lib},
		{Kind: "drive", ID: 13},
		nil, // tape14 is not in device.conf
	}
	for i, w := range want {
		if got := m.Backstores[i].Device; !reflect.DeepEqual(got, w) {
			t.Errorf("backstore %s device = %+v, want %+v", m.Backstores[i].Name, got, w)
		}
	}
}

func TestReadNoConfigFS(t *testing.T) {
	m := Read(filepath.Join(t.TempDir(), "absent"))
	if m.ConfigFS || len(m.Backstores) != 0 || len(m.Fabrics) != 0 || len(m.Warnings) != 0 {
		t.Errorf("map = %+v, want empty", m)
	}
	if m.Backstores == nil || m.Fabrics == nil || m.Warnings == nil {
		t.Error("nil slices would encode as null")
	}
	if got := m.Active(); len(got) != 0 {
		t.Errorf("active = %q, want none", got)
	}
}

func TestReadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode 000 files")
	}
	root := iscsiTree(t)
	info := filepath.Join(root, "iscsi", iqn, "tpgt_1", "acls", "iqn.1993-08.org.debian:01:backup1", "info")
	cfg := filepath.Join(root, "core", "user_0", "tape12", "attrib", "dev_config")
	for _, p := range []string{info, cfg} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
	}
	m := Read(root)
	if len(m.Warnings) != 2 {
		t.Fatalf("warnings = %q, want 2", m.Warnings)
	}
	for _, w := range m.Warnings {
		if !strings.HasSuffix(w, ": permission denied") || strings.HasPrefix(w, root) {
			t.Errorf("warning %q: want a root-relative path and permission denied", w)
		}
	}
	if b := m.Backstores[2]; b.Name != "tape12" || b.DevConfig != "" || b.Minor != nil || b.UnitSerial != "SER12" {
		t.Errorf("tape12 = %+v, want dev_config left out, the rest read", b)
	}
	if s := m.Fabrics[1].Targets[0].TPGs[0].ACLs[0].Session; s != nil {
		t.Errorf("session = %+v, want left out", s)
	}
	if n := len(m.Fabrics[1].Targets[0].TPGs[0].LUNs); n != 5 {
		t.Errorf("luns = %d, want 5", n)
	}
}

func TestDynamicSessions(t *testing.T) {
	root := fakeTree(t, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/enable":           "0\n",
		"iscsi/" + iqn + "/tpgt_1/dynamic_sessions": "iqn.a:one\n\x00iqn.b:two\n\x00",
	}, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/lun/lun_0/x": "../../../../../../target/core/user_0/lib10",
	})
	m := Read(root)
	tpg := m.Fabrics[0].Targets[0].TPGs[0]
	if !reflect.DeepEqual(tpg.DynamicSessions, []string{"iqn.a:one", "iqn.b:two"}) {
		t.Errorf("dynamic sessions = %q", tpg.DynamicSessions)
	}
	if got := m.Active(); len(got) != 0 {
		t.Errorf("active = %q, want none: the TPG is disabled", got)
	}
}

// An FC ACL has no info attribute (only iSCSI's has one,
// iscsi_target_configfs.c lio_target_nacl_info_show): its session is not
// reported, which is not the same as having none.
func TestReadFCAndUSB(t *testing.T) {
	const fcACL = "qla2xxx/21:00:00:24:ff:31:4c:48/tpgt_1/acls/21:00:00:24:ff:31:4c:49"
	root := fakeTree(t, map[string]string{
		"qla2xxx/21:00:00:24:ff:31:4c:48/tpgt_1/enable": "1\n",
		"usb_gadget/naa.6001405c3214b06a/tpgt_1/enable": "1\n",
		fcACL + "/lun_0/write_protect":                  "0\n",
	}, map[string]string{
		"qla2xxx/21:00:00:24:ff:31:4c:48/tpgt_1/lun/lun_0/x": "../../../../../../target/core/user_0/lib10",
		fcACL + "/lun_0/m": "../../../../../../qla2xxx/21:00:00:24:ff:31:4c:48/tpgt_1/lun/lun_0",
	})
	m := Read(root)
	if len(m.Fabrics) != 2 || m.Fabrics[0].Transport != "fc" || m.Fabrics[1].Transport != "usb" {
		t.Fatalf("fabrics = %+v", m.Fabrics)
	}
	acls := m.Fabrics[0].Targets[0].TPGs[0].ACLs
	if len(acls) != 1 || acls[0].Initiator != "21:00:00:24:ff:31:4c:49" || len(acls[0].MappedLUNs) != 1 {
		t.Fatalf("fc acls = %+v, want one with lun 0", acls)
	}
	if acls[0].SessionReported || acls[0].Session != nil {
		t.Errorf("fc acl = %+v, want session not reported", acls[0])
	}
	if got := m.Active(); !reflect.DeepEqual(got, []string{"fc"}) {
		t.Errorf("active = %q, want fc (the USB TPG has no LUN)", got)
	}
}

// Active(root), the shallow scan behind GET /system, agrees with
// Read(root).Active() on every tree.
func TestActiveMatchesRead(t *testing.T) {
	trees := map[string]string{
		"iscsi": iscsiTree(t),
		"none":  filepath.Join(t.TempDir(), "missing"),
		"npiv and fcoe, one disabled": fakeTree(t, map[string]string{
			"qla2xxx_npiv/21:00:00:24:ff:31:4c:48@21:01:00:24:ff:31:4c:48/tpgt_1/enable": "0\n",
			"efct_npiv/10:00:00:90:fa:00:00:01@10:00:00:90:fa:00:00:02/tpgt_1/enable":    "1\n",
			"fc/20:00:00:25:b5:00:00:01/tpgt_1/enable":                                   "1\n",
		}, map[string]string{
			"qla2xxx_npiv/21:00:00:24:ff:31:4c:48@21:01:00:24:ff:31:4c:48/tpgt_1/lun/lun_0/x": "../../../../../../target/core/user_0/lib10",
			"fc/20:00:00:25:b5:00:00:01/tpgt_1/lun/lun_0/x":                                   "../../../../../../target/core/user_0/lib10",
		}),
		"usb without a LUN": fakeTree(t, map[string]string{
			"usb_gadget/naa.6001405c3214b06a/tpgt_1/enable": "1\n",
		}, nil),
	}
	want := map[string][]string{
		"iscsi":                       {"local", "iscsi"},
		"none":                        {},
		"npiv and fcoe, one disabled": {"fc"},
		"usb without a LUN":           {},
	}
	for name, root := range trees {
		got := Active(root)
		if !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s: Active = %q, want %q", name, got, want[name])
		}
		if read := Read(root).Active(); !reflect.DeepEqual(got, read) {
			t.Errorf("%s: Active = %q, Read().Active() = %q", name, got, read)
		}
	}
}

// The NPIV and FCoE fabric directories are read as Fibre Channel
func TestReadFCFabricNames(t *testing.T) {
	files := map[string]string{}
	for _, dir := range []string{"qla2xxx_npiv", "efct_npiv", "fc"} {
		files[dir+"/20:00:00:25:b5:00:00:01/tpgt_1/enable"] = "1\n"
	}
	m := Read(fakeTree(t, files, nil))
	var names []string
	for _, f := range m.Fabrics {
		if f.Transport != "fc" {
			t.Errorf("%s: transport %q", f.Name, f.Transport)
		}
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"qla2xxx_npiv", "efct_npiv", "fc"}) {
		t.Errorf("fabrics = %q", names)
	}
}

// target_wwn_vpd_unit_serial_show() prints "T10 VPD Unit Serial Number: %s\n";
// an empty serial is "", not the label.
func TestEmptyUnitSerial(t *testing.T) {
	m := Read(fakeTree(t, map[string]string{
		"core/user_0/tape11/wwn/vpd_unit_serial": "T10 VPD Unit Serial Number: \n",
	}, nil))
	if len(m.Backstores) != 1 || m.Backstores[0].UnitSerial != "" {
		t.Errorf("backstores = %+v, want one with an empty unit serial", m.Backstores)
	}
}

// A LUN, mapped LUN, ACL, TPG, target or backstore removed while it is read
// (an mhvtl-fabrics apply) is left out, without a warning, and does not
// count as exported.
func TestRemovedMidRead(t *testing.T) {
	root := iscsiTree(t)
	tpg := filepath.Join(root, "iscsi", iqn, "tpgt_1")
	// A second target, removed after it is listed.
	goneTarget := filepath.Join(root, "iscsi", "iqn.2003-01.org.linux-iscsi.vtl:gone")
	if err := os.MkdirAll(filepath.Join(goneTarget, "tpgt_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goneTarget, "tpgt_1", "enable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{
		filepath.Join(tpg, "lun", "lun_2"):                                       true,
		filepath.Join(tpg, "acls", "iqn.1993-08.org.debian:01:backup1", "lun_3"): true,
		filepath.Join(tpg, "acls", "iqn.1993-08.org.debian:01:backup2"):          true,
		// The whole loopback TPG: its lun/ is still listed, then the
		// TPG directory is found gone.
		filepath.Join(root, "loopback", "naa.5001405000000001", "tpgt_1"): true,
		filepath.Join(root, "core", "user_0", "tape14"):                   true,
		goneTarget: true,
	}
	// lun_1's link is removed after its directory is listed: the entry is
	// there, os.Readlink finds nothing.
	lun1 := filepath.Join(tpg, "lun", "lun_1")
	// An ACL removed right after its own directory is listed: its info and
	// LUN reads find nothing.
	lateACL := filepath.Join(tpg, "acls", "iqn.2026-11.org.example:late")
	if err := os.MkdirAll(filepath.Join(lateACL, "lun_0"), 0o755); err != nil {
		t.Fatal(err)
	}
	readDir = func(p string) ([]os.DirEntry, error) {
		if gone[p] {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		ents, err := os.ReadDir(p)
		switch p {
		case lun1:
			_ = os.Remove(filepath.Join(lun1, "tape11link"))
		case lateACL:
			_ = os.RemoveAll(lateACL)
		}
		return ents, err
	}
	t.Cleanup(func() { readDir = os.ReadDir })
	m := Read(root)
	if len(m.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", m.Warnings)
	}
	var names []string
	for _, b := range m.Backstores {
		names = append(names, b.Name)
	}
	if !reflect.DeepEqual(names, []string{"lib10", "tape11", "tape12", "tape13"}) {
		t.Errorf("backstores = %q, want tape14 left out", names)
	}
	if n := len(m.Fabrics[1].Targets); n != 1 || m.Fabrics[1].Targets[0].WWN != iqn {
		t.Fatalf("iscsi targets = %+v, want %s only", m.Fabrics[1].Targets, iqn)
	}
	tp := m.Fabrics[1].Targets[0].TPGs[0]
	var luns []int
	for _, l := range tp.LUNs {
		luns = append(luns, l.LUN)
	}
	if !reflect.DeepEqual(luns, []int{0, 3, 4}) {
		t.Errorf("luns = %v, want 0 3 4", luns)
	}
	if len(tp.ACLs) != 1 || tp.ACLs[0].Initiator != "iqn.1993-08.org.debian:01:backup1" {
		t.Fatalf("acls = %+v, want backup1 only", tp.ACLs)
	}
	var mapped []int
	for _, ml := range tp.ACLs[0].MappedLUNs {
		mapped = append(mapped, ml.LUN)
	}
	if !reflect.DeepEqual(mapped, []int{0, 1, 2, 4}) {
		t.Errorf("mapped luns = %v, want 0 1 2 4", mapped)
	}
	if lb := m.Fabrics[0].Targets[0].TPGs; len(lb) != 0 {
		t.Errorf("loopback tpgs = %+v, want none", lb)
	}
	want := []string{"iscsi"}
	if got := m.Active(); !reflect.DeepEqual(got, want) {
		t.Errorf("Read().Active() = %q, want %q", got, want)
	}
	if got := Active(root); !reflect.DeepEqual(got, want) {
		t.Errorf("Active = %q, want %q", got, want)
	}
}

// A LUN directory without its link yet (mkdir done, ln -s not) is listed
// without a backstore and exports nothing; a mapped LUN without its link
// has no tpg_lun.
func TestUnlinkedLUN(t *testing.T) {
	root := fakeTree(t, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/enable":                             "1\n",
		"iscsi/" + iqn + "/tpgt_1/lun/lun_0/alua_tg_pt_gp":            "default_tg_pt_gp\n",
		"iscsi/" + iqn + "/tpgt_1/acls/iqn.a:one/lun_3/write_protect": "0\n",
	}, nil)
	m := Read(root)
	tp := m.Fabrics[0].Targets[0].TPGs[0]
	if len(tp.LUNs) != 1 || tp.LUNs[0].Backstore != "" {
		t.Errorf("luns = %+v, want lun 0 without a backstore", tp.LUNs)
	}
	if ml := tp.ACLs[0].MappedLUNs; len(ml) != 1 || ml[0].LUN != 3 || ml[0].TPGLUN != nil {
		t.Errorf("mapped luns = %+v, want lun 3 without tpg_lun", ml)
	}
	if got := m.Active(); len(got) != 0 {
		t.Errorf("Read().Active() = %q, want none", got)
	}
	if got := Active(root); len(got) != 0 {
		t.Errorf("Active = %q, want none", got)
	}
}

// An alias with newlines cannot add a connection or change the session.
func TestSessionAliasInjection(t *testing.T) {
	info := "InitiatorName: iqn.1993-08.org.debian:01:backup1\n" +
		"InitiatorAlias: x\nLIO Session ID: 9   ISID: 0x00  TSIH: 9  SessionType: Discovery\n" +
		"Session State: TARG_SESS_STATE_FREE\n----------------------[iSCSI Connections]-------------------------\n" +
		"CID: 7  Connection State: TARG_CONN_STATE_LOGGED_IN\n   Address 6.6.6.6 TCP  StatSN: 0x1\n" +
		sessionInfo[strings.Index(sessionInfo, "LIO Session ID:"):]
	got := parseSession(info)
	if got.Type != "Normal" || got.State != "TARG_SESS_STATE_LOGGED_IN" {
		t.Errorf("session = %+v, want the kernel's Normal / LOGGED_IN", got)
	}
	want := []Connection{{CID: 0, State: "TARG_CONN_STATE_LOGGED_IN", Address: "192.0.2.10"}}
	if !reflect.DeepEqual(got.Connections, want) {
		t.Errorf("connections = %+v, want %+v", got.Connections, want)
	}
	if strings.Contains(got.Alias, "\n") || !strings.HasPrefix(got.Alias, "x LIO Session ID: 9") {
		t.Errorf("alias = %q, want the injected text on one line", got.Alias)
	}
}

// A fabric directory that is not one of the known ones (srpt, ib_srpt's, here) is
// read as transport "other", after the known ones.
func TestOtherFabric(t *testing.T) {
	root := fakeTree(t, map[string]string{
		"version":                                "Target Engine Core ConfigFS Infrastructure v5.0\n",
		"core/alua/lu_gps/default_lu_gp/members": "",
		"srpt/fe80:0000:0000:0000:0002:c903:0007:5f21/tpgt_1/enable": "1\n",
		"iscsi/" + iqn + "/tpgt_1/enable":                            "1\n",
	}, map[string]string{
		"srpt/fe80:0000:0000:0000:0002:c903:0007:5f21/tpgt_1/lun/lun_0/x": "../../../../../../target/core/user_0/tape11",
	})
	m := Read(root)
	if len(m.Fabrics) != 2 || m.Fabrics[0].Name != "iscsi" || m.Fabrics[1].Name != "srpt" || m.Fabrics[1].Transport != "other" {
		t.Fatalf("fabrics = %+v, want iscsi then srpt (other)", m.Fabrics)
	}
	if got := m.Active(); !reflect.DeepEqual(got, []string{"other"}) {
		t.Errorf("Read().Active() = %q, want other", got)
	}
	if got := Active(root); !reflect.DeepEqual(got, []string{"other"}) {
		t.Errorf("Active = %q, want other", got)
	}
}

// The kernel reads lun_<N> with kstrtoull(..., 0, ...)
// (target_fabric_make_lun, target_fabric_make_mappedlun): lun_0x10 is LUN
// 16, lun_010 is LUN 8.
func TestLUNNumberBase(t *testing.T) {
	root := fakeTree(t, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/enable":                                "1\n",
		"iscsi/" + iqn + "/tpgt_1/acls/iqn.a:one/lun_0x10/write_protect": "0\n",
	}, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/lun/lun_0x10/x":            "../../../../../../target/core/user_0/lib10",
		"iscsi/" + iqn + "/tpgt_1/lun/lun_010/x":             "../../../../../../target/core/user_0/tape11",
		"iscsi/" + iqn + "/tpgt_1/acls/iqn.a:one/lun_0x10/m": "../../../../../../iscsi/" + iqn + "/tpgt_1/lun/lun_010",
	})
	tp := Read(root).Fabrics[0].Targets[0].TPGs[0]
	if len(tp.LUNs) != 2 || tp.LUNs[0].LUN != 8 || tp.LUNs[0].Backstore != "tape11" ||
		tp.LUNs[1].LUN != 16 || tp.LUNs[1].Backstore != "lib10" {
		t.Errorf("luns = %+v, want 8 tape11, 16 lib10", tp.LUNs)
	}
	ml := tp.ACLs[0].MappedLUNs
	if len(ml) != 1 || ml[0].LUN != 16 || ml[0].TPGLUN == nil || *ml[0].TPGLUN != 8 {
		t.Errorf("mapped luns = %+v, want 16 -> 8", ml)
	}
	for name, want := range map[string]uint64{"lun_+5": 5, "lun_0x10": 16, "lun_18446744073709551615": 1<<64 - 1} {
		if n, ok := lunNumber(name); !ok || n != want {
			t.Errorf("lunNumber(%q) = %d %v, want %d", name, n, ok, want)
		}
	}
	for _, bad := range []string{"lun_", "lun_x", "lun_-1", "lun_+", "lun_++1", "lun_+-1", "lun_18446744073709551616", "tpgt_1"} {
		if _, ok := lunNumber(bad); ok {
			t.Errorf("lunNumber(%q) ok, want refused", bad)
		}
	}
}

// TestLUNAboveMax: a LUN number the kernel takes (a u64) but JSON cannot
// carry exactly is left out with a warning, not shown as another LUN, and
// the export it belongs to still counts for Active.
func TestLUNAboveMax(t *testing.T) {
	root := fakeTree(t, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/enable":                             "1\n",
		"iscsi/" + iqn + "/tpgt_1/acls/iqn.a:one/lun_0/write_protect": "0\n",
	}, map[string]string{
		"iscsi/" + iqn + "/tpgt_1/lun/lun_0x8000000000000000/x": "../../../../../../target/core/user_0/lib10",
		"iscsi/" + iqn + "/tpgt_1/acls/iqn.a:one/lun_0/m":       "../../../../../../iscsi/" + iqn + "/tpgt_1/lun/lun_0x8000000000000000",
	})
	m := Read(root)
	tp := m.Fabrics[0].Targets[0].TPGs[0]
	if len(tp.LUNs) != 0 {
		t.Errorf("luns = %+v, want none", tp.LUNs)
	}
	if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "lun_0x8000000000000000") || !strings.Contains(m.Warnings[0], "not shown") {
		t.Errorf("warnings = %q, want one naming lun_0x8000000000000000", m.Warnings)
	}
	if got := Active(root); !reflect.DeepEqual(got, []string{"iscsi"}) {
		t.Errorf("Active = %q, want [iscsi]", got)
	}
	if got := m.Active(); !reflect.DeepEqual(got, []string{"iscsi"}) {
		t.Errorf("Read().Active = %q, want [iscsi] like Active", got)
	}
	if want := []HiddenLUN{{Name: "lun_0x8000000000000000", HBA: "user_0", Backstore: "lib10"}}; !reflect.DeepEqual(tp.HiddenLUNs, want) {
		t.Errorf("hidden_luns = %+v, want %+v", tp.HiddenLUNs, want)
	}
	if len(tp.ACLs) != 1 || len(tp.ACLs[0].MappedLUNs) != 0 {
		t.Errorf("acls = %+v, want one with its mapping to the hidden LUN left out", tp.ACLs)
	}
}
