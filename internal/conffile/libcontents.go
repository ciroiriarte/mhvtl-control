package conffile

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrDriveBarcode is returned for an edit that would put a value on a Drive
// line. That value is not a cartridge: it sets the drive's serial number as
// the changer reports it (init_drive_slot -> set_drive_serial, mhvtl
// usr/cmd/vtllibrary.c:850-881 and :621). device.conf's "Unit serial number"
// overrides it, since update_drive_details reads device.conf after
// init_slot_info (:1593-1595, :1723-1725), so it counts only for a drive
// without one. The editor keeps serial numbers in device.conf only.
var ErrDriveBarcode = errors.New("library_contents: a Drive line's value is the drive's serial number; set it in device.conf")

// maxBarcode is MAX_BARCODE_LEN (mhvtl include/vtllib.h:90). add_barcode
// exits on a longer barcode, and on a duplicate one (mhvtl
// usr/cmd/vtllibrary.c:324-331), but it compares the stored barcode, padded
// to 16 with "%-16.16s" (:345), with strncmp(..., 17) (lookup_barcode,
// :309): only a 16-character duplicate matches.
const maxBarcode = 16

// Entry is one "Drive N:", "Picker N:", "MAP N: X" or "Slot N: X" line.
type Entry struct {
	Kind   string // "Drive", "Picker", "MAP" or "Slot"
	Number int
	// Barcode is the value as sscanf "Slot %d: %s" (mhvtl
	// usr/cmd/vtllibrary.c:1030-1063) reads it: the first word after the
	// colon, empty when the element is empty. That drops the padding of a
	// .persist file, which save_config writes with "%-16.16s" (:345, :1206).
	// On a Drive line it is the drive's serial number (see DriveSerial).
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
	problem := func(i int, ambiguous bool, format string, a ...any) {
		c.Problems = append(c.Problems, Problem{Line: i + 1, Ambiguous: ambiguous, Msg: fmt.Sprintf(format, a...)})
	}
	for i, l := range c.lines {
		if msg := lineProblem(l); msg != "" {
			problem(i, true, "%s", msg)
		}
		// mhVTL skips only a column-1 '#' (mhvtl usr/cmd/vtllibrary.c:1025),
		// but an indented one fails every sscanf pattern there and is
		// ignored too.
		text := cTrim(l.Text)
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
			problem(i, false, "missing ':' in %q", text)
			continue
		}
		f := cFields(head)
		if len(f) != 2 {
			problem(i, false, "malformed %q", text)
			continue
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			problem(i, false, "bad number %q", f[1])
			continue
		}
		switch f[0] {
		case "Drive", "Picker", "MAP", "Slot":
		default:
			problem(i, false, "unknown entry %q", f[0])
			continue
		}
		// The sscanf formats start with the kind, so mhVTL ignores an
		// indented entry.
		if l.Text[0] == ' ' || l.Text[0] == '\t' {
			problem(i, true, "indented %s %d: mhVTL ignores this line", f[0], n)
			continue
		}
		e := Entry{Kind: f[0], Number: n, Line: i}
		words := cFields(rest)
		switch {
		case strings.TrimRight(head, " \t") != head:
			// sscanf stops at the blank before ':' with the number read, and
			// the element is created empty.
			problem(i, true, "blank before ':' in %q: mhVTL reads %s %d as empty", text, f[0], n)
		case len(words) > 1:
			problem(i, true, "white space inside %q: mhVTL reads only %q", cTrim(rest), words[0])
			e.Barcode = words[0]
		case len(words) == 1:
			e.Barcode = words[0]
		}
		c.entries = append(c.entries, e)
	}

	// Whole-file checks of what vtllibrary does with the entries.
	seen := map[string]int{}
	last := map[string]int{}
	number := map[[2]string]bool{}
	for _, e := range c.entries {
		// MAP and Picker elements are created per line with no hole
		// filling (init_map_slot, init_transport_slot, :883-925), so a
		// repeated number creates the element twice.
		if e.Kind == "MAP" || e.Kind == "Picker" {
			k := [2]string{e.Kind, strconv.Itoa(e.Number)}
			if number[k] {
				problem(e.Line, true, "%s %d listed twice: mhVTL creates the element twice", e.Kind, e.Number)
			}
			number[k] = true
		}
		// Drives and slots fill holes up to each number in turn
		// (vtllibrary.c:1032-1063). A slot number not above the previous
		// one creates the slot twice. init_drive_slot finds the existing
		// drive instead (lookup_drive, :859), but still counts it again
		// (num_drives++, :874).
		if e.Kind == "Drive" || e.Kind == "Slot" {
			if prev, ok := last[e.Kind]; ok && e.Number <= prev {
				effect := "mhVTL creates the slot twice"
				if e.Kind == "Drive" {
					effect = "vtllibrary counts the drive twice in num_drives"
				}
				problem(e.Line, true, "%s %d after %s %d: %s; keep them in ascending order", e.Kind, e.Number, e.Kind, prev, effect)
			}
			last[e.Kind] = e.Number
		}
		if e.Kind == "Drive" || e.Barcode == "" {
			continue
		}
		if len(e.Barcode) > maxBarcode {
			problem(e.Line, true, "barcode %q is longer than %d characters: vtllibrary exits on start", e.Barcode, maxBarcode)
		}
		if j, dup := seen[e.Barcode]; dup {
			if len(e.Barcode) == maxBarcode {
				problem(e.Line, true, "barcode %s already at line %d: vtllibrary exits on start", e.Barcode, j+1)
			} else {
				problem(e.Line, true, "barcode %s already at line %d: vtllibrary loads both as one cartridge's media", e.Barcode, j+1)
			}
			continue
		}
		seen[e.Barcode] = e.Line
	}
	sort.SliceStable(c.Problems, func(a, b int) bool { return c.Problems[a].Line < c.Problems[b].Line })
}

