package transports

import (
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
		"loopback/naa.5001405000000001/tpgt_1/nexus":    "naa.50014050000000ff\n",
		"iscsi/discovery_auth/enforce_discovery_auth":   "0\n",
		"iscsi/" + iqn + "/tpgt_1/enable":               "1\n",
		"iscsi/" + iqn + "/tpgt_1/dynamic_sessions":     "",
		"iscsi/" + iqn + "/tpgt_1/np/0.0.0.0:3260/iser": "0\n",
	}
	links := map[string]string{
		"loopback/naa.5001405000000001/tpgt_1/lun/lun_0/b1f2c3": "../../../../../../target/core/user_0/lib10",
	}
	backstores := []string{"lib10", "tape11", "tape12", "tape13", "tape14"}
	for i, bs := range backstores {
		minor := strings.TrimLeft(bs, "libtape")
		files["core/user_0/"+bs+"/enable"] = "1\n"
		files["core/user_0/"+bs+"/attrib/dev_config"] = "mhvtl/" + minor + "\n"
		files["core/user_0/"+bs+"/wwn/vpd_unit_serial"] = "SER" + minor + "\n"
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

func TestReadFCAndUSB(t *testing.T) {
	root := fakeTree(t, map[string]string{
		"qla2xxx/21:00:00:24:ff:31:4c:48/tpgt_1/enable": "1\n",
		"usb_gadget/naa.6001405c3214b06a/tpgt_1/enable": "1\n",
	}, map[string]string{
		"qla2xxx/21:00:00:24:ff:31:4c:48/tpgt_1/lun/lun_0/x": "../../../../../../target/core/user_0/lib10",
	})
	m := Read(root)
	if len(m.Fabrics) != 2 || m.Fabrics[0].Transport != "fc" || m.Fabrics[1].Transport != "usb" {
		t.Fatalf("fabrics = %+v", m.Fabrics)
	}
	if got := m.Active(); !reflect.DeepEqual(got, []string{"fc"}) {
		t.Errorf("active = %q, want fc (the USB TPG has no LUN)", got)
	}
}
