package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"mhvtld/internal/changer"
	"mhvtld/internal/config"
	"mhvtld/internal/drivestats"
)

type fakeDrives struct {
	got []int
	st  drivestats.Stats
	err error
}

func (f *fakeDrives) Read(d config.Drive) (drivestats.Stats, error) {
	f.got = append(f.got, d.ID)
	return f.st, f.err
}

func TestDriveStats(t *testing.T) {
	fd := &fakeDrives{st: drivestats.Stats{Device: "/dev/sg3", SupportedPages: []string{"0x00", "0x0c"}}}
	h := NewServer(Config{ConfigDir: writeConfigDir(t), Drives: fd}).Handler()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/drives/11/stats", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"device":"/dev/sg3"`) {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(fd.got) != 1 || fd.got[0] != 11 {
		t.Errorf("read drives %v", fd.got)
	}
	cases := []struct {
		path   string
		err    error
		status int
	}{
		{"/api/v1/libraries/10/drives/99/stats", nil, 404},
		{"/api/v1/libraries/10/drives/x/stats", nil, 400},
		{"/api/v1/libraries/10/drives/11/stats", drivestats.ErrReserved, 409},
		{"/api/v1/libraries/10/drives/11/stats", fmt.Errorf("%w: gone", changer.ErrUnavailable), 503},
		{"/api/v1/libraries/10/drives/11/stats", drivestats.ErrUnavailable, 503},
		{"/api/v1/libraries/10/drives/11/stats", errors.New("odd"), 500},
	}
	for _, c := range cases {
		h := NewServer(Config{ConfigDir: writeConfigDir(t), Drives: &fakeDrives{err: c.err}}).Handler()
		if rec := doJSON(t, h, http.MethodGet, c.path, ""); rec.Code != c.status {
			t.Errorf("%s %v: status = %d, want %d", c.path, c.err, rec.Code, c.status)
		}
	}
	h = NewServer(Config{ConfigDir: writeConfigDir(t)}).Handler()
	if rec := doJSON(t, h, http.MethodGet, "/api/v1/libraries/10/drives/11/stats", ""); rec.Code != 503 {
		t.Errorf("no drive reader: status = %d, want 503", rec.Code)
	}
}
