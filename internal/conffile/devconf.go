// Package conffile is a lossless model of mhVTL's configuration files
// (device.conf, library_contents.<id> and mhvtl.conf). Every line keeps its
// exact bytes, so rendering an unedited file reproduces it byte for byte, and
// an edit rewrites only the lines it touches.
//
// Citations of mhVTL source lines refer to upstream master 852b78a
// (github.com/markh794/mhvtl).
//
// Every writer must call Writable on the model it is about to write, and
// write nothing when it returns an error.
//
// Known gaps, for the editor's validation and apply phases (4.3, 5). Those
// marked [writer] must be closed before any writer is wired:
//   - [writer] The media home is read by find_media_home_directory (mhvtl
//     usr/vtllib.c:1673-1740), not like other attributes: its "found" flag
//     is never reset, so the first " Home directory:" line after the
//     library's header wins even when it is in a later stanza, and sscanf
//     "%s" (:1723) cuts the path at its first blank. A home path with a
//     blank, and a " Home directory:" line outside its library's stanza,
//     are not flagged here.
//   - [writer] mhVTL's other readers of device.conf are not modelled. The
//     systemd generator (usr/cmd/mhvtl-device-conf-generator.c:229-262)
//     needs "Library: " or "Drive: " in column 1 with exactly one space,
//     so it skips "Library:10" and "Library:\t10", which vtllibrary
//     reads; it parses the id with strtol base 0, so "Drive: 011" starts
//     vtltape@9 (that one is flagged). mhvtl.init's device_ids
//     (etc/mhvtl.init.in:57) matches awk '$1 == "Library:"' and
//     '$1 == "Drive:"', so it too skips "Library:10", and it takes an
//     indented " Drive: 12" line, which the model reads as an attribute.
//   - [writer] make_vtl_media reads both files with awk, not like
//     vtllibrary (usr/cmd/make_vtl_media.in:341, :364). '/Library:/ {print
//     $2}' is unanchored: a commented-out "# Library: 20 ..." line prints
//     "Library:", so the script looks for library_contents.Library: and
//     exits whatever the other files hold ("#Library: 20", with no space,
//     prints 20); it prints "CHANNEL:" for "Library:10 CHANNEL: ...". '/^Slot/ {print
//     $3}' makes no media for "Slot 1:ABC", which vtllibrary loads, and
//     makes "E01001L8\r" from a CRLF library_contents.
//   - [writer] mhvtl.conf is read by the shell (and systemd's
//     EnvironmentFile=), but only simple KEY=VALUE lines are modelled.
//     Expansions ("$HOME/x"), separators ("a;"), quotes inside a word
//     ("a'b'") and backslash continuations raise no Problem, and Set
//     replaces the whole word after '=', so on "CAPACITY=/a;rm" it also
//     drops the command the shell would run. Control flow is not modelled
//     either: an assignment inside "if false; then VERBOSE=1; fi" or a
//     heredoc is returned by Get and edited by Set, though the shell never
//     makes it.
//   - [writer] No write is gated or serialised. ReplaceIfHash takes any
//     model, and two writers can both pass its re-hash and both rename,
//     the later silently replacing the earlier. A single Commit that takes
//     an advisory lock, refuses unless Writable passes on both the base
//     and the edited model, then calls ReplaceIfHash, is still to come.
//     Writable reads the model's current Problems, which every edit
//     rebuilds, so a base that fails (a repeated "Unit serial number")
//     can pass after the edit removes the line: Commit must check the base
//     as read, before editing. The edit primitives and WriteAtomic,
//     ReplaceIfHash and MoveAcross are exported and ungated; only Commit
//     should be used once it exists.
//   - MoveAcross falls back to copying only on EXDEV, not when link(2)
//     fails with EPERM (a filesystem without hard links, or
//     protected_hardlinks for a non-owner).
package conffile

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
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
func isBlank(text string) bool { return cTrim(text) == "" }

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
//   - strlen(b) == 1 in mhvtl usr/cmd/vtllibrary.c:740 and :1282, and
//     mhvtl usr/cmd/vtltape.c:2141;
//   - strlen(b) < 3 in mhvtl usr/vtllib.c:1710 (find_media_home_directory).
//
// An empty line, LF or CRLF ("\r" alone, strlen 1), separates records for
// all of them, and so does a single white-space character. Two characters,
// such as "  " or " \r", separate only for the home-directory lookup, and
// three or more for none. Inside a Drive record, a line vtltape does not end
// the record at (strlen 2 or more) also reaches its sscanf " Compression:
// factor %d enabled %d" (mhvtl usr/cmd/vtltape.c:2219), which returns EOF on
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

