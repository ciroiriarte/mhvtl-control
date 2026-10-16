// Package promtext writes metrics in the Prometheus text exposition format,
// version 0.0.4 (https://prometheus.io/docs/instrumenting/exposition_formats/),
// using only the standard library.
package promtext

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the Content-Type of the format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Metric types.
const (
	Gauge   = "gauge"
	Counter = "counter"
)

// Set is a set of metric families. Output is deterministic: families sorted
// by name, samples by their rendered labels.
type Set struct {
	families map[string]*Family
}

// Family is one metric name with its HELP, TYPE and samples.
type Family struct {
	name, help, typ string
	samples         map[string]float64 // rendered labels -> value
}

// Family returns the family name, creating it with help and typ on first
// use.
func (s *Set) Family(name, help, typ string) *Family {
	if s.families == nil {
		s.families = map[string]*Family{}
	}
	f, ok := s.families[name]
	if !ok {
		f = &Family{name: name, help: help, typ: typ, samples: map[string]float64{}}
		s.families[name] = f
	}
	return f
}

// Add adds v to the sample with the given labels, alternating names and
// values ("library", "10"). Adding to the same labels again sums, so a
// family can count things as they are seen.
func (f *Family) Add(v float64, labels ...string) {
	f.samples[renderLabels(labels)] += v
}

// Set sets the sample with the given labels to v.
func (f *Family) Set(v float64, labels ...string) {
	f.samples[renderLabels(labels)] = v
}

// WriteTo writes the set in the text format.
func (s *Set) WriteTo(w io.Writer) (int64, error) {
	cw := &countWriter{w: w}
	bw := bufio.NewWriter(cw)
	names := make([]string, 0, len(s.families))
	for n := range s.families {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := s.families[n]
		if len(f.samples) == 0 {
			continue
		}
		bw.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n")
		bw.WriteString("# TYPE " + f.name + " " + f.typ + "\n")
		keys := make([]string, 0, len(f.samples))
		for k := range f.samples {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bw.WriteString(f.name + k + " " + formatValue(f.samples[k]) + "\n")
		}
	}
	err := bw.Flush()
	return cw.n, err
}

// renderLabels renders name/value pairs as {a="x",b="y"}, or "" for none.
// A trailing name without a value is ignored.
func renderLabels(kv []string) string {
	if len(kv) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(kv[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

// escapeLabel escapes a label value: backslash, double quote and line feed.
// The format is UTF-8, so invalid bytes (a home path, say) become U+FFFD.
func escapeLabel(s string) string { return labelEscaper.Replace(strings.ToValidUTF8(s, "\uFFFD")) }

// escapeHelp escapes HELP text: backslash and line feed.
func escapeHelp(s string) string { return helpEscaper.Replace(s) }

// formatValue renders a sample value; the format spells infinities and NaN
// as +Inf, -Inf and NaN.
func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// countWriter counts the bytes written through it, for WriteTo.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
