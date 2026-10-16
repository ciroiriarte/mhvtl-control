package config

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"mhvtl-control/internal/conffile"
)

// ParseDeviceConf parses a device.conf stream into libraries and drives. It
// is a typed projection of conffile's lossless model, so the editor and the
// read path share one grammar.
func ParseDeviceConf(r io.Reader) (libraries []Library, drives []Drive, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	c := conffile.ParseDeviceConf(data)
	// An ambiguous line still loads in mhVTL, so the read path goes on; the
	// editor refuses to write over it.
	for _, p := range c.Problems {
		if !p.Ambiguous {
			return nil, nil, fmt.Errorf("device.conf %w", p)
		}
	}
	for _, st := range c.Stanzas() {
		if !st.HasAddress {
			return nil, nil, fmt.Errorf("device.conf line %d: malformed record header %q",
				st.HeaderLine+1, strings.TrimRight(c.Lines()[st.HeaderLine].Text, " \t\r"))
		}
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
			if aerr := applyAttr(lib, drv, a.Key, a.Value); aerr != nil {
				return nil, nil, fmt.Errorf("device.conf line %d: %w", a.Line+1, aerr)
			}
		}
		if lib != nil {
			libraries = append(libraries, *lib)
		} else {
			drives = append(drives, *drv)
		}
	}
	return libraries, drives, nil
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
		// Form: "10 Slot: 01"
		if drv == nil {
			return nil
		}
		f := strings.Fields(strings.ReplaceAll(val, ":", " "))
		if len(f) != 1 && !(len(f) == 3 && f[1] == "Slot") {
			return fmt.Errorf("malformed Library ID %q", val)
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			return fmt.Errorf("bad Library ID %q", val)
		}
		drv.LibraryID = id
		if len(f) == 3 {
			s, err := strconv.Atoi(f[2])
			if err != nil {
				return fmt.Errorf("bad Slot %q", val)
			}
			drv.Slot = s
		}
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
		f := strings.Fields(val)
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
