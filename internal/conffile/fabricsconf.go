package conffile

// fabrics.conf, read by mhvtl-fabrics (fabrics.conf(5)). Citations refer to
// upstream mhVTL master 19b71bd; the parser mirrored here is fab_conf_parse
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L350-L462),
// the planner fab_plan
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L537-L611).

import (
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strings"
)

// Limits from include/fabrics.h
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/include/fabrics.h#L20-L30),
// and fab_conf_parse's line and field-name buffers
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L352).
const (
	fabNameMax       = 224
	fabSecretMax     = 256
	fabAddrMax       = 64
	fabMaxPortals    = 16
	fabMaxInitiators = 32
	fabMaxTargets    = 32
	fabMaxLUNs       = 64
	fabMaxDevs       = 256
	fabLineBuf       = 1024
	fabFieldNameBuf  = 64
)

// Record kinds. fabBad is a column-1 line upstream rejects (unknown record,
// second iSCSI record, bad or repeated target ID); its fields are kept but
// not checked, the header's Problem covers them.
const (
	fabVersion = iota + 1
	fabISCSI
	fabTarget
	fabBad
)

// chapFields are the CHAP field names in fab_auth order.
var chapFields = [4]string{"chap name", "chap secret", "mutual chap name", "mutual chap secret"}

// fabCanonical is the spelling of each field in fabrics.conf(5), used for
// lines an edit adds.
var fabCanonical = map[string]string{
	"portal":             "Portal",
	"base name":          "Base name",
	"initiator name":     "Initiator name",
	"target name":        "Target name",
	"chap name":          "CHAP name",
	"chap secret":        "CHAP secret",
	"mutual chap name":   "Mutual CHAP name",
	"mutual chap secret": "Mutual CHAP secret",
}

// fabField is one indented "Name: value" line: name lower-cased with blanks
// collapsed, value without comment and trimmed, as split_field returns them.
type fabField struct {
	name, value string
	line        int // index into the file's lines
}

type fabPortal struct {
	text, addr string
	port       int64
	ipv6       bool
}

// fabRecord is one column-1 record and the field lines up to the next one.
// The other fields hold what upstream keeps of it: only values it accepts,
// the last one for a field given twice (upstream overwrites).
type fabRecord struct {
	kind   int
	id     int64
	header int // index of the header line
	fields []fabField

	portals    []fabPortal // iSCSI
	baseName   string      // iSCSI
	initiators []string    // target
	wwn        string      // target: Target name
	auth       [4]string   // target: chapFields values
}

// FabricsConf is a lossless fabrics.conf. Parsing never fails; every line
// fab_conf_parse would reject is a Problem (none is Ambiguous: upstream
// refuses the whole file on any of them).
//
// Upstream stops at its first error; the model goes on, skipping the bad
// line (or the fields of a bad record), so Problems[0] is the error
// mhvtl-fabrics reports. Problem messages are upstream's, with the line
// numbers of this file, and never contain a CHAP name or secret.
type FabricsConf struct {
	lines    []Line
	records  []fabRecord
	Problems []Problem
}

// ParseFabricsConf parses fabrics.conf.
func ParseFabricsConf(data []byte) *FabricsConf {
	c := &FabricsConf{lines: splitLines(data)}
	c.index()
	return c
}

// Writable is DeviceConf.Writable for fabrics.conf.
func (c *FabricsConf) Writable() error { return writable("fabrics.conf", c.Problems) }

// Render returns the file's bytes.
func (c *FabricsConf) Render() []byte { return renderLines(c.lines) }

// fabLineProblem is fab_conf_parse's fgets check
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L359-L366):
// a line that does not fit the 1024-byte buffer with its '\n' is refused,
// and so is one with a NUL, where strchr stops looking for the '\n'. (On a
// last line without a newline upstream reads up to the NUL instead; that
// is flagged too.)
func fabLineProblem(l Line) string {
	n := len(l.Text)
	if l.EOL == "\r\n" {
		n++
	}
	nl := l.EOL != ""
	if nl {
		n++
	}
	switch {
	case strings.IndexByte(l.Text, 0) >= 0:
		return "line contains a NUL byte: mhvtl-fabrics reads it as too long"
	case n > fabLineBuf-1 || (n == fabLineBuf-1 && !nl):
		return "line too long"
	}
	return ""
}

func fabIndent(text string) int {
	p := 0
	for p < len(text) && (text[p] == ' ' || text[p] == '\t') {
		p++
	}
	return p
}

// isFabComment reports a comment line: '#' after the indentation.
func isFabComment(text string) bool {
	p := fabIndent(text)
	return p < len(text) && text[p] == '#'
}

