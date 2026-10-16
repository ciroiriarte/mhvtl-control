package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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
	return ReadInventory(configDir, func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(configDir, name))
	})
}

// ReadInventory is LoadInventory with the files' contents coming from read,
// which is given a file name (device.conf, library_contents.<id>) and
// reports a missing file with an error matching fs.ErrNotExist. Paths in
// errors and Problems are joined to configDir, as LoadInventory's are. It
// lets the configuration editor read a proposed set of files.
func ReadInventory(configDir string, read func(name string) ([]byte, error)) (Inventory, error) {
	path := filepath.Join(configDir, "device.conf")
	data, err := read("device.conf")
	if err != nil {
		return Inventory{}, &DeviceConfError{Path: path, Err: err}
	}
	libs, drives, problems, err := ParseDeviceConf(bytes.NewReader(data))
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
		name := "library_contents." + strconv.Itoa(lib.ID)
		cpath := filepath.Join(configDir, name)
		data, err := read(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Inventory{}, err
		}
		driveLines, pickers, maps, slots, problems, err := ParseLibraryContents(bytes.NewReader(data))
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
