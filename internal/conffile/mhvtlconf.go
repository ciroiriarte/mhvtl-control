package conffile

import (
	"fmt"
	"strings"
)

// ModelledKeys are the mhvtl.conf keys MhvtlConf.Set accepts.
var ModelledKeys = []string{"CAPACITY", "VERBOSE", "VTL_DEBUG", "MHVTL_HOME_PATH", "MHVTL_CONFIG_PATH"}

// assignment is a parsed "KEY=VALUE" line, split so that only the value
// token is ever rewritten: prefix is everything up to the value (indent,
// "export ", key, '=' and spacing), quote the value's quote character (0
// when unquoted), and suffix whatever follows the value.
type assignment struct {
	key, value     string
	prefix, suffix string
	quote          byte
	line           int
}

// MhvtlConf is a lossless mhvtl.conf: shell KEY=VALUE lines, read both by
// the shell (the init script, systemd EnvironmentFile) and by mhVTL itself
// with sscanf "%[^= \t\r] = %[^\n]" (mhvtl usr/vtllib.c:291), which takes
// the value to the end of the line, quotes included.
type MhvtlConf struct {
	lines   []Line
	assigns []assignment
}

// ParseMhvtlConf parses mhvtl.conf. It never fails: lines that are not a
// simple assignment are kept verbatim and not modelled.
func ParseMhvtlConf(data []byte) *MhvtlConf {
	c := &MhvtlConf{lines: splitLines(data)}
	c.index()
	return c
}

func (c *MhvtlConf) index() {
	c.assigns = nil
	for i, l := range c.lines {
		if a, ok := parseAssignment(l.Text); ok {
			a.line = i
			c.assigns = append(c.assigns, a)
		}
	}
}

func parseAssignment(text string) (assignment, bool) {
	p := 0
	skipSpace := func() {
		for p < len(text) && (text[p] == ' ' || text[p] == '\t') {
			p++
		}
	}
	skipSpace()
	if strings.HasPrefix(text[p:], "export ") {
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
	skipSpace()
	if p == len(text) || text[p] != '=' {
		return assignment{}, false
	}
	p++
	skipSpace()
	a := assignment{key: key, prefix: text[:p]}
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

// Render returns the file's bytes.
func (c *MhvtlConf) Render() []byte { return renderLines(c.lines) }

// Set sets one of ModelledKeys. The last assignment to the key keeps its
// quoting and anything after the value; a missing key is appended as
// KEY=value. Values are restricted to characters that need no quoting, so
// the shell and mhVTL's sscanf read the same thing.
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
	c.lines = insertLines(c.lines, len(c.lines), []Line{{Text: key + "=" + value, EOL: eol(c.lines)}})
	c.index()
	return nil
}