// splitFabField is split_field
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L52-L90)
// on a line without indentation or trailing white space: a '#' after a blank
// starts a comment; the name, up to the first ':', is lower-cased with each
// run of white space one space (and cut at 63 bytes, the size of its
// buffer); the value is trimmed.
func splitFabField(rest string) (name, value string, ok bool) {
	for k := 0; k+1 < len(rest); k++ {
		if (rest[k] == ' ' || rest[k] == '\t') && rest[k+1] == '#' {
			rest = rest[:k]
			break
		}
	}
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", "", false
	}
	var b []byte
	blank := false
	for k := 0; k < colon && len(b)+1 < fabFieldNameBuf; k++ {
		ch := rest[k]
		if isCSpace(rune(ch)) {
			blank = len(b) > 0
			continue
		}
		if blank && len(b)+2 < fabFieldNameBuf {
			b = append(b, ' ')
		}
		blank = false
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		b = append(b, ch)
	}
	return string(b), cTrim(rest[colon+1:]), true
}

// cStrtol is strtol(s, &end, 10): leading white space, a sign, digits,
// clamped on overflow. ok is false when there are no digits, and rest is
// then all of s.
func cStrtol(s string) (v int64, rest string, ok bool) {
	i := 0
	for i < len(s) && isCSpace(rune(s[i])) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var u uint64
	over := false
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		d := uint64(s[i] - '0')
		if u > (math.MaxUint64-d)/10 {
			over = true
		} else {
			u = u*10 + d
		}
	}
	if i == start {
		return 0, s, false
	}
	switch {
	case neg && (over || u > 1<<63):
		v = math.MinInt64
	case neg:
		v = -int64(u)
	case over || u > math.MaxInt64:
		v = math.MaxInt64
	default:
		v = int64(u)
	}
	return v, s[i:], true
}

// parseFabID is parse_id
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L210-L216).
func parseFabID(s string) (int64, bool) {
	v, rest, _ := cStrtol(s)
	return v, s != "" && strings.TrimRightFunc(rest, isCSpace) == "" && v >= 0
}

// ValidISCSIName is fab_valid_iscsi_name
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L104-L123):
// an iqn., eui. or naa. name of 5 to 223 bytes, of lower-case letters,
// digits, '.', '-' and ':' only.
func ValidISCSIName(s string) bool {
	if !strings.HasPrefix(s, "iqn.") && !strings.HasPrefix(s, "eui.") && !strings.HasPrefix(s, "naa.") {
		return false
	}
	if len(s) > fabNameMax-1 || len(s) <= 4 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == ':') {
			return false
		}
	}
	return true
}

// validCHAP is valid_chap
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L125-L139):
// non-empty, printable ASCII without blanks (isgraph), and not starting
// with "NULL", which clears the value in LIO.
func validCHAP(s string) bool {
	if s == "" || strings.HasPrefix(s, "NULL") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// parsePortal is parse_portal
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L141-L184):
// "192.0.2.10", "192.0.2.10:3260", "[2001:db8::1]:3260" or "[2001:db8::1]",
// the address as inet_pton reads it, the port 1 to 65535 (default 3260).
func parsePortal(s string) (fabPortal, bool) {
	if len(s) >= fabAddrMax+8 {
		return fabPortal{}, false
	}
	p := fabPortal{text: s, port: 3260}
	var port string
	hasPort := false
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return fabPortal{}, false
		}
		switch after := s[end+1:]; {
		case strings.HasPrefix(after, ":"):
			port, hasPort = after[1:], true
		case after != "":
			return fabPortal{}, false
		}
		p.addr, p.ipv6 = s[1:end], true
		if a, err := netip.ParseAddr(p.addr); err != nil || !a.Is6() || a.Zone() != "" {
			return fabPortal{}, false
		}
	} else {
		p.addr = s
		if k := strings.IndexByte(s, ':'); k >= 0 {
			p.addr, port, hasPort = s[:k], s[k+1:], true
		}
		if a, err := netip.ParseAddr(p.addr); err != nil || !a.Is4() {
			return fabPortal{}, false
		}
	}
	if hasPort {
		n, rest, _ := cStrtol(port)
		if port == "" || rest != "" || n < 1 || n > 65535 {
			return fabPortal{}, false
		}
		p.port = n
	}
	return p, true
}

// portalAny is portal_any
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L186-L189):
// the wildcard address as written, not as parsed, so "[0::0]" is no
// wildcard.
func portalAny(p fabPortal) bool {
	if p.ipv6 {
		return p.addr == "::"
	}
	return p.addr == "0.0.0.0"
}

// portalConflict is portal_conflict
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L191-L208):
// same family and port, and the same address text or either a wildcard.
func (r *fabRecord) portalConflict(p fabPortal) bool {
	for _, q := range r.portals {
		if q.port != p.port || q.ipv6 != p.ipv6 {
			continue
		}
		if q.addr == p.addr || portalAny(q) || portalAny(p) {
			return true
		}
	}
	return false
}

