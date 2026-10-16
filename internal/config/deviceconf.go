package config

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"mhvtl-control/internal/conffile"
)

// ParseDeviceConf parses a device.conf stream into libraries and drives. It
// is a typed projection of conffile's lossless model, so the editor and the
// read path share one grammar.
//
// mhVTL starts with lines it ignores or reads in its own way, so a Problem
// does not fail the read: problems returns conffile's, plus each attribute
// the projection could not read, which it skips. A stanza whose header has
// no full address is kept, with the numbers sscanf read. err is set only
// when r cannot be read.
func ParseDeviceConf(r io.Reader) (libraries []Library, drives []Drive, problems []conffile.Problem, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, nil, err
	}
	c := conffile.ParseDeviceConf(data)
	problems = append(problems, c.Problems...)
	flagged := map[int]bool{}
	for _, p := range c.Problems {
		flagged[p.Line] = true
	}
	for _, st := range c.Stanzas() {
		var (
			lib *Library
			drv *Drive
		)
		switch st.Kind {
		case "Library":
			lib = &Library{ID: st.ID, Channel: st.Channel, Target: st.Target, LUN: st.LUN}
		case "Drive":
			drv = &Drive{ID: st.ID, Channel: st.Channel, Target: st.Target, LUN: st.LUN}
		}
		for _, a := range st.Attrs {
			// conf_value ignores a field with no value, and so do the sscanf
			// reads of NAA, Library ID and Home directory. An empty
			// Compression is not ignored (vtltape.c:2204): conffile flags it,
			// and there is no value to project.
			if a.Value == "" {
				continue
			}
			if aerr := applyAttr(lib, drv, a.Key, a.Value); aerr != nil && !flagged[a.Line+1] {
				problems = append(problems, conffile.Problem{Line: a.Line + 1, Msg: aerr.Error()})
			}
		}
		if lib != nil {
			libraries = append(libraries, *lib)
		} else {
			drives = append(drives, *drv)
		}
	}
	sort.SliceStable(problems, func(a, b int) bool { return problems[a].Line < problems[b].Line })
	return libraries, drives, problems, nil
}

func applyAttr(lib *Library, drv *Drive, key, val string) error {
	switch key {
	case "Vendor identification":
		if lib != nil {
			lib.Vendor = val
		} else {
			drv.Vendor = val
		}
	case "Product identification":
		if lib != nil {
			lib.Product = val
		} else {
			drv.Product = val
		}
	case "Unit serial number":
		if lib != nil {
			lib.Serial = val
		} else {
			drv.Serial = val
		}
	case "NAA":
		// sscanf needs all 8 bytes (vtltape.c:2217, vtllibrary.c:1342).
		// With fewer, mhVTL drops the NAA for its default; with none it
		// ignores the line.
		switch n := conffile.ScanNAA(val); {
		case n < 1:
			return nil
		case n < 8:
			val = ""
		}
		if lib != nil {
			lib.NAA = val
		} else {
			drv.NAA = val
		}
	case "Home directory":
		if lib != nil {
			lib.HomeDir = val
		}
	case "Product revision level":
		if drv != nil {
			drv.Revision = val
		}
	case "Library ID":
		// Form: "10 Slot: 01". vtllibrary puts a drive in a library only
		// with both numbers (vtllibrary.c:686); conffile flags a bare
		// "10", and it is not projected.
		if drv == nil {
			return nil
		}
		f := strings.FieldsFunc(strings.ReplaceAll(val, ":", " "), isCSpace)
		if len(f) != 1 && !(len(f) == 3 && f[1] == "Slot") {
			return fmt.Errorf("malformed Library ID %q", val)
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			return fmt.Errorf("bad Library ID %q", val)
		}
		if len(f) == 1 {
			return nil
		}
		s, err := strconv.Atoi(f[2])
		if err != nil {
			return fmt.Errorf("bad Slot %q", val)
		}
		drv.LibraryID, drv.Slot = id, s
	case "Slot":
		if drv != nil {
			s, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("bad Slot %q", val)
			}
			drv.Slot = s
		}
	case "Compression":
		if drv == nil {
			return nil
		}
		f := strings.FieldsFunc(val, isCSpace)
		// "Compression: N" sets the factor alone, 0 when N is out of 1-9
		// (vtltape.c:2208-2212).
		if len(f) == 1 {
			n, err := strconv.Atoi(f[0])
			if err != nil {
				return fmt.Errorf("malformed Compression %q", val)
			}
			if n < 1 || n > 9 {
				n = 0
			}
			drv.CompressionFactor = n
			return nil
		}
		if len(f) != 4 || f[0] != "factor" || f[2] != "enabled" {
			return fmt.Errorf("malformed Compression %q", val)
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			return fmt.Errorf("bad compression factor %q", f[1])
		}
		e, err := strconv.Atoi(f[3])
		if err != nil {
			return fmt.Errorf("bad compression enabled %q", f[3])
		}
		drv.CompressionFactor = n
		drv.CompressionEnabled = e != 0
	case "Compression type":
		if drv != nil {
			drv.CompressionType = val
		}
	}
	return nil
}

// isCSpace is C's isspace in the "C" locale, the white space sscanf's %d
// skips; unlike strings.Fields it does not split on Unicode spaces.
func isCSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r'
}
