// Package transports reads the LIO target configuration from configfs
// (/sys/kernel/config/target): the TCMU user backstores mhVTL's TCMU handler
// creates, and the fabrics that export them (loopback, iSCSI, Fibre Channel,
// USB gadget) with their LUNs, ACLs and sessions. It only reads; changes go
// through mhVTL's mhvtl-fabrics (ADR 0004).
//
// The layout and file formats are the kernel's, as of Linux v6.12:
//   - core/user_<N>/<name>/{enable,attrib/dev_config,wwn/vpd_unit_serial}
//     (drivers/target/target_core_configfs.c, target_core_user.c
//     tcmu_dev_config_show);
//   - <fabric>/<wwn>/tpgt_<T>/{enable,lun/lun_<N>/<link>,np/<portal>,
//     acls/<initiator>/lun_<M>/{<link>,write_protect}}
//     (drivers/target/target_core_fabric_configfs.c);
//   - iscsi/<iqn>/tpgt_<T>/acls/<initiator>/info and
//     tpgt_<T>/dynamic_sessions (drivers/target/iscsi/iscsi_target_configfs.c
//     lio_target_nacl_info_show, target_core_transport.c
//     target_show_dynamic_sessions).
//
// https://github.com/torvalds/linux/tree/v6.12/drivers/target
package transports

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mhvtl-control/internal/config"
)

// DefaultRoot is where configfs exposes the LIO target.
const DefaultRoot = "/sys/kernel/config/target"

// fabrics are the fabric directories known, with the transport each serves;
// any other directory next to core is a fabric too (srpt, vhost,
// xen-pvscsi, ibmvscsis, sbp, ...) and is read as transport "other".
// loopback (tcm_loop) is how the TCMU handler presents the devices on this
// host. The Fibre Channel modules register these names: tcm_qla2xxx
// qla2xxx and qla2xxx_npiv, efct efct and efct_npiv, tcm_fc (FCoE) fc
// (drivers/scsi/qla2xxx/tcm_qla2xxx.c, drivers/scsi/elx/efct/efct_lio.c,
// drivers/target/tcm_fc/tfc_conf.c).
type fabricDir struct{ dir, transport string }

var fabrics = []fabricDir{
	{"loopback", "local"},
	{"iscsi", "iscsi"},
	{"qla2xxx", "fc"},
	{"qla2xxx_npiv", "fc"},
	{"efct", "fc"},
	{"efct_npiv", "fc"},
	{"fc", "fc"},
	{"usb_gadget", "usb"},
}

// Map is the LIO configuration read from configfs.
type Map struct {
	// ConfigFS is whether the target directory exists (configfs mounted
	// and target_core_mod loaded).
	ConfigFS   bool        `json:"configfs"`
	Backstores []Backstore `json:"backstores"`
	Fabrics    []Fabric    `json:"fabrics"`
	// Warnings name the files that could not be read; what they hold is
	// left out.
	Warnings []string `json:"warnings"`
}

// Backstore is a TCMU user backstore, core/user_<N>/<name>.
type Backstore struct {
	HBA     string `json:"hba"`
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled,omitempty"`
	// DevConfig is attrib/dev_config; mhVTL's handler sets "mhvtl/<minor>"
	// (usr/cmd/mhvtl_tcmu_handler.c), the minor being the device.conf
	// Library or Drive number.
	DevConfig  string `json:"dev_config,omitempty"`
	UnitSerial string `json:"unit_serial,omitempty"`
	// Minor is DevConfig's minor when it is an mhVTL backstore.
	Minor *int `json:"minor,omitempty"`
	// Device is the device.conf device Minor names, when one does.
	Device *Device `json:"device,omitempty"`
}

// Device is a device.conf library or drive.
type Device struct {
	// Kind is "library" or "drive".
	Kind string `json:"kind"`
	ID   int    `json:"id"`
	// LibraryID is a drive's library, absent for a standalone drive.
	LibraryID *int `json:"library_id,omitempty"`
}