// iscsiField is iscsi_field
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L218-L256).
// It returns upstream's error, or "".
func (r *fabRecord) iscsiField(name, value string) string {
	switch name {
	case "portal":
		if len(r.portals) == fabMaxPortals {
			return fmt.Sprintf("more than %d portals", fabMaxPortals)
		}
		p, ok := parsePortal(value)
		if !ok {
			return fmt.Sprintf("portal '%s' is not ADDRESS[:PORT] or [IPv6]:PORT", value)
		}
		if r.portalConflict(p) {
			return fmt.Sprintf("portal '%s' repeats or overlaps another portal on port %d", value, p.port)
		}
		r.portals = append(r.portals, p)
	case "base name":
		// copy_value cannot fail on a valid name: it is shorter than
		// FAB_NAME_MAX.
		if !ValidISCSIName(value) {
			return fmt.Sprintf("base name '%s' is not an iSCSI name", value)
		}
		r.baseName = value
	default:
		return fmt.Sprintf("unknown iSCSI field '%s'", name)
	}
	return ""
}

// targetField is target_field
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L258-L319).
// Its messages name the field, never a CHAP value.
func (r *fabRecord) targetField(name, value string) string {
	switch name {
	case "initiator name":
		if !ValidISCSIName(value) {
			return fmt.Sprintf("initiator name '%s' is not an iSCSI name (lower case iqn., eui. or naa.)", value)
		}
		for _, i := range r.initiators {
			if i == value {
				return fmt.Sprintf("initiator %s is listed twice", value)
			}
		}
		if len(r.initiators) == fabMaxInitiators {
			return fmt.Sprintf("more than %d initiators", fabMaxInitiators)
		}
		r.initiators = append(r.initiators, value)
		return ""
	case "target name":
		if !ValidISCSIName(value) {
			return fmt.Sprintf("target name '%s' is not an iSCSI name", value)
		}
		r.wwn = value
		return ""
	}
	for k, f := range chapFields {
		if name != f {
			continue
		}
		if !validCHAP(value) {
			return fmt.Sprintf("%s must be printable, without blanks, and not start with NULL", f)
		}
		if len(value) >= fabSecretMax {
			return fmt.Sprintf("%s is longer than %d bytes", f, fabSecretMax-1)
		}
		r.auth[k] = value
		return ""
	}
	return fmt.Sprintf("unknown iSCSI target field '%s'", name)
}

// checkTarget is check_target
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L321-L348).
func (r *fabRecord) checkTarget() string {
	a := r.auth
	switch {
	case (a[0] != "") != (a[1] != ""):
		return fmt.Sprintf("target %d: CHAP name and CHAP secret go together", r.id)
	case (a[2] != "") != (a[3] != ""):
		return fmt.Sprintf("target %d: mutual CHAP name and secret go together", r.id)
	case a[2] != "" && a[0] == "":
		return fmt.Sprintf("target %d: mutual CHAP needs CHAP", r.id)
	case a[3] != "" && a[1] == a[3]:
		// RFC 7143 section 9.2.1: the two directions use different secrets
		return fmt.Sprintf("target %d: mutual CHAP secret must differ from the CHAP secret", r.id)
	}
	return ""
}

// index rebuilds the records from the lines, as fab_conf_parse reads them
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L350-L462).
func (c *FabricsConf) index() {
	c.records, c.Problems = nil, nil
	problem := func(i int, format string, a ...any) {
		c.Problems = append(c.Problems, Problem{Line: i + 1, Msg: fmt.Sprintf(format, a...)})
	}
	cur, iscsi, targets := -1, -1, 0
	for i, l := range c.lines {
		if msg := fabLineProblem(l); msg != "" {
			problem(i, "%s", msg)
			continue
		}
		text := cText(l)
		p := fabIndent(text)
		rest := strings.TrimRightFunc(text[p:], isCSpace)
		if rest == "" || rest[0] == '#' {
			continue
		}
		name, value, ok := splitFabField(rest)
		if !ok {
			problem(i, "expected 'Name: value'")
			continue
		}
		if p == 0 { // column 1: a new record (L377-L423)
			r := fabRecord{kind: fabBad, header: i}
			switch {
			case name == "version":
				if value != "1" {
					problem(i, "unsupported VERSION %s", value)
				}
				r.kind = fabVersion
			case name == "iscsi" && value == "":
				if iscsi >= 0 {
					problem(i, "second iSCSI record")
					break
				}
				r.kind, iscsi = fabISCSI, len(c.records)
			case name == "iscsi target":
				id, ok := parseFabID(value)
				dup := false
				for _, o := range c.records {
					dup = dup || (o.kind == fabTarget && o.id == id)
				}
				switch {
				case !ok:
					problem(i, "iSCSI target needs a device.conf Library or Drive ID")
				case dup:
					problem(i, "second iSCSI target record for %d", id)
				case targets == fabMaxTargets:
					problem(i, "more than %d targets", fabMaxTargets)
				default:
					r.kind, r.id = fabTarget, id
					targets++
				}
			default:
				problem(i, "unknown record '%s'", name)
			}
			c.records = append(c.records, r)
			cur = len(c.records) - 1
			continue
		}
		if cur < 0 {
			problem(i, "field '%s' outside a record", name)
			continue
		}
		r := &c.records[cur]
		r.fields = append(r.fields, fabField{name: name, value: value, line: i})
		switch r.kind {
		case fabVersion: // VERSION ends the record before it (L385)
			problem(i, "field '%s' outside a record", name)
		case fabISCSI:
			if msg := r.iscsiField(name, value); msg != "" {
				problem(i, "%s", msg)
			}
		case fabTarget:
			if value == "" {
				problem(i, "%s has no value", name)
			} else if msg := r.targetField(name, value); msg != "" {
				problem(i, "%s", msg)
			}
		}
	}

	// L445-L460. Upstream reports "need an iSCSI record" with no line; it
	// is put on the first target here.
	first := -1
	for k := range c.records {
		t := &c.records[k]
		if t.kind != fabTarget {
			continue
		}
		if msg := t.checkTarget(); msg != "" {
			problem(t.header, "%s", msg)
		}
		for _, o := range c.records[:k] {
			if o.kind == fabTarget && t.wwn != "" && t.wwn == o.wwn {
				problem(t.header, "target name %s is used twice", t.wwn)
				break
			}
		}
		if first < 0 {
			first = t.header
		}
	}
	if first >= 0 && iscsi < 0 {
		problem(first, "iSCSI target records need an iSCSI record")
	}
}

