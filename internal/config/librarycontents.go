package config

import (
	"fmt"
	"io"

	"mhvtl-control/internal/conffile"
)

// ParseLibraryContents parses a library_contents.<id> stream, as a typed
// projection of conffile's lossless model.
func ParseLibraryContents(r io.Reader) (drives []int, pickers []int, maps []MAPSlot, slots []Slot, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	c := conffile.ParseLibContents(data)
	// Ambiguous lines are read as mhVTL reads them; the editor refuses to
	// write over them.
	for _, p := range c.Problems {
		if !p.Ambiguous {
			return nil, nil, nil, nil, fmt.Errorf("library_contents %w", p)
		}
	}
	for _, e := range c.Entries() {
		switch e.Kind {
		case "Drive":
			drives = append(drives, e.Number)
		case "Picker":
			pickers = append(pickers, e.Number)
		case "MAP":
			maps = append(maps, MAPSlot{Number: e.Number, Barcode: e.Barcode})
		case "Slot":
			slots = append(slots, Slot{Number: e.Number, Barcode: e.Barcode})
		}
	}
	return drives, pickers, maps, slots, nil
}
