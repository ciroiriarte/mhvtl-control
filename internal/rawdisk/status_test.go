package rawdisk

import (
	"errors"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	sb := &Superblock{Barcode: "VOL001L5"}
	cases := []struct {
		name  string
		sb    *Superblock
		err   error
		inUse bool
		want  string
	}{
		{"not formatted", nil, ErrNotFormatted, false, "no_mvtl"},
		{"bad crc", nil, ErrBadCRC, false, "probe_failed"},
		{"unsupported version", nil, ErrUnsupportedVersion, false, "probe_failed"},
		{"generic io error", nil, errors.New("permission denied"), false, "probe_failed"},
		{"valid mvtl", sb, nil, false, "mvtl"},
		{"in use precedence over mvtl", sb, nil, true, "in_use"},
		{"in use precedence over error", nil, ErrNotFormatted, true, "in_use"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyStatus(c.sb, c.err, c.inUse); got != c.want {
				t.Fatalf("classifyStatus = %q, want %q", got, c.want)
			}
		})
	}
}