func (c *FabricsConf) find(kind int, id int64) *fabRecord {
	for k := range c.records {
		if r := &c.records[k]; r.kind == kind && (kind != fabTarget || r.id == id) {
			return r
		}
	}
	return nil
}

// ISCSI returns the iSCSI record: its portals as written, in file order,
// and its base name. present is false when there is no iSCSI record. On a
// file with Problems, only the values upstream accepted are returned.
func (c *FabricsConf) ISCSI() (portals []string, baseName string, present bool) {
	r := c.find(fabISCSI, 0)
	if r == nil {
		return nil, "", false
	}
	for _, p := range r.portals {
		portals = append(portals, p.text)
	}
	return portals, r.baseName, true
}

// FabricsTarget is one "iSCSI target:" record. CHAP secrets are not part of
// the read model.
type FabricsTarget struct {
	ID             int
	Line           int // 1-based line of the record header
	Initiators     []string
	TargetName     string // "" when the name is derived from the Base name
	CHAPName       string
	HasCHAP        bool
	MutualCHAPName string
	HasMutualCHAP  bool
}

// Targets returns the iSCSI target records in file order (a repeated ID
// only once, its first record).
func (c *FabricsConf) Targets() []FabricsTarget {
	var out []FabricsTarget
	for _, r := range c.records {
		if r.kind != fabTarget {
			continue
		}
		out = append(out, FabricsTarget{
			ID:             int(r.id),
			Line:           r.header + 1,
			Initiators:     append([]string(nil), r.initiators...),
			TargetName:     r.wwn,
			CHAPName:       r.auth[0],
			HasCHAP:        r.auth[0] != "" && r.auth[1] != "",
			MutualCHAPName: r.auth[2],
			HasMutualCHAP:  r.auth[2] != "" && r.auth[3] != "",
		})
	}
	return out
}

// HasSecret reports a CHAP or mutual CHAP secret in any target: upstream
// then refuses the file unless it is mode 0600 (has_secret,
// https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L312-L313).
func (c *FabricsConf) HasSecret() bool {
	for _, r := range c.records {
		if r.kind == fabTarget && (r.auth[1] != "" || r.auth[3] != "") {
			return true
		}
	}
	return false
}

// ---- edits ----

// CHAPCreds is a CHAP name and secret; both are required.
type CHAPCreds struct{ Name, Secret string }

// TargetEdit is a change to one "iSCSI target:" record. A nil field is left
// as it is.
type TargetEdit struct {
	Initiators      []string   // nil: unchanged; empty: remove all
	TargetName      *string    // nil: unchanged; "": remove
	CHAP            *CHAPCreds // nil: unchanged
	ClearCHAP       bool       // removes CHAP and mutual CHAP
	MutualCHAP      *CHAPCreds // nil: unchanged
	ClearMutualCHAP bool
}

// fabEdit collects line changes against the current lines, applied in one
// pass so indices stay those of the parse.
type fabEdit struct {
	c   *FabricsConf
	del map[int]bool
	set map[int]string
	ins map[int][]string // new lines inserted before this index
}

func (e *fabEdit) insert(at int, texts ...string) { e.ins[at] = append(e.ins[at], texts...) }

// apply builds the new lines. A missing final newline stays missing, as
// with insertLines and deleteLines.
func (e *fabEdit) apply() {
	c := e.c
	nl := eol(c.lines)
	noFinal := len(c.lines) > 0 && c.lines[len(c.lines)-1].EOL == ""
	var out []Line
	for i := 0; i <= len(c.lines); i++ {
		for _, t := range e.ins[i] {
			out = append(out, Line{Text: t, EOL: nl})
		}
		if i == len(c.lines) || e.del[i] {
			continue
		}
		l := c.lines[i]
		if t, ok := e.set[i]; ok {
			l.Text = t
		}
		out = append(out, l)
	}
	if noFinal && len(out) > 0 {
		for k := range out {
			if out[k].EOL == "" {
				out[k].EOL = nl
			}
		}
		out[len(out)-1].EOL = ""
	}
	c.lines = out
}

