// Package conffile is a lossless model of mhVTL's configuration files
// (device.conf, library_contents.<id> and mhvtl.conf). Every line keeps its
// exact bytes, so rendering an unedited file reproduces it byte for byte, and
// an edit rewrites only the lines it touches.
package conffile

import (
	"bytes"
	"errors"
	"fmt"
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

// eol is the line ending new lines get: that of the first terminated line,
// else "\n".
func eol(lines []Line) string {
	for _, l := range lines {
		if l.EOL != "" {
			return l.EOL
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

// separates reports a blank line every mhVTL parser takes as the end of a
// record: one whose bytes before the '\n' number at most one (see
// ambiguousBlank). Longer white-space lines are kept inside the record, as
// vtllibrary and vtltape read them.
func separates(l Line) bool {
	n := len(l.Text)
	if l.EOL == "\r\n" {
		n++
	}
	return n <= 1
}

// ambiguousBlank reports a line that is white space but not empty (line
// ending aside). mhVTL's parsers disagree on whether it ends a record. They
// read lines with readline (mhvtl usr/vtllib.c:1064), which drops a '\n'
// past the first byte, then test:
//   - strlen(b) == 1 in mhvtl usr/cmd/vtllibrary.c:740 and :1281, and
//     mhvtl usr/cmd/vtltape.c:2134;
//   - strlen(b) < 3 in mhvtl usr/vtllib.c:1710 (find_media_home_directory).
//
// An empty line, LF or CRLF ("\r" alone, strlen 1), separates records for
// all of them, and so does a single white-space character. Two characters,
// such as "  " or " \r", separate only for the home-directory lookup, and
// three or more for none. Every non-empty white-space line is flagged, not
// just the ones they disagree on, so no edit has to reason about them.
// The model ends a record at the lines all of them agree on (separates).
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
	// Ambiguous marks a line mhVTL's parsers read differently from each
	// other (a white-space line inside a stanza), as opposed to a malformed
	// one. Such a file still loads, so readers may go on; writes may not.
	Ambiguous bool
}

func (p Problem) Error() string { return fmt.Sprintf("line %d: %s", p.Line, p.Msg) }

// Attr is one " Key: value" line of a device.conf stanza.
type Attr struct {
	Key, Value string
	Line       int // index into the file's lines
}

// Stanza is one "Library:" or "Drive:" record of device.conf.
type Stanza struct {
	Kind string // "Library" or "Drive"
	ID   int
	// HasAddress is set when the header carries CHANNEL, TARGET and LUN.
	// mhVTL accepts a bare "Drive: N", with target 0
	// (mhvtl usr/cmd/vtllibrary.c:683).
	HasAddress           bool
	Channel, Target, LUN int
	HeaderLine           int // index of the header line
	// Start is the first line of the stanza's leading comment block (equal
	// to HeaderLine when there is none); End is one past its last line,
	// which is the line before the next blank line or the next stanza.
	Start, End int
	Attrs      []Attr         // in file order, duplicates included
	Attr       map[string]int // key -> line index of its last occurrence
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
	problem := func(i int, format string, a ...any) {
		c.Problems = append(c.Problems, Problem{Line: i + 1, Msg: fmt.Sprintf(format, a...)})
	}
	for i, l := range c.lines {
		text := strings.TrimRight(l.Text, " \t\r")
		switch {
		case isBlank(text):
			if cur >= 0 && ambiguousBlank(l) {
				c.Problems = append(c.Problems, Problem{Line: i + 1, Ambiguous: true,
					Msg: "white-space line inside a record: mhVTL's parsers disagree on whether it ends the record; make it empty"})
			}
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
			st, ok := parseStanzaHeader(text)
			if !ok {
				problem(i, "malformed record header %q", text)
				continue
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
				problem(i, "attribute outside of a record")
				continue
			}
			key, val, ok := strings.Cut(trimmed, ":")
			if !ok {
				problem(i, "expected 'Key: value'")
				continue
			}
			key = strings.TrimSpace(key)
			st := &c.stanzas[cur]
			st.Attrs = append(st.Attrs, Attr{Key: key, Value: strings.TrimSpace(val), Line: i})
			st.Attr[key] = i
		}
	}
	closeAt(len(c.lines))
}

// parseStanzaHeader parses "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00" or a
// bare "Drive: 11", following the sscanf patterns in
// mhvtl usr/cmd/vtllibrary.c:679-686.
func parseStanzaHeader(text string) (Stanza, bool) {
	f := strings.Fields(strings.ReplaceAll(text, ":", " "))
	if len(f) < 2 || (f[0] != "Library" && f[0] != "Drive") ||
		!strings.HasPrefix(text, f[0]+":") {
		return Stanza{}, false
	}
	id, err := strconv.Atoi(f[1])
	if err != nil {
		return Stanza{}, false
	}
	st := Stanza{Kind: f[0], ID: id}
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
	return st, true
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

func checkKV(key, value string) error {
	if key == "" || strings.ContainsAny(key, ":\r\n#") || strings.TrimSpace(key) != key {
		return fmt.Errorf("invalid key %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid value for %s: contains a line break", key)
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
		c.lines[i].Text = prefix + value
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
		// Separator first, so the new stanza follows the old one's end.
		c.lines = insertLines(c.lines, c.stanzas[last].End, append([]Line{{EOL: nl}}, block...))
	case h.Kind == "Library" && len(c.stanzas) > 0:
		c.lines = insertLines(c.lines, c.stanzas[0].Start, append(block, Line{EOL: nl}))
	default:
		at := len(c.lines)
		if len(c.stanzas) > 0 {
			at = c.stanzas[len(c.stanzas)-1].End
		}
		if at > 0 && !isBlank(c.lines[at-1].Text) {
			block = append([]Line{{EOL: nl}}, block...)
		}
		c.lines = insertLines(c.lines, at, block)
	}
	c.index()
	return nil
}

// RemoveStanza removes a stanza with its leading comment block, and one
// blank line next to it so no double gap is left behind.
func (c *DeviceConf) RemoveStanza(kind string, id int) error {
	st, err := c.find(kind, id)
	if err != nil {
		return err
	}
	from, to := st.Start, st.End
	switch {
	case from > 0 && isBlank(c.lines[from-1].Text):
		from--
	case to < len(c.lines) && isBlank(c.lines[to].Text):
		to++
	}
	c.lines = deleteLines(c.lines, from, to)
	c.index()
	return nil
}
