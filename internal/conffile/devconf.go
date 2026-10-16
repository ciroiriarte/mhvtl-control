// Package conffile is a lossless model of mhVTL's configuration files
// (device.conf, library_contents.<id> and mhvtl.conf). Every line keeps its
// exact bytes, so rendering an unedited file reproduces it byte for byte, and
// an edit rewrites only the lines it touches.
//
// Citations of mhVTL source lines refer to upstream master 6458223
// (github.com/markh794/mhvtl).
//
// Known gaps, for the editor's validation and apply phases (4.3, 5):
//   - The media home is read by find_media_home_directory (mhvtl
//     usr/vtllib.c:1673-1737), not like other attributes: its "found" flag
//     is never reset, so the first " Home directory:" line after the
//     library's header wins even when it is in a later stanza, and sscanf
//     "%s" (:1720) cuts the path at its first blank. A home path with a
//     space is not flagged here.
//   - MoveAcross falls back to copying only on EXDEV, not when link(2)
//     fails with EPERM (a filesystem without hard links, or
//     protected_hardlinks for a non-owner).
package conffile

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Line is one line of a file: its text without the line ending, and the
// ending itself ("\n", "\r\n", or "" for a last line with no newline).
type Line struct {
	Text string
	EOL  string
}

func splitLines(data []byte) []Line {
	var lines []Line
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			lines = append(lines, Line{Text: string(data)})
			break
		}
		l := Line{Text: string(data[:i]), EOL: "\n"}
		if strings.HasSuffix(l.Text, "\r") {
			l.Text, l.EOL = l.Text[:len(l.Text)-1], "\r\n"
		}
		lines = append(lines, l)
		data = data[i+1:]
	}
	return lines
}

func renderLines(lines []Line) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteString(l.EOL)
	}
	return b.Bytes()
}

// eol is the line ending new lines get: that of the last terminated line,
// else "\n".
func eol(lines []Line) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].EOL != "" {
			return lines[i].EOL
		}
	}
	return "\n"
}

