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
	libs, drives, err := ParseDeviceConf(f)
	f.Close()
	if err != nil {
		return Inventory{}, fmt.Errorf("%s: %w", path, err)
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
		_, pickers, maps, slots, err := ParseLibraryContents(cf)
		cf.Close()
		if err != nil {
			return Inventory{}, fmt.Errorf("%s: %w", cpath, err)
		}
		lib.Pickers, lib.MAPs, lib.Slots = pickers, maps, slots
	}
	return Inventory{Libraries: libs}, nil
}
