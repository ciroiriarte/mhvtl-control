package conffile

import (
	"fmt"
	"strings"
)

// ModelledKeys are the mhvtl.conf keys MhvtlConf.Set accepts.
var ModelledKeys = []string{"CAPACITY", "VERBOSE", "VTL_DEBUG", "MHVTL_HOME_PATH", "MHVTL_CONFIG_PATH"}

// cReadKeys are the keys mhVTL's C code reads from mhvtl.conf itself
// (mhvtl usr/vtllib.c:288-301); the rest only reach it through the shell or
// systemd's EnvironmentFile.
var cReadKeys = map[string]bool{"MHVTL_CONFIG_PATH": true}

// assignment is a parsed "KEY=VALUE" line, split so that only the value
// token is ever rewritten: prefix is everything up to the value (indent,
// "export ", key, '=' and spacing), quote the value's quote character (0
// when unquoted), and suffix whatever follows the value.
type assignment struct {
	key, value       string
	prefix, suffix   string
	quote            byte
	indented, export bool
	// spaced is set for blanks around '=': mhVTL's sscanf accepts them, the
	// shell does not read the line as an assignment.
	spaced bool
	line   int
}

// MhvtlConf is a lossless mhvtl.conf. It is read two ways. The shell (the
// init script, make_vtl_media) and systemd's EnvironmentFile read
// KEY=VALUE assignments. mhVTL's C code reads MHVTL_CONFIG_PATH only, with
// sscanf "%[^= \t\r] = %[^\n]" (mhvtl usr/vtllib.c:291): the value runs to
// the end of the line, quotes, trailing text and a CRLF's '\r' included; an
// indented or "export" line is skipped as a syntax error; and the first
// assignment wins, where the shell takes the last. For that key every such
// line is a Problem.
type MhvtlConf struct {
	lines    []Line
	assigns  []assignment
	Problems []Problem
}

// Writable is DeviceConf.Writable for mhvtl.conf.
func (c *MhvtlConf) Writable() error { return writable("mhvtl.conf", c.Problems) }

// ParseMhvtlConf parses mhvtl.conf. It never fails: lines that are not a
// simple assignment are kept verbatim and not modelled.
func ParseMhvtlConf(data []byte) *MhvtlConf {
	c := &MhvtlConf{lines: splitLines(data)}
	c.index()
	return c
}

func (c *MhvtlConf) index() {
	c.assigns, c.Problems = nil, nil
	seen := map[string]int{}
	for i, l := range c.lines {
		if strings.IndexByte(l.Text, 0) >= 0 {
			c.Problems = append(c.Problems, Problem{Line: i + 1, Ambiguous: true,
				Msg: "line contains a NUL byte: mhVTL reads it only up to there"})
		}
		a, ok := parseAssignment(l.Text)
		if !ok {
			continue
		}
		a.line = i
		// The shell runs "KEY = value" as a command, so it is no
		// assignment to model; mhVTL's sscanf still reads it.
		if !a.spaced {
			c.assigns = append(c.assigns, a)
		}
		if !cReadKeys[a.key] {
			continue
		}
		msg := cProblem(a, l)
		if j, dup := seen[a.key]; dup && msg == "" {
			msg = fmt.Sprintf("set again after line %d: mhVTL uses the first, the shell the last", j+1)
		}
		if _, dup := seen[a.key]; !dup {
			seen[a.key] = i
		}
		if msg != "" {
			c.Problems = append(c.Problems, Problem{Line: i + 1, Ambiguous: true, Msg: a.key + ": " + msg})
		}
	}
}

// cProblem reports why mhVTL's sscanf would not read the assignment as the
// shell does, or "".
func cProblem(a assignment, l Line) string {
	switch {
	case a.spaced:
		return "blanks around '=': mhVTL reads the line, the shell does not"
	case a.indented:
		return "indented: mhVTL skips the line"
	case a.export:
		return "\"export\": mhVTL skips the line"
	case a.quote != 0:
		return "quoted: mhVTL takes the quotes into the value"
	case a.suffix != "":
		return "text after the value: mhVTL takes it into the value"
	case l.EOL == "\r\n":
		return "CRLF line ending: mhVTL takes the '\\r' into the value"
	}
	return ""
}

