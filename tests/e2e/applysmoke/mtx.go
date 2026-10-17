package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mhvtl-control/internal/conffile"
)

// live is a library's placement as `mtx status` prints it: storage slots
// and MAP slots in changer order, numbered from 1 as library_contents
// numbers them; "" is an empty element.
type live struct {
	Slots, MAPs []string
	Drives      []drive
}

// drive is a data transfer element: its cartridge and the storage slot it
// came from (0 when mtx does not know, or the source is a MAP slot).
type drive struct {
	Barcode string
	Source  int
}

// The lines mtx prints for each element (mtx.c, Status()).
var (
	reChanger = regexp.MustCompile(`^\s*Storage Changer `)
	reDrive   = regexp.MustCompile(`^Data Transfer Element (\d+):(Empty|Full)(?: \((?:Storage Element (\d+)|Unknown Storage Element) Loaded\))?(?::VolumeTag ?= ?(\S*))?`)
	reStorage = regexp.MustCompile(`^\s*Storage Element (\d+)( IMPORT/EXPORT)?:(Empty|Full)\s*(?::VolumeTag ?= ?(\S*))?`)
)

// parseMtx reads `mtx -f <sg> status`. mtx numbers storage elements in the
// order the changer returns them, MAP slots among them; library_contents
// numbers slots and MAP slots apart, so each kind is counted on its own.
// A drive's source is mtx's storage element number, mapped to its slot.
func parseMtx(r io.Reader) (live, error) {
	var p live
	var changer bool
	slotOf := map[int]int{} // mtx storage element number -> slot number
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case reChanger.MatchString(l):
			changer = true
		case reDrive.MatchString(l):
			m := reDrive.FindStringSubmatch(l)
			d := drive{}
			if m[2] == "Full" {
				d.Barcode = m[4]
				d.Source, _ = strconv.Atoi(m[3])
			}
			p.Drives = append(p.Drives, d)
		case reStorage.MatchString(l):
			m := reStorage.FindStringSubmatch(l)
			b := ""
			if m[3] == "Full" {
				b = m[4]
			}
			if m[2] != "" {
				p.MAPs = append(p.MAPs, b)
				continue
			}
			p.Slots = append(p.Slots, b)
			n, _ := strconv.Atoi(m[1])
			slotOf[n] = len(p.Slots)
		}
	}
	if err := sc.Err(); err != nil {
		return p, err
	}
	if !changer {
		return p, errors.New("not mtx status output: no Storage Changer line")
	}
	for i, d := range p.Drives {
		p.Drives[i].Source = slotOf[d.Source]
	}
	return p, nil
}

func parseMtxFile(path string) (live, error) {
	f, err := os.Open(path)
	if err != nil {
		return live{}, err
	}
	defer f.Close()
	p, err := parseMtx(f)
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// setEntry sets kind n to barcode, adding the line when the file lacks it.
func setEntry(c *conffile.LibContents, kind string, n int, barcode string) error {
	err := c.SetBarcode(kind, n, barcode)
	if errors.Is(err, conffile.ErrNotFound) {
		return c.AddEntry(kind, n, barcode)
	}
	return err
}

func hasEntry(c *conffile.LibContents, kind string, n int) bool {
	for _, e := range c.Entries() {
		if e.Kind == kind && e.Number == n {
			return true
		}
	}
	return false
}

// contents prints the library_contents a client builds from the live view
// (plan, amendment "Phase 4 client contract for next"): the file given,
// with every Slot and MAP line set to what mtx shows, and empty Slot and
// Drive lines added up to -slots and -drives.
func contents(fs *flag.FlagSet, args []string, stdout io.Writer) error {
	from := fs.String("from", "", "the library_contents to start from")
	mtx := fs.String("mtx", "", "`mtx status` output of the library")
	slots := fs.Int("slots", 0, "storage slots the result has (0: as many as mtx shows)")
	drives := fs.Int("drives", 0, "Drive lines the result has at least")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(*from)
	if err != nil {
		return err
	}
	p, err := parseMtxFile(*mtx)
	if err != nil {
		return err
	}
	if *slots != 0 && *slots < len(p.Slots) {
		return fmt.Errorf("-slots %d is fewer than the %d slots mtx shows", *slots, len(p.Slots))
	}
	c := conffile.ParseLibContents(data)
	for i, b := range p.Slots {
		if err := setEntry(c, "Slot", i+1, b); err != nil {
			return err
		}
	}
	for i, b := range p.MAPs {
		if err := setEntry(c, "MAP", i+1, b); err != nil {
			return err
		}
	}
	for n := len(p.Slots) + 1; n <= *slots; n++ {
		if err := setEntry(c, "Slot", n, ""); err != nil {
			return err
		}
	}
	for n := 1; n <= *drives; n++ {
		if !hasEntry(c, "Drive", n) {
			if err := c.AddEntry("Drive", n, ""); err != nil {
				return err
			}
		}
	}
	_, err = stdout.Write(c.Render())
	return err
}

// element is one line of a placement listing.
type element struct {
	kind    string
	n       int
	barcode string
	extra   string
}

// placement prints the full elements of mtx status or a library_contents,
// one per line, "<Kind> <n> <barcode>", sorted by kind and number, so two
// listings can be diffed. A drive line says the slot its cartridge came
// from; library_contents has no drive cartridges.
func placement(fs *flag.FlagSet, args []string, stdout io.Writer) error {
	mtx := fs.String("mtx", "", "`mtx status` output")
	file := fs.String("contents", "", "a library_contents")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var els []element
	switch {
	case *mtx != "" && *file == "":
		p, err := parseMtxFile(*mtx)
		if err != nil {
			return err
		}
		for i, d := range p.Drives {
			if d.Barcode == "" {
				continue
			}
			e := element{kind: "Drive", n: i + 1, barcode: d.Barcode}
			if d.Source > 0 {
				e.extra = fmt.Sprintf(" from Slot %d", d.Source)
			}
			els = append(els, e)
		}
		for i, b := range p.MAPs {
			els = append(els, element{kind: "MAP", n: i + 1, barcode: b})
		}
		for i, b := range p.Slots {
			els = append(els, element{kind: "Slot", n: i + 1, barcode: b})
		}
	case *file != "" && *mtx == "":
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		for _, e := range conffile.ParseLibContents(data).Entries() {
			if e.Kind == "Slot" || e.Kind == "MAP" {
				els = append(els, element{kind: e.Kind, n: e.Number, barcode: e.Barcode})
			}
		}
	default:
		return errors.New("give one of -mtx and -contents")
	}
	sort.SliceStable(els, func(i, j int) bool {
		if els[i].kind != els[j].kind {
			return els[i].kind < els[j].kind
		}
		return els[i].n < els[j].n
	})
	var b strings.Builder
	for _, e := range els {
		if e.barcode != "" {
			fmt.Fprintf(&b, "%s %d %s%s\n", e.kind, e.n, e.barcode, e.extra)
		}
	}
	_, err := io.WriteString(stdout, b.String())
	return err
}