// Fabric is one fabric module's configuration.
type Fabric struct {
	// Name is the configfs directory: loopback, iscsi, qla2xxx,
	// qla2xxx_npiv, efct, efct_npiv, fc, usb_gadget, or another module's.
	Name string `json:"name"`
	// Transport is local, iscsi, fc, usb, or other.
	Transport string   `json:"transport"`
	Targets   []Target `json:"targets"`
}

// Target is a fabric WWN: an IQN, an FC port name or a naa. name.
type Target struct {
	WWN  string `json:"wwn"`
	TPGs []TPG  `json:"tpgs"`
}

// TPG is a target portal group, tpgt_<Tag>.
type TPG struct {
	Tag int `json:"tag"`
	// Enabled is absent for fabrics without an enable attribute (loopback).
	Enabled *bool    `json:"enabled,omitempty"`
	Portals []string `json:"portals"`
	LUNs    []LUN    `json:"luns"`
	ACLs    []ACL    `json:"acls"`
	// DynamicSessions are the initiators logged in without an ACL
	// (generate_node_acls); iSCSI and qla2xxx only.
	DynamicSessions []string `json:"dynamic_sessions"`
}

// LUN is lun/lun_<LUN>, linked to a backstore.
type LUN struct {
	LUN       int    `json:"lun"`
	HBA       string `json:"hba,omitempty"`
	Backstore string `json:"backstore,omitempty"`
}

// ACL is an initiator's node ACL.
type ACL struct {
	Initiator  string      `json:"initiator"`
	MappedLUNs []MappedLUN `json:"mapped_luns"`
	// SessionReported is whether the fabric reports the initiator's
	// session: only iSCSI has the ACL info attribute
	// (iscsi_target_configfs.c lio_target_nacl_info_show); Fibre Channel,
	// USB and the others do not, and their Session is always nil.
	SessionReported bool `json:"session_reported"`
	// Session is the initiator's iSCSI session, nil when it has none or
	// SessionReported is false.
	Session *Session `json:"session,omitempty"`
}

// MappedLUN is acls/<initiator>/lun_<LUN>, linked to the TPG's lun_<TPGLUN>.
type MappedLUN struct {
	LUN          int   `json:"lun"`
	TPGLUN       *int  `json:"tpg_lun,omitempty"`
	WriteProtect *bool `json:"write_protect,omitempty"`
}

// Session is an iSCSI session, from the ACL's info file.
type Session struct {
	Alias       string       `json:"alias,omitempty"`
	Type        string       `json:"type,omitempty"`
	State       string       `json:"state,omitempty"`
	Connections []Connection `json:"connections"`
}

// Connection is one connection of a Session.
type Connection struct {
	CID     int    `json:"cid"`
	State   string `json:"state,omitempty"`
	Address string `json:"address,omitempty"`
}

// reader collects the warnings of one Read.
type reader struct {
	root     string
	warnings []string
}

func (r *reader) warn(p string, err error) {
	if rel, e := filepath.Rel(r.root, p); e == nil {
		p = rel
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	r.warnings = append(r.warnings, p+": "+err.Error())
}

// attr reads a configfs attribute. A missing one is no warning: not every
// fabric or kernel has every attribute.
func (r *reader) attr(p string) (string, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.warn(p, err)
		}
		return "", false
	}
	return strings.TrimRight(string(b), "\n\x00 "), true
}

func (r *reader) boolAttr(p string) *bool {
	v, ok := r.attr(p)
	if !ok {
		return nil
	}
	b := v == "1"
	return &b
}

// readDir is os.ReadDir; tests replace it to remove a directory mid-read.
var readDir = os.ReadDir

// list reads directory p. gone is true when p no longer exists: configfs
// entries vanish while an mhvtl-fabrics apply runs, which is no warning.
func (r *reader) list(p string) (ents []fs.DirEntry, gone bool) {
	ents, err := readDir(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true
		}
		r.warn(p, err)
	}
	return ents, false
}