// Writable is DeviceConf.Writable for library_contents.
func (c *LibContents) Writable() error { return writable("library_contents", c.Problems) }

// DriveSerial returns the serial number the last "Drive n: X" line sets for
// drive n, if any (see ErrDriveBarcode).
func (c *LibContents) DriveSerial(n int) (string, bool) {
	if j := c.lookup("Drive", n); j >= 0 && c.entries[j].Barcode != "" {
		return c.entries[j].Barcode, true
	}
	return "", false
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

// checkBarcode refuses a barcode mhVTL would not read back whole, sscanf
// "Slot %d: %s" stopping at the first white space (mhvtl
// usr/cmd/vtllibrary.c:1055), or one add_barcode exits on for its length
// (:324-328). Duplicates are a whole-file matter, reported in Problems.
func checkBarcode(kind, barcode string) error {
	if kind == "Drive" && barcode != "" {
		return ErrDriveBarcode
	}
	// vtllibrary loads a Picker barcode (init_transport_slot,
	// vtllibrary.c:906-924), but a cartridge goes in a Slot or MAP here.
	if kind != "Slot" && kind != "MAP" && barcode != "" {
		return fmt.Errorf("library_contents: the editor puts cartridges only in Slot and MAP lines, not %s", kind)
	}
	for _, r := range barcode {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("library_contents: invalid barcode %q", barcode)
		}
	}
	if len(barcode) > maxBarcode {
		return fmt.Errorf("library_contents: barcode %q is longer than %d characters", barcode, maxBarcode)
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

// AddEntry adds "Kind N:" (with a barcode for Slot and MAP) in numeric order
// among the entries of its kind: before the first with a higher number, else
// after the last. vtllibrary creates the missing drives and slots below each
// number as it reads them (vtllibrary.c:1032-1063), so one out of order would
// be created twice. With no entry of its kind it goes at the end of the
// file, after a blank line.
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
		if e.Kind != kind {
			continue
		}
		if e.Number > n {
			at = e.Line
			break
		}
		at = e.Line + 1
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