// insertLines inserts add before index at, keeping a missing final newline
// missing: when appending after an unterminated last line, that line gets an
// ending and the new last line goes without one.
func insertLines(lines []Line, at int, add []Line) []Line {
	if len(add) == 0 {
		return lines
	}
	if at == len(lines) && at > 0 && lines[at-1].EOL == "" {
		lines[at-1].EOL = eol(lines)
		add[len(add)-1].EOL = ""
	}
	out := make([]Line, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

// deleteLines removes lines [from, to). If that drops an unterminated last
// line, the new last line loses its ending so the file still has none.
func deleteLines(lines []Line, from, to int) []Line {
	noFinal := to == len(lines) && lines[to-1].EOL == ""
	out := append(lines[:from:from], lines[to:]...)
	if noFinal && len(out) > 0 {
		out[len(out)-1].EOL = ""
	}
	return out
}

// isBlank reports a line with nothing but white space. Whether it separates
// records depends on its length (see separates).
func isBlank(text string) bool { return strings.TrimSpace(text) == "" }

// cLen is strlen(b) for the line as mhVTL's readline (mhvtl
// usr/vtllib.c:1064) leaves it: fgets, then every '\n' past the first byte
// cut. A '\r' of a CRLF ending stays, and an empty LF line keeps its '\n'.
func cLen(l Line) int {
	n := len(l.Text)
	if l.EOL == "\r\n" {
		n++
	}
	if n == 0 && l.EOL == "\n" {
		n = 1
	}
	return n
}

// separates reports a blank line every mhVTL parser takes as the end of a
// record: white space with cLen at most one (see ambiguousBlank). Longer
// white-space lines are kept inside the record, as vtllibrary and vtltape
// read them.
func separates(l Line) bool { return isBlank(l.Text) && cLen(l) <= 1 }

// maxLine is the longest line readline returns whole: fgets into a
// MALLOC_SZ (512, mhvtl include/vtllib.h:94) buffer reads at most 511 bytes,
// the '\n' included, so a longer line is read as two.
const maxLine = 510

// lineProblem reports a line mhVTL's readline does not read as one C string:
// too long for its buffer, or holding a NUL, which ends the string early.
// Both device.conf and library_contents are read that way.
func lineProblem(l Line) string {
	switch {
	case strings.IndexByte(l.Text, 0) >= 0:
		return "line contains a NUL byte: mhVTL reads it only up to there"
	case len(l.Text)+len(l.EOL) > maxLine+1:
		return fmt.Sprintf("line longer than %d bytes: mhVTL's readline splits it in two", maxLine)
	}
	return ""
}

// ambiguousBlank reports a line that is white space but not empty (line
// ending aside). mhVTL's parsers disagree on whether it ends a record. They
// read lines with readline (mhvtl usr/vtllib.c:1064), which drops a '\n'
// past the first byte, then test:
//   - strlen(b) == 1 in mhvtl usr/cmd/vtllibrary.c:740 and :1281, and
//     mhvtl usr/cmd/vtltape.c:2137;
//   - strlen(b) < 3 in mhvtl usr/vtllib.c:1710 (find_media_home_directory).
//
// An empty line, LF or CRLF ("\r" alone, strlen 1), separates records for
// all of them, and so does a single white-space character. Two characters,
// such as "  " or " \r", separate only for the home-directory lookup, and
// three or more for none. Inside a Drive record, a line vtltape does not end
// the record at (strlen 2 or more) also reaches its sscanf " Compression:
// factor %d enabled %d" (mhvtl usr/cmd/vtltape.c:2204), which returns EOF on
// it; vtltape takes that as a match and loads uninitialised compression
// settings. Every non-empty white-space line is flagged, not just the ones
// they disagree on, so no edit has to reason about them.
// The model ends a record at the lines all of them agree on (separates).
// An indented line of cLen 2 (" #", "\ta") is split on the same way, and
// flagged too. (A column-1 one is a comment, which every parser skips first,
// or a malformed header.)
func ambiguousBlank(l Line) bool { return l.Text != "" && isBlank(l.Text) }

// isComment reports a comment line. mhVTL ignores '#' in column 1 only
// (mhvtl usr/cmd/vtllibrary.c:677); an indented '#' line is kept as an
// opaque stanza line.
func isComment(text string) bool { return strings.HasPrefix(text, "#") }

// ErrNotFound is returned by an edit naming a stanza or entry that does not
// exist.
var ErrNotFound = errors.New("not found")

// Problem is a line mhVTL would not read the way its shape suggests. Parsing
// never fails, so any file round-trips; callers that need a well-formed file
// check Problems, and a writer must refuse to write over a file with any.
type Problem struct {
	Line int // 1-based
	Msg  string
	// Ambiguous marks a line the model still reads, but which does not mean
	// to mhVTL what its shape says: mhVTL's parsers read it differently from
	// the model or from each other (a white-space line inside a record, a
	// duplicate record, a barcode vtllibrary exits on). The others are lines
	// the model cannot read at all. Readers may go on past an ambiguous
	// problem; writes may not.
	Ambiguous bool
}

func (p Problem) Error() string { return fmt.Sprintf("line %d: %s", p.Line, p.Msg) }

// Attr is one " Key: value" line of a device.conf stanza, read as mhVTL's
// conf_value (mhvtl usr/vtllib.c:1090-1144) reads it: Key with each run of
// blanks collapsed to one space, Value without a trailing comment (a '#'
// after a blank) and trimmed. mhVTL reads a few keys with sscanf on the
// whole line instead (Compression, NAA, and vtllibrary's Library ID); a
// value it reads differently from that, or not at all, is an ambiguous
// Problem (see scanfProblem).
type Attr struct {
	Key, Value string
	Line       int // index into the file's lines
}

// Stanza is one "Library:" or "Drive:" record of device.conf.
type Stanza struct {
	Kind string // "Library" or "Drive"
	ID   int
	// HasAddress is set when the header carries CHANNEL, TARGET and LUN.
	// Without them the header is an ambiguous Problem: sscanf returns 1 or
	// 2, which vtltape and vtllibrary take as a match, and they copy an
	// uninitialised mhvtl_ctl (mhvtl usr/cmd/vtltape.c:2078, :2146, used by
	// add_lu at :2684; usr/cmd/vtllibrary.c:1224, :1290). Only the
	// changer's element descriptor gets a defined SCSI ID for a bare
	// "Drive: N", 0 (vtllibrary.c:682-684).
	HasAddress           bool
	Channel, Target, LUN int
	HeaderLine           int // index of the header line
	// Start is the first line of the stanza's leading comment block (equal
	// to HeaderLine when there is none); End is one past its last line,
	// which is the line before the next blank line or the next stanza.
	Start, End int
	Attrs      []Attr // in file order, duplicates included
	// Attr maps a key to the line of its last non-empty occurrence (else
	// its last one): conf_value ignores a field with no value, so the last
	// non-empty one is what mhVTL keeps.
	Attr map[string]int
}

// DeviceConf is a lossless device.conf.
type DeviceConf struct {
	lines       []Line
	stanzas     []Stanza
	versionLine int // -1 when there is no VERSION line
	Problems    []Problem
}

// ParseDeviceConf parses device.conf. It never fails: lines it cannot
// interpret are kept verbatim and reported in Problems.
func ParseDeviceConf(data []byte) *DeviceConf {
	c := &DeviceConf{lines: splitLines(data)}
	c.index()
	return c
}

// index rebuilds the stanza view from the lines. Edits change the lines and
// call it again, so line indices are always current.
func (c *DeviceConf) index() {
	c.stanzas, c.Problems, c.versionLine = nil, nil, -1
	cur := -1 // index into c.stanzas of the open stanza
	closeAt := func(i int) {
		if cur >= 0 {
			c.stanzas[cur].End = i
			cur = -1
		}
	}
	problem := func(i int, ambiguous bool, format string, a ...any) {
		c.Problems = append(c.Problems, Problem{Line: i + 1, Ambiguous: ambiguous, Msg: fmt.Sprintf(format, a...)})
	}
	for i, l := range c.lines {
		if msg := lineProblem(l); msg != "" {
			problem(i, true, "%s", msg)
		}
		if cur >= 0 && !isComment(l.Text) {
			switch {
			case ambiguousBlank(l) && c.stanzas[cur].Kind == "Drive" && cLen(l) > 1:
				problem(i, true, "white-space line inside a record: mhVTL's parsers disagree on whether it ends the record, and vtltape's Compression sscanf (vtltape.c:2204) takes it as a match and loads uninitialised compression settings; make it empty")
			case ambiguousBlank(l):
				problem(i, true, "white-space line inside a record: mhVTL's parsers disagree on whether it ends the record; make it empty")
			case cLen(l) == 2 && (l.Text[0] == ' ' || l.Text[0] == '\t'):
				problem(i, true, "two-byte line inside a record: strlen 2 is blank to strlen(b) < 3 (vtllib.c:1710) but not to strlen(b) == 1 (vtllibrary.c:740, :1281, vtltape.c:2137)")
			}
		}
		text := strings.TrimRight(l.Text, " \t\r")
		switch {
		case isBlank(text):
			if separates(l) {
				closeAt(i)
			}
		case isComment(text):
		case text[0] != ' ' && text[0] != '\t':
			if strings.HasPrefix(text, "VERSION:") {
				if c.versionLine < 0 {
					c.versionLine = i
				}
				continue
			}
			st, trailing, ok := parseStanzaHeader(text)
			if !ok {
				problem(i, false, "malformed record header %q", text)
				continue
			}
			switch {
			case trailing:
				problem(i, true, "text after LUN in %q: mhVTL ignores it, but this header is not read as an address", text)
			case !st.HasAddress && st.Kind == "Drive":
				problem(i, true, "Drive %d has no CHANNEL, TARGET and LUN: vtltape registers it at an uninitialised address (vtltape.c:2078, :2146)", st.ID)
			case !st.HasAddress:
				problem(i, true, "Library %d has no CHANNEL, TARGET and LUN: vtllibrary registers it at an uninitialised address (vtllibrary.c:1224, :1290)", st.ID)
			}
			// mhVTL's parsers follow one kind of record each and only a
			// blank line ends it (vtllibrary.c:677-740 and :1281-1293,
			// vtltape.c:2137-2148): a header of the other kind leaves the
			// open record running, so its attributes apply to both.
			if cur >= 0 && c.stanzas[cur].Kind != st.Kind {
				problem(i, true, "%s header with no blank line after %s %d: mhVTL reads what follows as part of %s %d too",
					st.Kind, c.stanzas[cur].Kind, c.stanzas[cur].ID, c.stanzas[cur].Kind, c.stanzas[cur].ID)
			}
			// vtllibrary moves to a new drive only at its " Library ID:"
			// line (vtllibrary.c:686-712), so with no blank line between,
			// the attributes before it still go to the previous drive.
			if cur >= 0 && st.Kind == "Drive" && c.stanzas[cur].Kind == "Drive" {
				problem(i, true, "Drive header with no blank line after Drive %d: vtllibrary applies what comes before its Library ID to Drive %d",
					c.stanzas[cur].ID, c.stanzas[cur].ID)
			}
			// Column-1 comments right above the header belong to this
			// stanza, not the one before it.
			start := i
			for start > 0 && isComment(c.lines[start-1].Text) {
				start--
			}
			closeAt(start)
			st.HeaderLine, st.Start, st.Attr = i, start, map[string]int{}
			c.stanzas = append(c.stanzas, st)
			cur = len(c.stanzas) - 1
		default:
			trimmed := strings.TrimSpace(text)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if cur < 0 {
				problem(i, false, "attribute outside of a record")
				continue
			}
			rawKey, val, ok := strings.Cut(trimmed, ":")
			if !ok {
				problem(i, false, "expected 'Key: value'")
				continue
			}
			if strings.TrimRight(rawKey, " \t") != rawKey {
				// conf_value needs the ':' right after the name.
				problem(i, true, "blank before ':' in %q: mhVTL ignores this line", trimmed)
				continue
			}
			key := strings.Join(strings.Fields(rawKey), " ")
			value := attrValue(val)
			st := &c.stanzas[cur]
			if msg := scanfProblem(st.Kind, key, cText(l)); msg != "" {
				problem(i, true, "%s", msg)
			}
			st.Attrs = append(st.Attrs, Attr{Key: key, Value: value, Line: i})
			if j, seen := st.Attr[key]; !seen || value != "" || c.attrValue(j) == "" {
				st.Attr[key] = i
			}
		}
	}
	closeAt(len(c.lines))

	type kindID struct {
		kind string
		id   int
	}
	first := map[kindID]int{}
	for _, st := range c.stanzas {
		k := kindID{st.Kind, st.ID}
		if j, dup := first[k]; dup {
			problem(st.HeaderLine, true, "duplicate %s %d (first at line %d): mhVTL applies both", st.Kind, st.ID, j+1)
			continue
		}
		first[k] = st.HeaderLine
	}
	sort.SliceStable(c.Problems, func(a, b int) bool { return c.Problems[a].Line < c.Problems[b].Line })
}

// attrValue reads a value as conf_value does: leading blanks skipped, cut at
// a '#' that follows a blank, trailing blanks and '\r' trimmed.
func attrValue(v string) string {
	v = strings.TrimLeft(v, " \t")
	for i := 0; i+1 < len(v); i++ {
		if (v[i] == ' ' || v[i] == '\t') && v[i+1] == '#' {
			v = v[:i]
			break
		}
	}
	return strings.TrimRight(v, " \t\r\n")
}

// attrValue returns the value of the attribute on line i.
func (c *DeviceConf) attrValue(i int) string {
	_, v, _ := strings.Cut(c.lines[i].Text, ":")
	return attrValue(v)
}

// parseStanzaHeader parses "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00" or a
// bare "Drive: 11", following the sscanf patterns in
// mhvtl usr/cmd/vtllibrary.c:679-686.
// trailing reports text after a full address, which mhVTL's sscanf ignores
// but leaves HasAddress false here.
func parseStanzaHeader(text string) (st Stanza, trailing, ok bool) {
	f := strings.Fields(strings.ReplaceAll(text, ":", " "))
	if len(f) < 2 || (f[0] != "Library" && f[0] != "Drive") ||
		!strings.HasPrefix(text, f[0]+":") {
		return Stanza{}, false, false
	}
	id, err := strconv.Atoi(f[1])
	if err != nil {
		return Stanza{}, false, false
	}
	st = Stanza{Kind: f[0], ID: id}
	if len(f) > 8 && f[2] == "CHANNEL" && f[4] == "TARGET" && f[6] == "LUN" {
		trailing = true
	}
	if len(f) == 8 && f[2] == "CHANNEL" && f[4] == "TARGET" && f[6] == "LUN" {
		var nums [3]int
		ok := true
		for j, s := range []string{f[3], f[5], f[7]} {
			n, err := strconv.Atoi(s)
			if err != nil {
				ok = false
				break
			}
			nums[j] = n
		}
		if ok {
			st.HasAddress = true
			st.Channel, st.Target, st.LUN = nums[0], nums[1], nums[2]
		}
	}
	return st, trailing, true
}

// Lines returns the file's lines. The slice must not be modified.
func (c *DeviceConf) Lines() []Line { return c.lines }

// Version returns the value of the VERSION line, if any.
func (c *DeviceConf) Version() (string, bool) {
	if c.versionLine < 0 {
		return "", false
	}
	_, v, _ := strings.Cut(c.lines[c.versionLine].Text, ":")
	return strings.TrimSpace(v), true
}

// Stanzas returns the stanzas in file order. The result must not be modified;
// it is invalidated by the next edit.
func (c *DeviceConf) Stanzas() []Stanza { return c.stanzas }

// Stanza returns the stanza of the given kind and id.
func (c *DeviceConf) Stanza(kind string, id int) (Stanza, bool) {
	for _, st := range c.stanzas {
		if st.Kind == kind && st.ID == id {
			return st, true
		}
	}
	return Stanza{}, false
}

// Get returns the effective value of key in a stanza: its last occurrence, as
// mhVTL reads the lines in order and the last one wins.
func (st Stanza) Get(key string) (string, bool) {
	i, ok := st.Attr[key]
	if !ok {
		return "", false
	}
	for _, a := range st.Attrs {
		if a.Line == i {
			return a.Value, true
		}
	}
	return "", false
}

// Render returns the file's bytes.
func (c *DeviceConf) Render() []byte { return renderLines(c.lines) }

// checkKV refuses what conf_value would not read back as written: a key
// that is not in canonical form, a value with a line break or NUL, leading
// or trailing blanks (trimmed), or a '#' after a blank (a comment), and a
// line too long for readline.
func checkKV(key, value string) error {
	if key == "" || strings.ContainsAny(key, ":\r\n#\x00") || strings.Join(strings.Fields(key), " ") != key {
		return fmt.Errorf("invalid key %q", key)
	}
	switch {
	case strings.ContainsAny(value, "\r\n\x00"):
		return fmt.Errorf("invalid value for %s: contains a line break or NUL", key)
	case strings.Trim(value, " \t") != value:
		return fmt.Errorf("invalid value for %s: leading or trailing blanks", key)
	case strings.Contains(value, " #") || strings.Contains(value, "\t#"):
		return fmt.Errorf("invalid value for %s: a '#' after a blank starts a comment", key)
	case len(attrText(key, value))+2 > maxLine+1:
		return fmt.Errorf("invalid value for %s: line longer than %d bytes", key, maxLine)
	}
	return nil
}

// checkScanf refuses a value of a field mhVTL reads with sscanf that it would
// not read as written (see scanfProblem), such as an NAA short of 8 bytes.
func checkScanf(kind, key, value string) error {
	if msg := scanfProblem(kind, key, attrText(key, value)); msg != "" {
		return fmt.Errorf("invalid value for %s: %s", key, msg)
	}
	return nil
}

func (c *DeviceConf) find(kind string, id int) (Stanza, error) {
	st, ok := c.Stanza(kind, id)
	if !ok {
		return Stanza{}, fmt.Errorf("%s %d: %w", kind, id, ErrNotFound)
	}
	return st, nil
}

// SetAttr sets key in a stanza. An existing line keeps its indent, key and
// the spacing after the colon, and only its value changes; with duplicates,
// the last (effective) one is changed. A new key is added as " Key: value"
// after the stanza's last attribute.
func (c *DeviceConf) SetAttr(kind string, id int, key, value string) error {
	if err := checkKV(key, value); err != nil {
		return err
	}
	st, err := c.find(kind, id)
	if err != nil {
		return err
	}
	if err := checkScanf(kind, key, value); err != nil {
		return err
	}
	if i, ok := st.Attr[key]; ok {
		text := c.lines[i].Text
		colon := strings.IndexByte(text, ':') + 1
		sp := colon
		for sp < len(text) && (text[sp] == ' ' || text[sp] == '\t') {
			sp++
		}
		prefix := text[:sp]
		if sp == colon && value != "" {
			prefix += " "
		}
		// A trailing comment stays, after a blank. With no value it goes:
		// conf_value would take "# ..." as the value.
		comment := ""
		for j := sp; j+1 < len(text) && value != ""; j++ {
			if (text[j] == ' ' || text[j] == '\t') && text[j+1] == '#' {
				comment = text[j:]
				break
			}
		}
		if len(prefix)+len(value)+len(comment)+2 > maxLine+1 {
			return fmt.Errorf("invalid value for %s: line longer than %d bytes", key, maxLine)
		}
		c.lines[i].Text = prefix + value + comment
		c.index()
		return nil
	}
	at := st.HeaderLine + 1
	if n := len(st.Attrs); n > 0 {
		at = st.Attrs[n-1].Line + 1
	}
	c.lines = insertLines(c.lines, at, []Line{{Text: attrText(key, value), EOL: eol(c.lines)}})
	c.index()
	return nil
}

func attrText(key, value string) string {
	if value == "" {
		return " " + key + ":"
	}
	return " " + key + ": " + value
}

// DeleteAttr removes every line of key from a stanza. A missing key is not an
// error.
func (c *DeviceConf) DeleteAttr(kind string, id int, key string) error {
	st, err := c.find(kind, id)
	if err != nil {
		return err
	}
	for j := len(st.Attrs) - 1; j >= 0; j-- {
		if a := st.Attrs[j]; a.Key == key {
			c.lines = deleteLines(c.lines, a.Line, a.Line+1)
		}
	}
	c.index()
	return nil
}

// Header is the address of a new stanza.
type Header struct {
	Kind                 string // "Library" or "Drive"
	ID                   int
	Channel, Target, LUN int
}

// AddStanza adds a stanza in canonical form, as the shipped generator writes
// it (etc/generate_device_conf.in): the header, then " Key: value" lines. It
// goes after the last stanza of its kind, separated by one blank line. The
// first Library goes before every other stanza, and the first Drive after
// them all. Where a Drive lands doesn't matter to mhVTL: a drive belongs to
// the library its " Library ID:" names (mhvtl usr/cmd/vtllibrary.c:686), not
// to the Library stanza it follows. Blank lines it writes are empty, never
// white space (see ambiguousBlank).
func (c *DeviceConf) AddStanza(h Header, attrs []Attr) error {
	if h.Kind != "Library" && h.Kind != "Drive" {
		return fmt.Errorf("invalid stanza kind %q", h.Kind)
	}
	if _, ok := c.Stanza(h.Kind, h.ID); ok {
		return fmt.Errorf("%s %d already exists", h.Kind, h.ID)
	}
	for _, a := range attrs {
		if err := checkKV(a.Key, a.Value); err != nil {
			return err
		}
		if err := checkScanf(h.Kind, a.Key, a.Value); err != nil {
			return err
		}
	}
	nl := eol(c.lines)
	block := []Line{{Text: fmt.Sprintf("%s: %d CHANNEL: %02d TARGET: %02d LUN: %02d",
		h.Kind, h.ID, h.Channel, h.Target, h.LUN), EOL: nl}}
	for _, a := range attrs {
		block = append(block, Line{Text: attrText(a.Key, a.Value), EOL: nl})
	}
	last := -1
	for j, st := range c.stanzas {
		if st.Kind == h.Kind {
			last = j
		}
	}
	switch {
	case last >= 0:
		// Separator first, so the new stanza follows the old one's end, and
		// one after it unless a blank line is already there.
		at := c.stanzas[last].End
		if at < len(c.lines) && !separates(c.lines[at]) {
			block = append(block, Line{EOL: nl})
		}
		c.lines = insertLines(c.lines, at, append([]Line{{EOL: nl}}, block...))
	case h.Kind == "Library" && len(c.stanzas) > 0:
		c.lines = insertLines(c.lines, c.stanzas[0].Start, append(block, Line{EOL: nl}))
	default:
		at := len(c.lines)
		if len(c.stanzas) > 0 {
			at = c.stanzas[len(c.stanzas)-1].End
		}
		if at > 0 && !separates(c.lines[at-1]) {
			block = append([]Line{{EOL: nl}}, block...)
		}
		c.lines = insertLines(c.lines, at, block)
	}
	c.index()
	return nil
}

// RemoveStanza removes a stanza from Start to End: its leading comment
// block, and the column-1 comments that follow it up to the next blank line
// (such as the shipped "# fifo:" line, a commented-out attribute of that
// record). When the next header follows with no blank line, every comment
// between the two is the next header's leading block (see index), so it
// stays. One blank line next to the stanza goes too, so no double gap is
// left: the one before it, or at the top of the file the one after it. When
// the next record abuts the stanza, no blank goes, so the records around it
// never end up adjacent.
func (c *DeviceConf) RemoveStanza(kind string, id int) error {
	st, err := c.find(kind, id)
	if err != nil {
		return err
	}
	from, to := st.Start, st.End
	if to == len(c.lines) || separates(c.lines[to]) {
		switch {
		case from > 0 && separates(c.lines[from-1]):
			from--
		case from == 0 && to < len(c.lines):
			to++
		}
	}
	c.lines = deleteLines(c.lines, from, to)
	c.index()
	return nil
}