// gone reports whether directory p no longer exists. Checked after an
// object's attributes are read, it leaves out a backstore, target or TPG
// removed meanwhile instead of reporting it empty. It warns about nothing:
// the reads before it already did.
func (r *reader) gone(p string) bool {
	_, err := readDir(p)
	return errors.Is(err, fs.ErrNotExist)
}

// dirs lists the subdirectories of p, sorted by name.
func (r *reader) dirs(p string) []string {
	ents, _ := r.list(p)
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// link returns the target of the first symlink in p ("" when it has none):
// configfs names it as the user chose, so it is found by type. gone is true
// when p, or the link found in it, was removed while it was read.
func (r *reader) link(p string) (target string, gone bool) {
	ents, gone := r.list(p)
	for _, e := range ents {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		t, err := os.Readlink(filepath.Join(p, e.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", true
			}
			r.warn(filepath.Join(p, e.Name()), err)
			continue
		}
		return t, false
	}
	return "", gone
}

// fabricDirs returns the fabric directories under root: the known ones in
// fabrics order, then any other directory except core, sorted, as "other".
func (r *reader) fabricDirs(root string) []fabricDir {
	known := map[string]string{}
	for _, f := range fabrics {
		known[f.dir] = f.transport
	}
	present := map[string]bool{}
	var others []fabricDir
	for _, d := range r.dirs(root) {
		present[d] = true
		if _, ok := known[d]; !ok && d != "core" {
			others = append(others, fabricDir{d, "other"})
		}
	}
	var out []fabricDir
	for _, f := range fabrics {
		if present[f.dir] {
			out = append(out, f)
		}
	}
	return append(out, others...)
}

// numbered parses "<prefix><N>".
func numbered(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(prefix):])
	return n, err == nil && n >= 0
}

// lunNumber parses "lun_<N>" as the kernel does: target_fabric_make_lun and
// target_fabric_make_mappedlun read N with kstrtoull(name + 4, 0, ...), so
// base 0 into a u64, after one optional leading + (lun_0x10 is LUN 16,
// lun_010 is LUN 8, lun_+5 is LUN 5)
// (drivers/target/target_core_fabric_configfs.c,
// https://github.com/torvalds/linux/blob/master/drivers/target/target_core_fabric_configfs.c;
// kstrtoull, lib/kstrtox.c,
// https://github.com/torvalds/linux/blob/master/lib/kstrtox.c).
func lunNumber(name string) (uint64, bool) {
	s, ok := strings.CutPrefix(name, "lun_")
	if !ok {
		return 0, false
	}
	s = strings.TrimPrefix(s, "+")
	if s == "" || s[0] == '+' || s[0] == '-' {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 0, 64)
	return n, err == nil
}

// maxLUN is the largest LUN number reported: JSON readers such as the
// dashboard hold numbers as float64, exact only up to 2^53-1, and int must
// hold it.
var maxLUN = func() uint64 {
	if strconv.IntSize == 32 {
		return math.MaxInt32
	}
	return 1<<53 - 1
}()

// lun parses the lun_<N> entry name under dir for the map. A number the
// kernel accepts but the map cannot carry exactly (above maxLUN) is left out
// with a warning, rather than shown as a different LUN.
func (r *reader) lun(dir, name string) (int, bool) {
	n, ok := lunNumber(name)
	if !ok {
		return 0, false
	}
	if n > maxLUN {
		r.warn(filepath.Join(dir, name), fmt.Errorf("LUN %d is above %d and not shown", n, maxLUN))
		return 0, false
	}
	return int(n), true
}