// edit runs an edit and keeps it only if the result parses without
// Problems; otherwise the file is left as it was and the first Problem of
// the edited text is returned. An edit on a file that already has Problems
// is refused.
func (c *FabricsConf) edit(f func(e *fabEdit) error) error {
	if err := c.Writable(); err != nil {
		return err
	}
	e := &fabEdit{c: c, del: map[int]bool{}, set: map[int]string{}, ins: map[int][]string{}}
	if err := f(e); err != nil {
		return fmt.Errorf("fabrics.conf: %w", err)
	}
	old := c.lines
	e.apply()
	c.index()
	if len(c.Problems) > 0 {
		p := c.Problems[0]
		c.lines = old
		c.index()
		return fmt.Errorf("fabrics.conf: edit refused, the result would have %w", p)
	}
	return nil
}

func fieldText(name, value string) string { return " " + fabCanonical[name] + ": " + value }

// checkFabValue refuses a value the file cannot carry as written: control
// characters (a line break would start a new line), leading or trailing
// blanks (trimmed), or a '#' after a blank (a comment). The message never
// contains the value, which may be a secret. What upstream would refuse
// beyond that is caught by parsing the edited file.
func checkFabValue(name, v string) error {
	for i := 0; i < len(v); i++ {
		if v[i] < ' ' || v[i] == 0x7f {
			return fmt.Errorf("invalid %s: contains a control character", fabCanonical[name])
		}
	}
	switch {
	case strings.Trim(v, " ") != v:
		return fmt.Errorf("invalid %s: leading or trailing blanks", fabCanonical[name])
	case strings.Contains(v, " #"):
		return fmt.Errorf("invalid %s: a '#' after a blank starts a comment", fabCanonical[name])
	}
	return nil
}

func checkFabList(name string, vs []string) error {
	seen := map[string]bool{}
	for _, v := range vs {
		if err := checkFabValue(name, v); err != nil {
			return err
		}
		if seen[v] {
			return fmt.Errorf("%s %s is listed twice", fabCanonical[name], v)
		}
		seen[v] = true
	}
	return nil
}

func checkCreds(kind string, cr *CHAPCreds) error {
	if cr == nil {
		return nil
	}
	if cr.Name == "" || cr.Secret == "" {
		return fmt.Errorf("%s needs both a name and a secret", kind)
	}
	if err := checkFabValue(kind+" name", cr.Name); err != nil {
		return err
	}
	return checkFabValue(kind+" secret", cr.Secret)
}

// fabValueSpan locates the value of a field line: the first ':', and the
// value between the blanks after it and the blanks before a comment or the
// end of the line.
func fabValueSpan(text string) (colon, vs, ve int) {
	p := fabIndent(text)
	cut := len(text)
	for k := p; k+1 < len(text); k++ {
		if (text[k] == ' ' || text[k] == '\t') && text[k+1] == '#' {
			cut = k
			break
		}
	}
	colon = p + strings.IndexByte(text[p:cut], ':')
	vs = colon + 1
	for vs < cut && isCSpace(rune(text[vs])) {
		vs++
	}
	ve = cut
	for ve > vs && isCSpace(rune(text[ve-1])) {
		ve--
	}
	return colon, vs, ve
}

// rewriteValue replaces a field line's value, keeping its indentation,
// name spelling, spacing and trailing comment.
func rewriteValue(text, value string) string {
	colon, vs, ve := fabValueSpan(text)
	prefix := text[:vs]
	if vs == colon+1 {
		prefix += " "
	}
	return prefix + value + text[ve:]
}

// end is the index after the record's last field line (after the header
// when it has none): where new fields go.
func (r *fabRecord) end() int {
	end := r.header + 1
	for _, f := range r.fields {
		if f.line+1 > end {
			end = f.line + 1
		}
	}
	return end
}

// setMulti makes the record's lines of a repeatable field hold values: the
// lines of values that stay are kept with their comments, the others go,
// and new values are added after the field's last line.
func (e *fabEdit) setMulti(r *fabRecord, name string, values []string) {
	want := map[string]bool{}
	for _, v := range values {
		want[v] = true
	}
	at := -1
	for _, f := range r.fields {
		if f.name != name {
			continue
		}
		at = f.line + 1
		if want[f.value] {
			delete(want, f.value)
		} else {
			e.del[f.line] = true
		}
	}
	if at < 0 {
		at = r.end()
	}
	for _, v := range values {
		if want[v] {
			e.insert(at, fieldText(name, v))
			delete(want, v)
		}
	}
}

