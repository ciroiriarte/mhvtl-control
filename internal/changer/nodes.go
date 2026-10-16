package changer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"mhvtl-control/internal/config"
)

// Nodes are the device nodes of one SCSI device, as a backup application
// is configured with them. Node numbers change when mhVTL restarts, so they
// are resolved on every request.
type Nodes struct {
	// SG is the SCSI generic node (/dev/sgN).
	SG string `json:"sg,omitempty"`
	// ST and NST are a tape drive's rewinding and non-rewinding nodes
	// (/dev/stN, /dev/nstN), without the a/l/m density-mode variants.
	ST  string `json:"st,omitempty"`
	NST string `json:"nst,omitempty"`
	// SCH is a medium changer's st-style node (/dev/schN).
	SCH string `json:"sch,omitempty"`
	// ByID and ByPath are the udev links under /dev/tape that point at any
	// of the nodes above.
	ByID   []string `json:"by_id,omitempty"`
	ByPath []string `json:"by_path,omitempty"`
}

var (
	sgName  = regexp.MustCompile(`^sg[0-9]+$`)
	stName  = regexp.MustCompile(`^st[0-9]+$`)
	nstName = regexp.MustCompile(`^nst[0-9]+$`)
	schName = regexp.MustCompile(`^sch[0-9]+$`)
)

// Nodes returns the device nodes of the SCSI device behind sg: the
// scsi_tape and scsi_changer children of its sysfs device, and the
// /dev/tape/by-id and /dev/tape/by-path links to any of them. sg is a
// /dev/sgN path, or a link to one (a --changer or --drive override); any
// other path is an error that doesn't wrap ErrUnavailable.
func (l *Locator) Nodes(sg string) (Nodes, error) {
	name := filepath.Base(sg)
	if real, err := filepath.EvalSymlinks(sg); err == nil {
		name = filepath.Base(real)
	}
	if !sgName.MatchString(name) {
		return Nodes{}, fmt.Errorf("%s is not an sg node or a link to one", sg)
	}
	devDir := filepath.Join(l.SysRoot, "class", "scsi_generic", name, "device")
	if _, err := os.Stat(devDir); err != nil {
		return Nodes{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	n := Nodes{SG: filepath.Join(l.DevRoot, name)}
	targets := map[string]bool{name: true}
	for _, child := range childNames(filepath.Join(devDir, "scsi_tape")) {
		switch {
		case stName.MatchString(child):
			n.ST = filepath.Join(l.DevRoot, child)
			targets[child] = true
		case nstName.MatchString(child):
			n.NST = filepath.Join(l.DevRoot, child)
			targets[child] = true
		}
	}
	for _, child := range childNames(filepath.Join(devDir, "scsi_changer")) {
		if schName.MatchString(child) {
			n.SCH = filepath.Join(l.DevRoot, child)
			targets[child] = true
		}
	}
	n.ByID = l.linksTo(filepath.Join(l.DevRoot, "tape", "by-id"), targets)
	n.ByPath = l.linksTo(filepath.Join(l.DevRoot, "tape", "by-path"), targets)
	return n, nil
}

// childNames lists a sysfs class directory's entries; a missing directory
// (a drive has no scsi_changer, st not loaded) yields none.
func childNames(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// linksTo returns the links in dir whose target's base name is one of
// targets, sorted. udev writes them relative ("../../nst0").
func (l *Locator) linksTo(dir string, targets map[string]bool) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		dst, err := os.Readlink(p)
		if err != nil {
			continue
		}
		if targets[filepath.Base(dst)] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// NodeResolver resolves the device nodes of libraries' changers and drives.
type NodeResolver struct {
	Locator *Locator
	// Drive returns a drive's sg path: its --drive override, else discovery
	// by unit serial number (drivestats.Reader.Locate).
	Drive func(config.Drive) (string, error)
}

// LibraryNodes returns the nodes of lib's medium changer.
func (r NodeResolver) LibraryNodes(lib config.Library) (Nodes, error) {
	sg, err := r.Locator.Find(lib)
	if err != nil {
		return Nodes{}, err
	}
	return r.Locator.Nodes(sg)
}

// DriveNodes returns the nodes of tape drive d.
func (r NodeResolver) DriveNodes(d config.Drive) (Nodes, error) {
	if r.Drive == nil {
		return Nodes{}, fmt.Errorf("%w: drive %d: no drive locator", ErrUnavailable, d.ID)
	}
	sg, err := r.Drive(d)
	if err != nil {
		return Nodes{}, err
	}
	return r.Locator.Nodes(sg)
}
