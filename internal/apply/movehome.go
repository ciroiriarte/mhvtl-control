package apply

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// MoveHomeDir holds mhvtl-move-home's journals, one directory per move:
// <MoveHomeDir>/<id>/journal.json (plan 5.3).
const MoveHomeDir = "/var/lib/mhvtl-move-home"

// moveHomeJournal is the part of a move-home journal the helper reads:
// its step. The journal is written atomically by mhvtl-move-home; Phase 5
// adds the rest of its fields.
type moveHomeJournal struct {
	Step string `json:"step"`
}

// moveHomeSettled are the steps in which a move is over: done, cleaned
// and rolled_back. A planned move has touched nothing yet, but it was
// planned against the configuration an apply would change, and running
// it after the apply would switch homes from a stale plan, so it refuses
// an apply too, with its own message: run it or cancel it first. Every
// other step (quiesced, copying, copied, verified, switching, switched,
// restarted, cleaning) is a move in progress or interrupted: its
// consumers may be stopped, its homes half copied, and mhvtl.conf or
// device.conf about to be switched.
var moveHomeSettled = map[string]bool{"done": true, "cleaned": true, "rolled_back": true}

// checkMoveHome refuses an apply while any move-home journal under dir is
// planned, in an unsettled step, or cannot be read: an apply then would
// write the configuration the move is switching or was planned against,
// or start the units it stopped (plan 3.3, exec step 1). A missing dir is
// no move.
func checkMoveHome(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("move-home journals: %w", err)
	}
	var busy, planned []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "journal.json")
		step, err := readMoveHomeStep(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return refuse(ErrMoveHome, "%s: %v; settle the move with mhvtl-move-home first", path, err)
		}
		switch {
		case step == "planned":
			planned = append(planned, e.Name())
		case !moveHomeSettled[step]:
			busy = append(busy, e.Name()+" ("+step+")")
		}
	}
	if len(busy) > 0 {
		sort.Strings(busy)
		return refuse(ErrMoveHome, "media home moves in progress: %s; resume or roll them back with mhvtl-move-home first", strings.Join(busy, ", "))
	}
	if len(planned) > 0 {
		sort.Strings(planned)
		return refuse(ErrMoveHome, "media home moves planned and not run: %s; run or cancel them with mhvtl-move-home first, since the apply would change the configuration they were planned against", strings.Join(planned, ", "))
	}
	return nil
}

func readMoveHomeStep(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxIntent))
	if err != nil {
		return "", err
	}
	var j moveHomeJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return "", err
	}
	if j.Step == "" {
		return "", errors.New("no step")
	}
	return j.Step, nil
}

// bayMonitor is mhVTL's raw-disk bay monitor (etc/mhvtl-bay-monitor.service.in
// at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/etc/mhvtl-bay-monitor.service.in).
const (
	bayMonitor = "mhvtl-bay-monitor.service"
	baysConf   = "mhvtl_raw_bays.conf"
)

// bayTargets lists the libraries the bay map in configDir binds bays to:
// the first field of each line that is not blank or a comment, as
// raw_bay_conf.c parses it ("<library_id> <map_slot> <by-path> [by-id=]",
// usr/raw_bay_conf.c:62-118 at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/raw_bay_conf.c#L62-L118).
// No bay map means no targets.
func bayTargets(configDir string) (map[int]bool, error) {
	b, err := os.ReadFile(filepath.Join(configDir, baysConf))
	if errors.Is(err, fs.ErrNotExist) {
		return map[int]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if id, err := strconv.Atoi(f[0]); err == nil && id >= 0 {
			out[id] = true
		}
	}
	return out, sc.Err()
}