// setSingle sets a single-valued field: "" removes every line of it; else
// the last line (the one upstream keeps) gets the value and earlier ones go.
func (e *fabEdit) setSingle(r *fabRecord, name, value string) {
	var lines []int
	for _, f := range r.fields {
		if f.name == name {
			lines = append(lines, f.line)
		}
	}
	if value == "" {
		for _, k := range lines {
			e.del[k] = true
		}
		return
	}
	if len(lines) == 0 {
		e.insert(r.end(), fieldText(name, value))
		return
	}
	for _, k := range lines[:len(lines)-1] {
		e.del[k] = true
	}
	k := lines[len(lines)-1]
	e.set[k] = rewriteValue(e.c.lines[k].Text, value)
}

// appendRecord adds a record at the end of the file, after a blank line.
func (e *fabEdit) appendRecord(texts []string) {
	n := len(e.c.lines)
	if n > 0 && !isBlank(e.c.lines[n-1].Text) {
		texts = append([]string{""}, texts...)
	}
	e.insert(n, texts...)
}

// leadingStart is the first line of the comment block right above a
// header, with no blank line between (the header itself when there is
// none).
func (c *FabricsConf) leadingStart(header int) int {
	k := header
	for k > 0 && isFabComment(c.lines[k-1].Text) {
		k--
	}
	return k
}

// SetISCSI edits the iSCSI record. portals nil leaves the Portal lines,
// an empty slice removes them all; baseName nil leaves the Base name, ""
// removes it. A missing iSCSI record is created after the VERSION line, or
// else at the end of the file (in a valid file without an iSCSI record,
// only VERSION lines can be records, so nothing else can come first).
//
// Like every edit, it returns an error and changes nothing when the file as
// read has Problems, an argument is malformed, or the edited file would
// have a Problem (an invalid portal or name, a portal conflict, more than
// 16 portals).
func (c *FabricsConf) SetISCSI(portals []string, baseName *string) error {
	return c.edit(func(e *fabEdit) error {
		if err := checkFabList("portal", portals); err != nil {
			return err
		}
		if baseName != nil {
			if err := checkFabValue("base name", *baseName); err != nil {
				return err
			}
		}
		r := c.find(fabISCSI, 0)
		if r != nil {
			if portals != nil {
				e.setMulti(r, "portal", portals)
			}
			if baseName != nil {
				e.setSingle(r, "base name", *baseName)
			}
			return nil
		}
		texts := []string{"iSCSI:"}
		for _, p := range portals {
			texts = append(texts, fieldText("portal", p))
		}
		if baseName != nil && *baseName != "" {
			texts = append(texts, fieldText("base name", *baseName))
		}
		if v := c.find(fabVersion, 0); v != nil {
			at := v.end()
			texts = append([]string{""}, texts...)
			if at < len(c.lines) && !isBlank(c.lines[at].Text) {
				texts = append(texts, "")
			}
			e.insert(at, texts...)
		} else {
			e.appendRecord(texts)
		}
		return nil
	})
}

// UpsertTarget edits the "iSCSI target:" record of a device.conf ID, or
// appends a new one. Field lines it does not change stay as they are, with
// their comments. Clearing CHAP also clears mutual CHAP, which upstream
// does not accept alone. A new target needs an iSCSI record (SetISCSI) to
// be valid. Errors as for SetISCSI; a CHAP change and a clear of the same
// thing in one edit is an error.
func (c *FabricsConf) UpsertTarget(id int, te TargetEdit) error {
	return c.edit(func(e *fabEdit) error {
		switch {
		case te.CHAP != nil && te.ClearCHAP:
			return fmt.Errorf("iSCSI target %d: CHAP both set and cleared", id)
		case te.MutualCHAP != nil && (te.ClearMutualCHAP || te.ClearCHAP):
			return fmt.Errorf("iSCSI target %d: mutual CHAP both set and cleared", id)
		}
		if err := checkFabList("initiator name", te.Initiators); err != nil {
			return err
		}
		if te.TargetName != nil {
			if err := checkFabValue("target name", *te.TargetName); err != nil {
				return err
			}
		}
		if err := checkCreds("chap", te.CHAP); err != nil {
			return err
		}
		if err := checkCreds("mutual chap", te.MutualCHAP); err != nil {
			return err
		}
		r := c.find(fabTarget, int64(id))
		if r == nil {
			texts := []string{fmt.Sprintf("iSCSI target: %d", id)}
			for _, i := range te.Initiators {
				texts = append(texts, fieldText("initiator name", i))
			}
			if te.TargetName != nil && *te.TargetName != "" {
				texts = append(texts, fieldText("target name", *te.TargetName))
			}
			if te.CHAP != nil {
				texts = append(texts, fieldText("chap name", te.CHAP.Name), fieldText("chap secret", te.CHAP.Secret))
			}
			if te.MutualCHAP != nil {
				texts = append(texts, fieldText("mutual chap name", te.MutualCHAP.Name),
					fieldText("mutual chap secret", te.MutualCHAP.Secret))
			}
			e.appendRecord(texts)
			return nil
		}
		if te.Initiators != nil {
			e.setMulti(r, "initiator name", te.Initiators)
		}
		if te.TargetName != nil {
			e.setSingle(r, "target name", *te.TargetName)
		}
		switch {
		case te.ClearCHAP:
			for _, f := range chapFields {
				e.setSingle(r, f, "")
			}
		case te.CHAP != nil:
			e.setSingle(r, "chap name", te.CHAP.Name)
			e.setSingle(r, "chap secret", te.CHAP.Secret)
		}
		switch {
		case te.ClearMutualCHAP:
			e.setSingle(r, "mutual chap name", "")
			e.setSingle(r, "mutual chap secret", "")
		case te.MutualCHAP != nil:
			e.setSingle(r, "mutual chap name", te.MutualCHAP.Name)
			e.setSingle(r, "mutual chap secret", te.MutualCHAP.Secret)
		}
		return nil
	})
}

