package conffile

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrDriveBarcode is returned for an edit that would put a barcode on a
// Drive line. vtllibrary loads that cartridge into the drive at start
// (mhvtl usr/cmd/vtllibrary.c:1029-1039, init_drive_slot), so a merge must
// never write one.
var ErrDriveBarcode = errors.New("library_contents: a Drive line must not carry a barcode")

// Entry is one "Drive N:", "Picker N:", "MAP N: X" or "Slot N: X" line.
type Entry struct {
	Kind   string // "Drive", "Picker", "MAP" or "Slot"
	Number int
	// Barcode is empty when the element is empty. It is trimmed: the
	// .persist file vtllibrary writes at stop pads barcodes to 16 with
	// spaces ("%-16.16s", mhvtl usr/cmd/vtllibrary.c:345 and :1206), and
	// mhVTL's own sscanf "%s" drops the padding too.
	Barcode string
	Line    int // index into the file's lines
}

// LibContents is a lossless library_contents.<id>, or the
// library_contents.<id>.persist save_config writes (no VERSION line, padded
// barcodes, "Slot N: " with a trailing space when empty).
type LibContents struct {
	lines       []Line
	entries     []Entry
	versionLine int
	Problems    []Problem
}

// ParseLibContents parses library_contents.<id>. Like ParseDeviceConf it
// never fails; lines it cannot interpret are reported in Problems.
func ParseLibContents(data []byte) *LibContents {
	c := &LibContents{lines: splitLines(data)}
	c.index()
	return c
}

func (c *LibContents) index() {
	c.entries, c.Problems, c.versionLine = nil, nil, -1
	problem := func(i int, format string, a ...any) {
		c.Problems = append(c.Problems, Problem{Line: i + 1, Msg: fmt.Sprintf(format, a...)})
	}
	for i, l := range c.lines {
		// mhVTL skips only a column-1 '#' (mhvtl usr/cmd/vtllibrary.c:1025),
		// but an indented one fails every sscanf pattern there and is
		// ignored too.
		text := strings.TrimSpace(l.Text)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "VERSION:") {
			if c.versionLine < 0 {
				c.versionLine = i
			}
			continue
		}
		head, rest, ok := strings.Cut(text, ":")
		if !ok {
			problem(i, "missing ':' in %q", text)
			continue
		}
		f := strings.Fields(head)
		if len(f) != 2 {
			problem(i, "malformed %q", text)
			continue
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			problem(i, "bad number %q", f[1])
			continue
		}
		switch f[0] {
		case "Drive", "Picker", "MAP", "Slot":
			c.entries = append(c.entries, Entry{Kind: f[0], Number: n, Barcode: strings.TrimSpace(rest), Line: i})
		default:
			problem(i, "unknown entry %q", f[0])
		}
	}
}

// Lines returns the file's lines. The slice must not be modified.
func (c *LibContents) Lines() []Line { return c.lines }

// Version returns the value of the VERSION line, if any.
func (c *LibContents) Version() (string, bool) {
	if c.versionLine < 0 {
		return "", false
	}
	_, v, _ := strings.Cut(c.lines[c.versionLine].Text, ":")
	return strings.TrimSpace(v), true
}

// Entries returns the entries in file order. The result must not be
// modified; it is invalidated by the next edit.
func (c *LibContents) Entries() []Entry { return c.entries }

// Render returns the file's bytes.
func (c *LibContents) Render() []byte { return renderLines(c.lines) }

// lookup returns the index in c.entries of the last entry for kind and n,
// the one mhVTL applies last.
func (c *LibContents) lookup(kind string, n int) int {
	found := -1
	for j, e := range c.entries {
		if e.Kind == kind && e.Number == n {
			found = j
		}
	}
	return found
}

// checkBarcode refuses a barcode mhVTL would not read back whole: sscanf
// "Slot %d: %s" stops at the first white space
// (mhvtl usr/cmd/vtllibrary.c:1055).
func checkBarcode(kind, barcode string) error {
	if kind == "Drive" && barcode != "" {
		return ErrDriveBarcode
	}
	if kind != "Slot" && kind != "MAP" && barcode != "" {
		return fmt.Errorf("library_contents: a %s line takes no barcode", kind)
	}
	for _, r := range barcode {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("library_contents: invalid barcode %q", barcode)
		}
	}
	return nil
}

func entryText(kind string, n int, barcode string) string {
	if barcode == "" {
		return fmt.Sprintf("%s %d:", kind, n)
	}
	return fmt.Sprintf("%s %d: %s", kind, n, barcode)
}

func checkKind(kind string) error {
	switch kind {
	case "Drive", "Picker", "MAP", "Slot":
		return nil
	}
	return fmt.Errorf("library_contents: invalid entry kind %q", kind)
}

// SetBarcode sets or clears (barcode "") the cartridge of a Slot or MAP. The
// line keeps everything up to its colon. Drive lines are refused
// (ErrDriveBarcode).
func (c *LibContents) SetBarcode(kind string, n int, barcode string) error {
	if err := checkKind(kind); err != nil {
		return err
	}
	if err := checkBarcode(kind, barcode); err != nil {
		return err
	}
	j := c.lookup(kind, n)
	if j < 0 {
		return fmt.Errorf("%s %d: %w", kind, n, ErrNotFound)
	}
	i := c.entries[j].Line
	text := c.lines[i].Text
	prefix := text[:strings.IndexByte(text, ':')+1]
	if barcode != "" {
		prefix += " " + barcode
	}
	c.lines[i].Text = prefix
	c.index()
	return nil
}

// AddEntry adds "Kind N:" (with a barcode for Slot and MAP) after the last
// entry of the same kind, or at the end of the file after a blank line.
func (c *LibContents) AddEntry(kind string, n int, barcode string) error {
	if err := checkKind(kind); err != nil {
		return err
	}
	if err := checkBarcode(kind, barcode); err != nil {
		return err
	}
	if c.lookup(kind, n) >= 0 {
		return fmt.Errorf("%s %d already exists", kind, n)
	}
	nl := eol(c.lines)
	add := []Line{{Text: entryText(kind, n, barcode), EOL: nl}}
	at := -1
	for _, e := range c.entries {
		if e.Kind == kind {
			at = e.Line + 1
		}
	}
	if at < 0 {
		at = len(c.lines)
		if at > 0 && !isBlank(c.lines[at-1].Text) {
			add = append([]Line{{EOL: nl}}, add...)
		}
	}
	c.lines = insertLines(c.lines, at, add)
	c.index()
	return nil
}

// RemoveEntry removes every line for kind and n.
func (c *LibContents) RemoveEntry(kind string, n int) error {
	if c.lookup(kind, n) < 0 {
		return fmt.Errorf("%s %d: %w", kind, n, ErrNotFound)
	}
	for j := len(c.entries) - 1; j >= 0; j-- {
		if e := c.entries[j]; e.Kind == kind && e.Number == n {
			c.lines = deleteLines(c.lines, e.Line, e.Line+1)
		}
	}
	c.index()
	return nil
}
