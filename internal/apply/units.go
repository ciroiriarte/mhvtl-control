package apply

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"mhvtl-control/internal/conffile"
)

// unitsTimeout bounds a units request's systemctl call. It reads a few
// properties of a few dozen units from the service manager, normally in
// milliseconds; 5 s allows a busy manager and ends well before the daemon
// gives up on the request.
var unitsTimeout = 5 * time.Second

// mhvtlTarget and supportUnits are mhVTL's target and the units besides the
// instances that are PartOf= it (etc/mhvtl.target and etc/*.service.in at
// ebbda22, https://github.com/markh794/mhvtl/tree/ebbda22646e72106b7f476a5672adc8be915bfc9/etc).
const mhvtlTarget = "mhvtl.target"

var supportUnits = []string{"mhvtl-load-modules.service", "mhvtl-tcmu-handler.service", "mhvtl-media.service", bayMonitor}

// unitProps are the properties units reads. systemctl show prints them in
// its own order, so the parser goes by name.
var unitProps = []string{"LoadState", "ActiveState", "SubState", "Result", "MainPID", "ActiveEnterTimestampMonotonic"}

// unitStates reads, with one systemctl show, mhvtl.target, its support units
// and the vtllibrary@ and vtltape@ instance of every stanza of device.conf,
// standalone drives included. It returns them in that order, and the
// ActiveState of each library's vtllibrary@.
func (h *Helper) unitStates(ctx context.Context) (map[int]string, []UnitStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, unitsTimeout)
	defer cancel()
	b, err := os.ReadFile(filepath.Join(h.cfg.ConfigDir, deviceConf))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	names := append([]string{mhvtlTarget}, supportUnits...)
	libs := map[string]int{}
	seen := map[string]bool{}
	for _, st := range conffile.ParseDeviceConf(b).Stanzas() {
		var name string
		switch st.Kind {
		case "Library":
			name = libraryUnit(st.ID)
			libs[name] = st.ID
		case "Drive":
			name = driveUnit(st.ID)
		default:
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	out, err := h.cfg.Systemctl(ctx, append([]string{"show", "--property=" + strings.Join(unitProps, ",")}, names...)...)
	if err != nil {
		return nil, nil, fmt.Errorf("systemctl show: %w", err)
	}
	units, err := parseShow(out, names)
	if err != nil {
		return nil, nil, err
	}
	if h.cfg.Monotonic != nil {
		if now, err := h.cfg.Monotonic(); err == nil {
			for i := range units {
				u := &units[i]
				if enter := time.Duration(u.ActiveEnterMonotonicUS) * time.Microsecond; u.ActiveState == "active" && enter > 0 && now > enter {
					u.UptimeUS = uint64((now - enter) / time.Microsecond)
				}
			}
		}
	}
	states := map[int]string{}
	for _, u := range units {
		if id, ok := libs[u.Name]; ok {
			states[id] = u.ActiveState
		}
	}
	return states, units, nil
}

// parseShow parses the output of systemctl show for the units names: one
// block of Key=Value lines per unit, in the order given, separated by blank
// lines. A unit systemd does not know still has its block (LoadState
// not-found).
func parseShow(out []byte, names []string) ([]UnitStatus, error) {
	var blocks [][]byte
	for _, b := range bytes.Split(bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
		if len(bytes.TrimSpace(b)) > 0 {
			blocks = append(blocks, b)
		}
	}
	if len(blocks) != len(names) {
		return nil, fmt.Errorf("systemctl show: %d units in the answer, asked for %d", len(blocks), len(names))
	}
	units := make([]UnitStatus, len(names))
	for i, b := range blocks {
		u := UnitStatus{Name: names[i]}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			var err error
			switch k {
			case "LoadState":
				u.LoadState = v
			case "ActiveState":
				u.ActiveState = v
			case "SubState":
				u.SubState = v
			case "Result":
				u.Result = v
			case "MainPID":
				u.MainPID, err = strconv.Atoi(v)
			case "ActiveEnterTimestampMonotonic":
				u.ActiveEnterMonotonicUS, err = strconv.ParseUint(v, 10, 64)
			}
			if err != nil {
				return nil, fmt.Errorf("systemctl show %s: %s: %w", names[i], k, err)
			}
		}
		units[i] = u
	}
	return units, nil
}

// monotonic reads CLOCK_MONOTONIC (clock_gettime(2)), which the Go runtime
// does not expose.
func monotonic() (time.Duration, error) {
	const clockMonotonic = 1 // CLOCK_MONOTONIC, linux/time.h
	var ts syscall.Timespec
	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return 0, errno
	}
	return time.Duration(ts.Nano()), nil
}
