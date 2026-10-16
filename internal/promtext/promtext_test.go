package promtext

import (
	"math"
	"strings"
	"testing"
)

func TestWriteTo(t *testing.T) {
	var s Set
	up := s.Family("b_up", "Whether it is up.", Gauge)
	up.Set(1, "library", "30")
	up.Set(0, "library", "10")
	req := s.Family("a_requests_total", "Requests.\nBy route \\ code.", Counter)
	req.Add(1, "route", "GET /x", "code", "200")
	req.Add(2, "route", "GET /x", "code", "200")
	s.Family("c_empty", "No samples, not written.", Gauge)
	s.Family("d_plain", "No labels.", Gauge).Set(math.Inf(1))

	var b strings.Builder
	n, err := s.WriteTo(&b)
	if err != nil {
		t.Fatal(err)
	}
	want := `# HELP a_requests_total Requests.\nBy route \\ code.
# TYPE a_requests_total counter
a_requests_total{route="GET /x",code="200"} 3
# HELP b_up Whether it is up.
# TYPE b_up gauge
b_up{library="10"} 0
b_up{library="30"} 1
# HELP d_plain No labels.
# TYPE d_plain gauge
d_plain +Inf
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	if n != int64(len(want)) {
		t.Errorf("n = %d, want %d", n, len(want))
	}
}

func TestEscapeLabel(t *testing.T) {
	var s Set
	s.Family("m", "h", Gauge).Set(1.5, "home", "/a\\b\"c\nd")
	var b strings.Builder
	if _, err := s.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `m{home="/a\\b\"c\nd"} 1.5`+"\n") {
		t.Fatalf("got %q", b.String())
	}
}

func TestFormatValue(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 3: "3", 0.25: "0.25", 1e21: "1e+21", math.Inf(-1): "-Inf"} {
		if got := formatValue(v); got != want {
			t.Errorf("formatValue(%v) = %q, want %q", v, got, want)
		}
	}
	if got := formatValue(math.NaN()); got != "NaN" {
		t.Errorf("NaN = %q", got)
	}
}
