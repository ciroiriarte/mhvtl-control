package conffile

import (
	"fmt"
	"strconv"
	"strings"
)

// isCSpace is C's isspace in the "C" locale, the white space sscanf skips.
// Go's unicode.IsSpace, behind strings.Fields and TrimSpace, also takes
// NBSP, U+0085 and other Unicode spaces, which C does not.
func isCSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r'
}

// isBlankRune is a blank to conf_value (mhvtl usr/vtllib.c:1093-1105):
// space or tab only.
func isBlankRune(r rune) bool { return r == ' ' || r == '\t' }

func cFields(s string) []string { return strings.FieldsFunc(s, isCSpace) }

func cTrim(s string) string { return strings.TrimFunc(s, isCSpace) }

// canonicalKey is a key as conf_value matches it: each run of blanks one
// space.
func canonicalKey(k string) string { return strings.Join(strings.FieldsFunc(k, isBlankRune), " ") }

// cText is the line as readline hands it to sscanf: a CRLF's '\r' stays.
func cText(l Line) string {
	if l.EOL == "\r\n" {
		return l.Text + "\r"
	}
	return l.Text
}

// cScanf returns what glibc's sscanf(s, format, ...) returns, for the
// directives mhVTL's device.conf readers use: white space, literal bytes,
// and the %d, %ld and %02x conversions. That is the number of conversions
// made, or -1 (EOF) when the input ends before the first one. C tests most
// of these calls for truth, so -1 counts as a match there.
func cScanf(s, format string) int {
	n, _ := cScan(s, format)
	return n
}

// cScan is cScanf that also returns the converted values. A value too large
// for an int64 is returned as 0.
func cScan(s, format string) (int, []int64) {
	n, i := 0, 0
	var vals []int64
	eof := func() (int, []int64) {
		if n == 0 {
			return -1, vals
		}
		return n, vals
	}
	for j := 0; j < len(format); j++ {
		f := format[j]
		switch {
		case isCSpace(rune(f)):
			for i < len(s) && isCSpace(rune(s[i])) {
				i++
			}
		case f == '%':
			j++
			width := 0
			for format[j] >= '0' && format[j] <= '9' {
				width = width*10 + int(format[j]-'0')
				j++
			}
			if format[j] == 'l' {
				j++
			}
			hex := format[j] == 'x'
			for i < len(s) && isCSpace(rune(s[i])) {
				i++
			}
			if i == len(s) {
				return eof()
			}
			if width == 0 {
				width = len(s)
			}
			start := i
			if s[i] == '+' || s[i] == '-' {
				i++
			}
			digits := 0
			for ; i < len(s) && i-start < width && isDigit(s[i], hex); i++ {
				digits++
			}
			if digits == 0 {
				return n, vals
			}
			base := 10
			if hex {
				base = 16
			}
			v, _ := strconv.ParseInt(strings.TrimPrefix(s[start:i], "+"), base, 64)
			vals = append(vals, v)
			n++
		default:
			if i == len(s) {
				return eof()
			}
			if s[i] != f {
				return n, vals
			}
			i++
		}
	}
	return n, vals
}

func isDigit(c byte, hex bool) bool {
	return c >= '0' && c <= '9' || hex && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')
}

const naaFormat = " NAA: %02x:%02x:%02x:%02x:%02x:%02x:%02x:%02x"

// ScanNAA returns what sscanf " NAA: %02x:...:%02x" (mhvtl
// usr/cmd/vtltape.c:2217, usr/cmd/vtllibrary.c:1342) returns for an NAA
// value: 8 for a valid one; 1 to 7 when mhVTL drops it for its default NAA;
// 0 or -1 when it ignores the line.
func ScanNAA(value string) int { return cScanf(" NAA: "+value, naaFormat) }

// scanfProblem reports an attribute line of a kind stanza that mhVTL reads
// with sscanf rather than conf_value, and does not read as written. b is
// the line as readline returns it (cText).
func scanfProblem(kind, key, b string) string {
	switch {
	case key == "Compression" && kind == "Drive":
		// vtltape.c:2204-2213 tests the first sscanf for truth: EOF (-1),
		// from an empty value or one that stops inside "factor", is taken
		// as a match, and so is "factor N" alone; both load uninitialised
		// ints into the compression settings.
		switch cScanf(b, " Compression: factor %d enabled %d") {
		case 2:
			return ""
		case 0:
			if cScanf(b, " Compression: %d") == 1 {
				return ""
			}
			return "Compression is neither 'factor N enabled M' nor a number: vtltape ignores it"
		}
		return "partial or empty Compression: vtltape's sscanf (vtltape.c:2204) takes it as a match and loads uninitialised compression settings; write 'factor N enabled M'"
	case key == "NAA":
		switch n := cScanf(b, naaFormat); {
		case n == 0:
			return "NAA is not 8 hex bytes: mhVTL ignores it"
		case n > 0 && n < 8:
			return fmt.Sprintf("NAA has %d of 8 hex bytes: mhVTL drops it and uses its default (vtltape.c:2217, vtllibrary.c:1342)", n)
		}
	case key == "Library ID" && kind == "Drive":
		// vtltape reads only the number (conf_value, vtltape.c:2176).
		if n := cScanf(b, " Library ID: %ld Slot: %d"); n >= 0 && n != 2 {
			return "Library ID without 'Slot: N': vtllibrary puts the drive in no slot (vtllibrary.c:686)"
		}
	}
	return ""
}