// RemoveTarget removes an "iSCSI target:" record: its lines up to the last
// field, the comments after that up to a blank line (such as commented-out
// CHAP lines) unless the next record follows with no blank line, and one
// blank line next to it. Unlike RemoveStanza it keeps the comments above
// the record: in mhVTL's sample fabrics.conf the block above the first
// target documents the record format and its SECURITY warning
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/etc/fabrics.conf.sample),
// and unexporting a library must not remove that. A missing target is
// ErrNotFound.
func (c *FabricsConf) RemoveTarget(id int) error {
	return c.edit(func(e *fabEdit) error {
		r := c.find(fabTarget, int64(id))
		if r == nil {
			return fmt.Errorf("iSCSI target %d: %w", id, ErrNotFound)
		}
		n := len(c.lines)
		from, to := r.header, r.end()
		j := to
		for j < n && isFabComment(c.lines[j].Text) {
			j++
		}
		if j == n || isBlank(c.lines[j].Text) {
			to = j
		}
		if to == n || isBlank(c.lines[to].Text) {
			switch {
			case from > 0 && isBlank(c.lines[from-1].Text):
				from--
			case from == 0 && to < n:
				to++
			}
		}
		for k := from; k < to; k++ {
			e.del[k] = true
		}
		return nil
	})
}

// ---- plan ----

type fabDev struct {
	id, libID, slot int64
	isLib           bool
}

// fabTopology is fab_topology_parse
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L466-L502),
// which reads device.conf with its own sscanf calls, not conf_value.
func fabTopology(dev *DeviceConf) ([]fabDev, string) {
	var devs []fabDev
	cur := -1
	for i, l := range dev.Lines() {
		b := cText(l)
		n, v := cScan(b, "Library: %ld")
		if n != 1 {
			n, v = cScan(b, "Drive: %ld")
		}
		switch {
		case n == 1:
			if len(devs) == fabMaxDevs {
				return nil, fmt.Sprintf("device.conf line %d: device.conf has more than %d devices", i+1, fabMaxDevs)
			}
			devs = append(devs, fabDev{id: v[0], isLib: b[0] == 'L', libID: -1})
			cur = len(devs) - 1
		case b == "" || (b[0] != ' ' && b[0] != '\t'):
			cur = -1
		case cur >= 0 && !devs[cur].isLib:
			if n, v := cScan(b, " Library ID: %ld Slot: %d"); n == 2 {
				devs[cur].libID, devs[cur].slot = v[0], v[1]
			}
		}
	}
	return devs, ""
}

// CheckPlan runs fab_plan's checks
// (https://github.com/markh794/mhvtl/blob/19b71bd98d8ac2371db1e23a4af8dc17e3b089f3/usr/fabrics_conf.c#L537-L611)
// against device.conf: each target names a Library or standalone Drive,
// has a Target name or a Base name to derive one from (no default: the
// naming authority is the administrator's), derived names are valid and
// unique, and the library's drives have distinct Slots, at most 64 LUNs.
// mhvtl-fabrics runs these at apply time, not when it parses, so they are
// not in Problems. Call it on a file without Problems. Problems are on this
// file's target header lines; a device.conf limit has Line 0. Upstream
// stops at the first; this goes on with the next target.
func (c *FabricsConf) CheckPlan(dev *DeviceConf) []Problem {
	devs, msg := fabTopology(dev)
	if msg != "" {
		return []Problem{{Msg: msg}}
	}
	_, base, _ := c.ISCSI()
	var ps []Problem
	var names []string
	for _, t := range c.records {
		if t.kind != fabTarget {
			continue
		}
		problem := func(format string, a ...any) {
			ps = append(ps, Problem{Line: t.header + 1, Msg: fmt.Sprintf(format, a...)})
		}
		var d *fabDev
		for k := range devs {
			if devs[k].id == t.id {
				d = &devs[k]
				break
			}
		}
		if d == nil {
			problem("iSCSI target %d: no Library or Drive %d in device.conf", t.id, t.id)
			continue
		}
		if !d.isLib && d.libID >= 0 {
			problem("iSCSI target %d: drive belongs to library %d; export the library", t.id, d.libID)
			continue
		}
		wwn := t.wwn
		if wwn == "" && base == "" {
			problem("iSCSI target %d has no Target name and the iSCSI record no Base name", t.id)
			continue
		}
		if wwn == "" {
			kind := "tape"
			if d.isLib {
				kind = "lib"
			}
			wwn = fmt.Sprintf("%s.%s%d", base, kind, t.id)
			if len(wwn) >= fabNameMax || !ValidISCSIName(wwn) {
				problem("iSCSI target %d: derived name is too long or invalid", t.id)
				continue
			}
		}
		dup := false
		for _, n := range names {
			dup = dup || n == wwn
		}
		names = append(names, wwn)
		if dup {
			problem("target name %s is used twice", wwn)
			continue
		}
		if !d.isLib {
			continue // one LUN, 0
		}
		// add_lun (L514-L535): LUN 0 is the changer, each drive its Slot.
		luns := map[int64]bool{0: true}
		for _, drv := range devs {
			if drv.isLib || drv.libID != d.id {
				continue
			}
			if drv.slot < 1 {
				problem("drive %d in library %d has no Slot", drv.id, d.id)
				break
			}
			if luns[drv.slot] {
				problem("target %d: two devices on LUN %d (check the drive Slot values in device.conf)", t.id, drv.slot)
				break
			}
			if len(luns) == fabMaxLUNs {
				problem("target %d: more than %d LUNs", t.id, fabMaxLUNs)
				break
			}
			luns[drv.slot] = true
		}
	}
	return ps
}