// Read reads the LIO configuration under root (DefaultRoot when empty). A
// missing root is an empty Map, not an error; unreadable files are left out
// and named in Warnings.
func Read(root string) Map {
	if root == "" {
		root = DefaultRoot
	}
	m := Map{Backstores: []Backstore{}, Fabrics: []Fabric{}, Warnings: []string{}}
	r := &reader{root: root}
	if _, err := os.Stat(root); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.warn(root, err)
			m.Warnings = r.warnings
		}
		return m
	}
	m.ConfigFS = true
	core := filepath.Join(root, "core")
	for _, hba := range r.dirs(core) {
		if _, ok := numbered(hba, "user_"); !ok {
			continue
		}
		for _, name := range r.dirs(filepath.Join(core, hba)) {
			if b, ok := r.backstore(filepath.Join(core, hba, name), hba, name); ok {
				m.Backstores = append(m.Backstores, b)
			}
		}
	}
	for _, f := range r.fabricDirs(root) {
		dir := filepath.Join(root, f.dir)
		fab := Fabric{Name: f.dir, Transport: f.transport, Targets: []Target{}}
		for _, wwn := range r.dirs(dir) {
			// The fabric module creates discovery_auth next to the targets
			// (iSCSI and loopback both have one); it is not a target.
			if wwn == "discovery_auth" {
				continue
			}
			t := Target{WWN: wwn, TPGs: []TPG{}}
			for _, tp := range r.dirs(filepath.Join(dir, wwn)) {
				if tag, ok := numbered(tp, "tpgt_"); ok {
					if g, ok := r.tpg(filepath.Join(dir, wwn, tp), tag); ok {
						t.TPGs = append(t.TPGs, g)
					}
				}
			}
			if !r.gone(filepath.Join(dir, wwn)) {
				fab.Targets = append(fab.Targets, t)
			}
		}
		m.Fabrics = append(m.Fabrics, fab)
	}
	if r.warnings != nil {
		m.Warnings = r.warnings
	}
	return m
}

// backstore reads a TCMU backstore; ok is false when it was removed while
// it was read.
func (r *reader) backstore(p, hba, name string) (b Backstore, ok bool) {
	b = Backstore{HBA: hba, Name: name, Enabled: r.boolAttr(filepath.Join(p, "enable"))}
	b.DevConfig, _ = r.attr(filepath.Join(p, "attrib", "dev_config"))
	// target_wwn_vpd_unit_serial_show() prints "T10 VPD Unit Serial Number: %s"
	serial, _ := r.attr(filepath.Join(p, "wwn", "vpd_unit_serial"))
	// attr trims the space an empty serial leaves after the colon.
	b.UnitSerial = strings.TrimSpace(strings.TrimPrefix(serial, "T10 VPD Unit Serial Number:"))
	if n, ok := numbered(b.DevConfig, "mhvtl/"); ok {
		b.Minor = &n
	}
	return b, !r.gone(p)
}

// tpg reads a target portal group; ok is false when it was removed while
// it was read.
func (r *reader) tpg(p string, tag int) (TPG, bool) {
	t := TPG{Tag: tag, Enabled: r.boolAttr(filepath.Join(p, "enable")),
		Portals: []string{}, LUNs: []LUN{}, ACLs: []ACL{}, DynamicSessions: []string{}}
	t.Portals = append(t.Portals, r.dirs(filepath.Join(p, "np"))...)
	for _, name := range r.dirs(filepath.Join(p, "lun")) {
		n, ok := r.lun(filepath.Join(p, "lun"), name)
		if !ok {
			continue
		}
		l := LUN{LUN: n}
		// .../core/<hba>/<backstore>
		dst, gone := r.link(filepath.Join(p, "lun", name))
		if gone {
			continue
		}
		if dst != "" {
			l.HBA, l.Backstore = filepath.Base(filepath.Dir(dst)), filepath.Base(dst)
		}
		t.LUNs = append(t.LUNs, l)
	}
	sort.Slice(t.LUNs, func(i, j int) bool { return t.LUNs[i].LUN < t.LUNs[j].LUN })
	for _, init := range r.dirs(filepath.Join(p, "acls")) {
		if a, ok := r.acl(filepath.Join(p, "acls", init), init); ok {
			t.ACLs = append(t.ACLs, a)
		}
	}
	if s, ok := r.attr(filepath.Join(p, "dynamic_sessions")); ok {
		// One "<initiator>\n\0" per session.
		for _, line := range strings.FieldsFunc(s, func(c rune) bool { return c == '\n' || c == 0 }) {
			if line = strings.TrimSpace(line); line != "" {
				t.DynamicSessions = append(t.DynamicSessions, line)
			}
		}
	}
	return t, !r.gone(p)
}

