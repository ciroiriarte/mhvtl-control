package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// DeviceConfError is returned when device.conf cannot be opened.
type DeviceConfError struct {
	Path string
	Err  error
}

func (e *DeviceConfError) Error() string {
	return fmt.Sprintf("cannot read device.conf %s: %v", e.Path, e.Err)
}

func (e *DeviceConfError) Unwrap() error { return e.Err }

// LoadInventory reads device.conf and library_contents.<id> from configDir.
func LoadInventory(configDir string) (Inventory, error) {
	path := filepath.Join(configDir, "device.conf")
	f, err := os.Open(path)
	if err != nil {
		return Inventory{}, &DeviceConfError{Path: path, Err: err}
	}
	libs, drives, problems, err := ParseDeviceConf(f)
	f.Close()
	if err != nil {
		return Inventory{}, fmt.Errorf("%s: %w", path, err)
	}
	var inv Inventory
	for _, p := range problems {
		inv.Problems = append(inv.Problems, Problem{File: path, Problem: p})
	}
	for _, d := range drives {
		attached := false
		for _, l := range libs {
			attached = attached || l.ID == d.LibraryID
		}
		if !attached {
			inv.Standalone = append(inv.Standalone, d)
		}
	}
	for i := range libs {
		lib := &libs[i]
		for _, d := range drives {
			if d.LibraryID == lib.ID {
				lib.Drives = append(lib.Drives, d)
			}
		}
		cpath := filepath.Join(configDir, "library_contents."+strconv.Itoa(lib.ID))
		cf, err := os.Open(cpath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Inventory{}, err
		}
		driveLines, pickers, maps, slots, problems, err := ParseLibraryContents(cf)
		cf.Close()
		if err != nil {
			return Inventory{}, fmt.Errorf("%s: %w", cpath, err)
		}
		lib.DriveLines, lib.MAPs, lib.Slots = driveLines, maps, slots
		for _, p := range pickers {
			lib.Pickers = append(lib.Pickers, p.Number)
			if p.Barcode != "" {
				lib.PickerMedia = append(lib.PickerMedia, p)
			}
		}
		for _, p := range problems {
			inv.Problems = append(inv.Problems, Problem{File: cpath, Problem: p})
		}
	}
	inv.Libraries = libs
	return inv, nil
}
