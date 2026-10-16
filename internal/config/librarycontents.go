package config

import (
	"io"

	"mhvtl-control/internal/conffile"
)

// ParseLibraryContents parses a library_contents.<id> stream, as a typed
// projection of conffile's lossless model. Like ParseDeviceConf it does not
// fail on a Problem: a line conffile cannot read is skipped, and problems
// lists them all. err is set only when r cannot be read.
func ParseLibraryContents(r io.Reader) (drives []DriveLine, pickers []int, maps []MAPSlot, slots []Slot, problems []conffile.Problem, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	c := conffile.ParseLibContents(data)
	for _, e := range c.Entries() {
		switch e.Kind {
		case "Drive":
			// The value is the first word, as vtllibrary reads "Drive %d: %s".
			drives = append(drives, DriveLine{Number: e.Number, Serial: e.Barcode})
		case "Picker":
			pickers = append(pickers, e.Number)
		case "MAP":
			maps = append(maps, MAPSlot{Number: e.Number, Barcode: e.Barcode})
		case "Slot":
			slots = append(slots, Slot{Number: e.Number, Barcode: e.Barcode})
		}
	}
	return drives, pickers, maps, slots, c.Problems, nil
}