// ---- redaction ----

// SecretMask replaces a CHAP secret wherever one would be shown.
const SecretMask = "********"

const fabMask = SecretMask

// fabSecretField matches the start of a line naming a CHAP or mutual CHAP
// secret, commented out or not, in any case and spacing.
var fabSecretField = regexp.MustCompile(`^[ \t#]*(?i:(?:mutual[ \t]*)?chap[ \t]*secret)`)

// RedactFabrics returns data with the value of every CHAP secret and
// Mutual CHAP secret line replaced by "********", every other byte as it
// was. It works on any text, parseable or not, for audit diffs and
// previews. A line counts as a secret line when it starts (after blanks
// and '#') with "CHAP secret" or "Mutual CHAP secret" in any case, so a
// commented-out secret and a mistyped one ("CHAP secret abc", no colon) are
// masked too. Everything after the name and its ':' is masked, a trailing
// comment included: in a file being fixed the "comment" may be the tail of
// a secret.
func RedactFabrics(data []byte) []byte {
	out, _ := redactFabrics(data)
	return out
}

// FabricsDiff is the change from before to after as the fabrics.conf lines
// removed and added between their common head and tail, under a header
// naming the file, with every CHAP secret masked in the lines it shows. It
// diffs the real lines, so a changed secret still shows, as a masked line
// removed and added. The helper's audit and the API's previews use it.
func FabricsDiff(before, after []byte) string {
	al, bl := strings.SplitAfter(string(before), "\n"), strings.SplitAfter(string(after), "\n")
	i := 0
	for i < len(al) && i < len(bl) && al[i] == bl[i] {
		i++
	}
	j := 0
	for j < len(al)-i && j < len(bl)-i && al[len(al)-1-j] == bl[len(bl)-1-j] {
		j++
	}
	var d strings.Builder
	fmt.Fprintf(&d, "--- fabrics.conf\n+++ fabrics.conf\n@@ line %d @@\n", i+1)
	for _, side := range []struct {
		sign  string
		lines []string
	}{{"-", al[i : len(al)-j]}, {"+", bl[i : len(bl)-j]}} {
		for _, l := range side.lines {
			if l != "" {
				d.WriteString(side.sign + string(RedactFabrics([]byte(strings.TrimSuffix(l, "\n")))) + "\n")
			}
		}
	}
	return d.String()
}

// FabricsSecrets returns the values RedactFabrics masks, each also cut at
// its first blank (a secret has none), for masking text that may quote
// them, such as a command's error output.
func FabricsSecrets(data []byte) []string {
	_, secrets := redactFabrics(data)
	return secrets
}

func redactFabrics(data []byte) ([]byte, []string) {
	var secrets []string
	lines := splitLines(data)
	for i, l := range lines {
		m := fabSecretField.FindStringIndex(l.Text)
		if m == nil {
			continue
		}
		t := l.Text
		v := m[1]
		for v < len(t) && (t[v] == ' ' || t[v] == '\t') {
			v++
		}
		if v < len(t) && t[v] == ':' {
			v++
		}
		for v < len(t) && isCSpace(rune(t[v])) {
			v++
		}
		if v < len(t) {
			secrets = append(secrets, t[v:])
			if f := strings.Fields(t[v:]); len(f) > 0 && f[0] != t[v:] {
				secrets = append(secrets, f[0])
			}
			lines[i].Text = t[:v] + fabMask
		}
	}
	return renderLines(lines), secrets
}
