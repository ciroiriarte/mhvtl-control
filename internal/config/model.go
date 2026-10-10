// Package config parses mhvtl configuration files (device.conf and
// library_contents.<id>).
package config

// Library is a virtual medium changer defined in device.conf.
type Library struct {
	ID                      int
	Channel, Target, LUN    int
	Vendor, Product, Serial string
	NAA, HomeDir            string
	Drives                  []Drive
	Slots                   []Slot
	MAPs                    []MAPSlot
	Pickers                 []int
}

// Drive is a virtual tape drive defined in device.conf.
type Drive struct {
	ID                 int
	LibraryID          int
	Slot               int
	Vendor, Product    string
	Revision, Serial   string
	NAA                string
	CompressionFactor  int
	CompressionEnabled bool
	CompressionType    string
}

// Slot is a storage slot. An empty Barcode means the slot is empty.
type Slot struct {
	Number  int
	Barcode string
}

// MAPSlot is a media access port (mail slot).
type MAPSlot struct {
	Number  int
	Barcode string
}

// Inventory is the full parsed configuration.
type Inventory struct {
	Libraries []Library
}