func parseAssignment(text string) (assignment, bool) {
	p := 0
	skipSpace := func() {
		for p < len(text) && (text[p] == ' ' || text[p] == '\t') {
			p++
		}
	}
	skipSpace()
	indented := p > 0
	export := strings.HasPrefix(text[p:], "export ")
	if export {
		p += len("export ")
		skipSpace()
	}
	k := p
	for p < len(text) && (text[p] == '_' || isAlnum(text[p])) {
		p++
	}
	if p == k || (text[k] >= '0' && text[k] <= '9') {
		return assignment{}, false
	}
	key := text[k:p]
	eq := p
	skipSpace()
	if p == len(text) || text[p] != '=' {
		return assignment{}, false
	}
	spaced := p > eq
	p++
	eq = p
	skipSpace()
	spaced = spaced || p > eq
	a := assignment{key: key, prefix: text[:p], indented: indented, export: export, spaced: spaced}
	rest := strings.TrimSuffix(text[p:], "\r")
	if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
		end := strings.IndexByte(rest[1:], rest[0])
		if end < 0 || strings.ContainsRune(rest[1:end+1], '\\') {
			return assignment{}, false // unterminated, or escapes: not modelled
		}
		a.quote, a.value, a.suffix = rest[0], rest[1:end+1], text[p+end+2:]
		return a, true
	}
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	a.value, a.suffix = rest[:end], text[p+end:]
	return a, true
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// Lines returns the file's lines. The slice must not be modified.
func (c *MhvtlConf) Lines() []Line { return c.lines }

// Get returns the unquoted value of the last assignment to key.
func (c *MhvtlConf) Get(key string) (string, bool) {
	for j := len(c.assigns) - 1; j >= 0; j-- {
		if c.assigns[j].key == key {
			return c.assigns[j].value, true
		}
	}
	return "", false
}

// Keys returns every key the file assigns, each once, in order of first
// assignment.
func (c *MhvtlConf) Keys() []string {
	var keys []string
	seen := map[string]bool{}
	for _, a := range c.assigns {
		if !seen[a.key] {
			seen[a.key] = true
			keys = append(keys, a.key)
		}
	}
	return keys
}

// Render returns the file's bytes.
func (c *MhvtlConf) Render() []byte { return renderLines(c.lines) }

// Set sets one of ModelledKeys. The last assignment to the key keeps its
// quoting and anything after the value; a missing key is appended as
// KEY=value. Values are restricted to characters that need no quoting. For
// a key the C code reads, Set refuses when that would not give the shell and
// the C code the same value: a Problem on the key's line, or a new line in
// a CRLF file.
func (c *MhvtlConf) Set(key, value string) error {
	modelled := false
	for _, k := range ModelledKeys {
		modelled = modelled || k == key
	}
	if !modelled {
		return fmt.Errorf("mhvtl.conf: %s is not an editable key", key)
	}
	for _, r := range value {
		if !(r < 0x80 && (isAlnum(byte(r)) || strings.ContainsRune("_./:@+,-", r))) {
			return fmt.Errorf("mhvtl.conf: invalid value %q for %s", value, key)
		}
	}
	if cReadKeys[key] {
		for _, p := range c.Problems {
			if strings.HasPrefix(p.Msg, key+": ") {
				return fmt.Errorf("mhvtl.conf: %w", p)
			}
		}
	}
	for j := len(c.assigns) - 1; j >= 0; j-- {
		a := c.assigns[j]
		if a.key != key {
			continue
		}
		v := value
		if a.quote != 0 {
			v = string(a.quote) + v + string(a.quote)
		}
		c.lines[a.line].Text = a.prefix + v + a.suffix
		c.index()
		return nil
	}
	if cReadKeys[key] && eol(c.lines) == "\r\n" {
		return fmt.Errorf("mhvtl.conf: %s: CRLF line endings: mhVTL would take the '\\r' into the value", key)
	}
	c.lines = insertLines(c.lines, len(c.lines), []Line{{Text: key + "=" + value, EOL: eol(c.lines)}})
	c.index()
	return nil
}
