package validate

import (
	"testing"

	"mhvtl-control/internal/config"
)

// withCounts gives l n drives (drive slots 1..n), storage slots, MAP slots
// and pickers.
func withCounts(l config.Library, drives, slots, maps, pickers int) config.Library {
	l.Drives, l.Slots, l.MAPs, l.Pickers = nil, nil, nil, nil
	for i := 1; i <= drives; i++ {
		l.Drives = append(l.Drives, config.Drive{ID: 100000 + i, LibraryID: l.ID, Slot: i})
	}
	for i := 1; i <= slots; i++ {
		l.Slots = append(l.Slots, config.Slot{Number: i})
	}
	for i := 1; i <= maps; i++ {
		l.MAPs = append(l.MAPs, config.MAPSlot{Number: i})
	}
	for i := 1; i <= pickers; i++ {
		l.Pickers = append(l.Pickers, i)
	}
	return l
}

func TestAddresses(t *testing.T) {
	ibm := lib(10, "IBM", "03584L32", 0)
	stk := lib(10, "STK", "L700", 0)
	overland := lib(10, "ACME", "OVERLAND NEO", 0)
	for _, tc := range []struct {
		name string
		lib  config.Library
		want []string
	}{
		// IBM 03584: drives 0x0101, MAP 0x0300, storage 0x0400.
		{"03584 at every limit", withCounts(ibm, 511, 0x10000-0x400, 256, 1), nil},
		{"03584 drive 512", withCounts(ibm, 512, 1, 1, 1),
			[]string{"drive 512 would sit at element address 0x0300, which overlaps the MAP slot addresses from 0x0300; the limit is 511 drives (drive addresses from 0x0101, usr/pm/ibm_smc_pm.c:432)"}},
		{"03584 MAP 257", withCounts(ibm, 1, 1, 257, 1),
			[]string{"MAP slot 257 would sit at element address 0x0400, which overlaps the storage slot addresses from 0x0400; the limit is 256 MAP slots"}},
		{"03584 storage past 0xffff", withCounts(ibm, 1, 0x10000-0x400+1, 1, 1),
			[]string{"storage slot 64513 would sit at element address 0x10000, beyond the 16-bit element address space; the limit is 64512 storage slots"}},
		{"03584 picker 257 runs into the drives", withCounts(ibm, 1, 1, 1, 257),
			[]string{"picker 257 would sit at element address 0x0101, which overlaps the drive addresses from 0x0101; the limit is 256 pickers"}},
		// STK L700: MAP 0x000a, drives 0x01f4, storage 0x03e8.
		{"L700 at the drive limit", withCounts(stk, 500, 39, 4, 1), nil},
		{"L700 drive 501", withCounts(stk, 501, 39, 4, 1),
			[]string{"library 10 (STK L-series (L180/L700)): drive 501 would sit at element address 0x03e8, which overlaps the storage slot addresses from 0x03e8; the limit is 500 drives (drive addresses from 0x01f4, usr/pm/stklxx_pm.c:109)"}},
		// Overland puts the MAP at 0 and the picker at 1: one MAP slot only.
		{"Overland one MAP slot", withCounts(overland, 1, 1, 1, 1), nil},
		{"Overland two MAP slots", withCounts(overland, 1, 1, 2, 1),
			[]string{"MAP slot 2 would sit at element address 0x0001, which overlaps the picker addresses from 0x0001; the limit is 1 MAP slots"}},
		{"duplicate and zero numbers", func() config.Library {
			l := withCounts(stk, 2, 3, 0, 1)
			l.Drives[1].Slot = 1
			l.Slots[2].Number = 0
			return l
		}(), []string{"drive 1 is configured twice", "storage slot 0 is invalid; numbering starts at 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(config.Inventory{}, inv(tc.lib), nil, nil, Options{})
			expect(t, tc.name, family(r.Errors, FamilyAddress), tc.want)
		})
	}
}