// writable is the error Writable returns for a file with problems.
func writable(file string, ps []Problem) error {
	if len(ps) == 0 {
		return nil
	}
	return fmt.Errorf("%s has %d problem(s), first %w; fix them by hand before editing", file, len(ps), ps[0])
}

// Writable returns an error when the file has any Problem, ambiguous or
// not. Every writer must call it, on the file as read and on the edited
// model, and write nothing if it fails.
func (c *DeviceConf) Writable() error { return writable("device.conf", c.Problems) }

// Attr is one " Key: value" line of a device.conf stanza, read as mhVTL's
// conf_value (mhvtl usr/vtllib.c:1089-1144) reads it: Key with each run of
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
	// uninitialised mhvtl_ctl (mhvtl usr/cmd/vtltape.c:2081, :2155, used by
	// add_lu at :2709; usr/cmd/vtllibrary.c:1224, :1296). Only the
	// changer's element descriptor gets a defined SCSI ID for a bare
	// "Drive: N", 0 (vtllibrary.c:682-684). Without an address Channel,
	// Target and LUN stay 0, so internal/config projects such a header as
	// 0/0/0; the Problem is what marks it as unusable.
	HasAddress           bool
	Channel, Target, LUN int
	HeaderLine           int // index of the header line
	// Start is the first line of the stanza's leading comment block (equal
	// to HeaderLine when there is none); End is one past its last line,
	// which is the line before the next blank line or the next stanza.
	Start, End int
	Attrs      []Attr // in file order, duplicates included
	// Attr maps a key to the line of its last non-empty occurrence (else
	// its last one), the line SetAttr changes. For a string read by
	// conf_value, which ignores a field with no value, that is the value
	// mhVTL keeps; for other keys it may not be, so a repeated key is an
	// ambiguous Problem.
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
				problem(i, true, "white-space line inside a record: mhVTL's parsers disagree on whether it ends the record, and vtltape's Compression sscanf (vtltape.c:2219) takes it as a match and loads uninitialised compression settings; make it empty")
			case ambiguousBlank(l):
				problem(i, true, "white-space line inside a record: mhVTL's parsers disagree on whether it ends the record; make it empty")
			case cLen(l) == 2 && (l.Text[0] == ' ' || l.Text[0] == '\t'):
				problem(i, true, "two-byte line inside a record: strlen 2 is blank to strlen(b) < 3 (vtllib.c:1710) but not to strlen(b) == 1 (vtllibrary.c:740, :1282, vtltape.c:2141)")
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
			st, ok := parseStanzaHeader(text)
			if !ok {
				// conf_value does not need a leading blank (vtllib.c:1093-1094),
				// so mhVTL reads a column-1 attribute inside a record.
				if cur >= 0 && strings.Contains(text, ":") {
					problem(i, false, "attribute in column 1 %q: mhVTL reads it as part of %s %d, this model does not; indent it",
						text, c.stanzas[cur].Kind, c.stanzas[cur].ID)
				} else {
					problem(i, false, "malformed record header %q", text)
				}
				continue
			}
			// The systemd generator reads the id with strtol base 0
			// (mhvtl usr/cmd/mhvtl-device-conf-generator.c:238, :251), so it starts
			// "Drive: 011" as vtltape@9; vtltape reads 11.
			if id := strings.TrimLeft(text[len(st.Kind)+1:], " \t\v\f\r"); len(id) > 1 && id[0] == '0' && id[1] >= '0' && id[1] <= '9' {
				problem(i, true, "%s id with a leading 0: the systemd generator reads it as octal, mhVTL as decimal", st.Kind)
			}
			switch {
			case !st.HasAddress && st.Kind == "Drive":
				problem(i, true, "Drive %d has no CHANNEL, TARGET and LUN: vtltape registers it at an uninitialised address (vtltape.c:2081, :2155)", st.ID)
			case !st.HasAddress:
				problem(i, true, "Library %d has no CHANNEL, TARGET and LUN: vtllibrary registers it at an uninitialised address (vtllibrary.c:1224, :1296)", st.ID)
			}
			// mhVTL's parsers follow one kind of record each and only a
			// blank line ends it (vtllibrary.c:677-740 and :1282-1305,
			// vtltape.c:2141-2163): a header of the other kind leaves the
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
			trimmed := strings.TrimLeft(text, " \t")
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
			key := canonicalKey(rawKey)
			value := attrValue(val)
			st := &c.stanzas[cur]
			if msg := scanfProblem(st.Kind, key, cText(l)); msg != "" {
				problem(i, true, "%s", msg)
			}
			// mhVTL does not settle a repeated key one way: a string read
			// by conf_value keeps its last non-empty value, PERSIST is
			// true once any line says so (vtllibrary.c:1335-1340), and an
			// invalid Compression type, Backoff or commandtimeout keeps the
			// earlier value (vtltape.c:2199-2214, vtllibrary.c:1344-1353).
			for _, a := range st.Attrs {
				if a.Key == key {
					problem(i, true, "%s repeated (first at line %d): mhVTL does not always keep the last one; keep one line", key, a.Line+1)
					break
				}
			}
			st.Attrs = append(st.Attrs, Attr{Key: key, Value: value, Line: i})
			if j, seen := st.Attr[key]; !seen || value != "" || c.attrValue(j) == "" {
				st.Attr[key] = i
			}
		}
	}
	closeAt(len(c.lines))

	// vtllibrary's update_drive_details applies a drive's attributes only
	// from its " Library ID:" line on (mhvtl usr/cmd/vtllibrary.c:686-738,
	// upstream master 852b78a), while vtltape reads the whole stanza. One
	// above it gives the drive itself one value and the changer another
	// (or none): for Unit serial number, the serial the changer reports for
	// the drive slot stays the library_contents Drive line's.
	for _, st := range c.stanzas {
		if st.Kind != "Drive" {
			continue
		}
		lib := -1
		for _, a := range st.Attrs {
			if a.Key == "Library ID" {
				lib = a.Line
				break
			}
		}
		for _, a := range st.Attrs {
			if lib >= 0 && a.Line < lib && changerReads[a.Key] {
				problem(a.Line, true, "%s above Library ID (line %d): vtllibrary reads a drive's attributes only from its Library ID line on, so the changer does not see it; move it below", a.Key, lib+1)
			}
		}
	}

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