// acl reads an initiator's node ACL; ok is false when it was removed while
// it was read.
func (r *reader) acl(p, init string) (a ACL, ok bool) {
	a = ACL{Initiator: init, MappedLUNs: []MappedLUN{}}
	ents, gone := r.list(p)
	if gone {
		return a, false
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		n, ok := r.lun(p, e.Name())
		if !ok {
			continue
		}
		name := e.Name()
		ml := MappedLUN{LUN: n, WriteProtect: r.boolAttr(filepath.Join(p, name, "write_protect"))}
		// .../lun/lun_<N>; none yet while an apply links it
		dst, gone := r.link(filepath.Join(p, name))
		if gone {
			continue
		}
		if tl, ok := r.lun(filepath.Dir(dst), filepath.Base(dst)); ok {
			ml.TPGLUN = &tl
		}
		a.MappedLUNs = append(a.MappedLUNs, ml)
	}
	sort.Slice(a.MappedLUNs, func(i, j int) bool { return a.MappedLUNs[i].LUN < a.MappedLUNs[j].LUN })
	if info, ok := r.attr(filepath.Join(p, "info")); ok {
		a.SessionReported = true
		a.Session = parseSession(info)
	}
	// Removed while it was read: its reads found nothing, so it would show
	// as an initiator allowed in with no LUNs.
	return a, !r.gone(p)
}

// parseSession parses an iSCSI ACL's info file (lio_target_nacl_info_show):
// "No active iSCSI Session ..." or
//
//	InitiatorName: <iqn>
//	InitiatorAlias: <alias>
//	LIO Session ID: 1   ISID: 0x... TSIH: 1  SessionType: Normal
//	Session State: TARG_SESS_STATE_LOGGED_IN
//	...
//	----------------------[iSCSI Connections]-------------------------
//	CID: 0  Connection State: TARG_CONN_STATE_LOGGED_IN
//	   Address 192.0.2.10 TCP  StatSN: 0x...
//
// The alias is the initiator's to choose, and LIO does not refuse a newline
// in it (TYPERANGE_UTF8), so an alias could carry lines that look like the
// kernel's. Everything up to the last "LIO Session ID:" line is the name and
// alias; the session is read only after it, and connections only after the
// connections header.
func parseSession(info string) *Session {
	if strings.HasPrefix(info, "No active iSCSI Session") {
		return nil
	}
	s := &Session{Connections: []Connection{}}
	head, rest := "", info
	if i := strings.LastIndex(info, "\nLIO Session ID:"); i >= 0 {
		head, rest = info[:i], info[i+1:]
	}
	if i := strings.Index(head, "InitiatorAlias:"); i >= 0 {
		s.Alias = strings.Join(strings.Fields(head[i+len("InitiatorAlias:"):]), " ")
	}
	conns := false
	for _, line := range strings.Split(rest, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.Contains(t, "[iSCSI Connections]"):
			conns = true
		case !conns && strings.HasPrefix(t, "LIO Session ID:"):
			if i := strings.Index(t, "SessionType:"); i >= 0 {
				s.Type = strings.TrimSpace(t[i+len("SessionType:"):])
			}
		case !conns && strings.HasPrefix(t, "Session State:"):
			s.State = strings.TrimSpace(strings.TrimPrefix(t, "Session State:"))
		case conns && strings.HasPrefix(t, "CID:"):
			c := Connection{}
			f := strings.Fields(t)
			if len(f) > 1 {
				c.CID, _ = strconv.Atoi(f[1])
			}
			if i := strings.Index(t, "Connection State:"); i >= 0 {
				c.State = strings.TrimSpace(t[i+len("Connection State:"):])
			}
			s.Connections = append(s.Connections, c)
		case conns && strings.HasPrefix(t, "Address "):
			if f := strings.Fields(t); len(f) > 1 && len(s.Connections) > 0 {
				s.Connections[len(s.Connections)-1].Address = f[1]
			}
		}
	}
	return s
}

