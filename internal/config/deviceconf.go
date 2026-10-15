package config

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseDeviceConf parses a device.conf stream into libraries and drives.
func ParseDeviceConf(r io.Reader) (libraries []Library, drives []Drive, err error) {
	sc := bufio.NewScanner(r)
	var (
		lib    *Library
		drv    *Drive
		lineNo int
	)
	flush := func() {
		if lib != nil {
			libraries = append(libraries, *lib)
			lib = nil
		}
		if drv != nil {
			drives = append(drives, *drv)
			drv = nil
		}
	}
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), " \t\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			// Record header (or VERSION).
			if strings.HasPrefix(line, "VERSION:") {
				continue
			}
			flush()
			kind, id, ch, tg, lun, herr := parseHeader(line)
			if herr != nil {
				return nil, nil, fmt.Errorf("device.conf line %d: %w", lineNo, herr)
			}
			switch kind {
			case "Library":
				lib = &Library{ID: id, Channel: ch, Target: tg, LUN: lun}
			case "Drive":
				drv = &Drive{ID: id, Channel: ch, Target: tg, LUN: lun}
			}
			continue
		}
		if lib == nil && drv == nil {
			return nil, nil, fmt.Errorf("device.conf line %d: attribute outside of a record", lineNo)
		}
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			return nil, nil, fmt.Errorf("device.conf line %d: expected 'Key: value'", lineNo)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if aerr := applyAttr(lib, drv, key, val); aerr != nil {
			return nil, nil, fmt.Errorf("device.conf line %d: %w", lineNo, aerr)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	flush()
	return libraries, drives, nil
}

// parseHeader parses "Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00".
func parseHeader(line string) (kind string, id, ch, tg, lun int, err error) {
	f := strings.Fields(strings.ReplaceAll(line, ":", " "))
	if len(f) != 8 || (f[0] != "Library" && f[0] != "Drive") ||
		f[2] != "CHANNEL" || f[4] != "TARGET" || f[6] != "LUN" {
		return "", 0, 0, 0, 0, fmt.Errorf("malformed record header %q", line)
	}
	nums := make([]int, 4)
	for i, s := range []string{f[1], f[3], f[5], f[7]} {
		n, e := strconv.Atoi(s)
		if e != nil {
			return "", 0, 0, 0, 0, fmt.Errorf("bad number %q in header %q", s, line)
		}
		nums[i] = n
	}
	return f[0], nums[0], nums[1], nums[2], nums[3], nil
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