// changerReads holds the drive attributes vtllibrary's update_drive_details
// reads (mhvtl usr/cmd/vtllibrary.c:716-738, upstream master 852b78a).
var changerReads = map[string]bool{
	"Unit serial number":     true,
	"Product identification": true,
	"Product revision level": true,
	"Vendor identification":  true,
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

// parseStanzaHeader parses "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00" or
// a Drive header with the sscanf pattern mhVTL uses (mhvtl
// usr/cmd/vtllibrary.c:1284, usr/cmd/vtltape.c:2143). ok is set when sscanf
// reads at least the id; HasAddress when it reads all four numbers. Text
// after LUN is ignored, as sscanf ignores it.
func parseStanzaHeader(text string) (st Stanza, ok bool) {
	for _, kind := range []string{"Library", "Drive"} {
		n, v := cScan(text, kind+": %d CHANNEL: %d TARGET: %d LUN: %d")
		if n < 1 {
			continue
		}
		st = Stanza{Kind: kind, ID: int(v[0])}
		if n == 4 {
			st.HasAddress = true
			st.Channel, st.Target, st.LUN = int(v[1]), int(v[2]), int(v[3])
		}
		return st, true
	}
	return Stanza{}, false
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

// Get returns the value of key on its Attr line. With a repeated key (an
// ambiguous Problem) mhVTL may use another one.
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
	if key == "" || strings.ContainsAny(key, ":\r\n#\x00") || canonicalKey(key) != key {
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