// Resolve sets each mhVTL backstore's Device from inv: the minor is the
// device.conf Library or Drive number (usr/transport_tcmu.c names the
// backstore lib<minor> or tape<minor>).
func (m *Map) Resolve(inv config.Inventory) {
	devs := map[int]*Device{}
	for _, lib := range inv.Libraries {
		devs[lib.ID] = &Device{Kind: "library", ID: lib.ID}
		for _, d := range lib.Drives {
			id := lib.ID
			devs[d.ID] = &Device{Kind: "drive", ID: d.ID, LibraryID: &id}
		}
	}
	for _, d := range inv.Standalone {
		devs[d.ID] = &Device{Kind: "drive", ID: d.ID}
	}
	for i := range m.Backstores {
		if b := &m.Backstores[i]; b.Minor != nil {
			b.Device = devs[*b.Minor]
		}
	}
}

// Active returns the transports that export something: a fabric with a
// TPG, not disabled, that has a LUN linked to a backstore. Each is named
// once, in fabricDirs order.
func (m Map) Active() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, f := range m.Fabrics {
		if seen[f.Transport] || !f.exports() {
			continue
		}
		seen[f.Transport] = true
		out = append(out, f.Transport)
	}
	return out
}

// Active returns what Map.Active returns for the configuration under root
// (DefaultRoot when empty), reading only the fabric, target, TPG and LUN
// directories and stopping at the first LUN each transport exports. GET
// /system calls it on every refresh, where Read would also walk every ACL
// and session.
func Active(root string) []string {
	if root == "" {
		root = DefaultRoot
	}
	r := &reader{root: root}
	out := []string{}
	seen := map[string]bool{}
	for _, f := range r.fabricDirs(root) {
		if seen[f.transport] {
			continue
		}
		dir := filepath.Join(root, f.dir)
		if r.fabricExports(dir) {
			seen[f.transport] = true
			out = append(out, f.transport)
		}
	}
	return out
}

func (r *reader) fabricExports(dir string) bool {
	for _, wwn := range r.dirs(dir) {
		if wwn == "discovery_auth" {
			continue
		}
		for _, tp := range r.dirs(filepath.Join(dir, wwn)) {
			if _, ok := numbered(tp, "tpgt_"); !ok {
				continue
			}
			p := filepath.Join(dir, wwn, tp)
			if en := r.boolAttr(filepath.Join(p, "enable")); en != nil && !*en {
				continue
			}
			for _, l := range r.dirs(filepath.Join(p, "lun")) {
				if _, ok := lunNumber(l); !ok {
					continue
				}
				// Read leaves out a TPG or target removed meanwhile.
				if dst, _ := r.link(filepath.Join(p, "lun", l)); dst != "" &&
					!r.gone(p) && !r.gone(filepath.Join(dir, wwn)) {
					return true
				}
			}
		}
	}
	return false
}

func (f Fabric) exports() bool {
	for _, t := range f.Targets {
		for _, tp := range t.TPGs {
			if tp.Enabled != nil && !*tp.Enabled {
				continue
			}
			for _, l := range tp.LUNs {
				if l.Backstore != "" {
					return true
				}
			}
		}
	}
	return false
}
