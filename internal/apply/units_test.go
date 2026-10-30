package apply

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestParseShow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   string
		names []string
		want  []UnitStatus
		err   bool
	}{
		{
			name:  "value with =",
			out:   "LoadState=loaded\nActiveState=active\nResult=a=b\n",
			names: []string{"a.service"},
			want:  []UnitStatus{{Name: "a.service", LoadState: "loaded", ActiveState: "active", Result: "a=b"}},
		},
		{
			name:  "empty values",
			out:   "LoadState=loaded\nActiveState=\nSubState=\nMainPID=\nActiveEnterTimestampMonotonic=\n",
			names: []string{"mhvtl.target"},
			want:  []UnitStatus{{Name: "mhvtl.target", LoadState: "loaded"}},
		},
		{
			name:  "bad numbers are left 0",
			out:   "LoadState=loaded\nMainPID=x\nActiveEnterTimestampMonotonic=99999999999999999999999\n\nLoadState=loaded\nMainPID=7\n",
			names: []string{"a.service", "b.service"},
			want:  []UnitStatus{{Name: "a.service", LoadState: "loaded"}, {Name: "b.service", LoadState: "loaded", MainPID: 7}},
		},
		{
			name:  "trailing blank lines",
			out:   "LoadState=loaded\n\nLoadState=not-found\n\n\n",
			names: []string{"a.service", "b.service"},
			want:  []UnitStatus{{Name: "a.service", LoadState: "loaded"}, {Name: "b.service", LoadState: "not-found"}},
		},
		// systemctl does not emit CRLF; a CRLF blank line is then no
		// separator, and the count refuses it rather than misassign.
		{name: "CRLF", out: "LoadState=loaded\r\n\r\nLoadState=loaded\r\n", names: []string{"a.service", "b.service"}, err: true},
		{name: "fewer blocks", out: "LoadState=loaded\n", names: []string{"a.service", "b.service"}, err: true},
		{name: "more blocks", out: "LoadState=loaded\n\nLoadState=loaded\n", names: []string{"a.service"}, err: true},
		{name: "empty", out: "", names: []string{"a.service"}, err: true},
	} {
		got, err := parseShow([]byte(tc.out), tc.names)
		if (err != nil) != tc.err || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, %v", tc.name, got, err)
		}
	}
}

// A drive whose MainPID does not parse leaves only that field empty: the
// libraries' states, which config staging reads, are still answered.
func TestUnitsBadField(t *testing.T) {
	e := newEnv(t)
	e.cfg.Systemctl = func(_ context.Context, args ...string) ([]byte, error) {
		var b []string
		for _, u := range args[2:] {
			switch u {
			case "vtllibrary@10.service":
				b = append(b, "LoadState=loaded\nActiveState=active\nMainPID=10\n")
			case "vtltape@11.service":
				b = append(b, "LoadState=loaded\nActiveState=active\nMainPID=garbage\nActiveEnterTimestampMonotonic=5\n")
			default:
				b = append(b, "LoadState=loaded\nActiveState=inactive\nMainPID=0\n")
			}
		}
		return []byte(strings.Join(b, "\n")), nil
	}
	e.rebuild()
	resp := e.do(Request{Op: OpUnits})
	if resp.Result != ResultOK || !reflect.DeepEqual(resp.LibraryStates, map[int]string{10: "active", 30: "inactive"}) {
		t.Fatalf("units: %+v", resp)
	}
	for _, u := range resp.UnitStates {
		if u.Name == "vtltape@11.service" && (u.MainPID != 0 || u.ActiveState != "active" || u.ActiveEnterMonotonicUS != 5) {
			t.Errorf("drive 11: %+v", u)
		}
		if u.Name == "vtllibrary@10.service" && u.MainPID != 10 {
			t.Errorf("library 10: %+v", u)
		}
	}
}
