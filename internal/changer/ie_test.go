package changer

import (
	"bytes"
	"errors"
	"testing"

	"mhvtld/internal/scsi"
)

func TestInventoryMAPState(t *testing.T) {
	s := newSim(ibmMap)
	m, _ := managerFor(s)
	inv, err := m.Inventory(testLib())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if inv.MAPOpen == nil || *inv.MAPOpen {
		t.Fatalf("closed MAP: MAPOpen = %v, want false", inv.MAPOpen)
	}
	s.mapOpen = true
	if inv, err = m.Inventory(testLib()); err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if inv.MAPOpen == nil || !*inv.MAPOpen {
		t.Fatalf("open MAP: MAPOpen = %v, want true", inv.MAPOpen)
	}

	noMAP := ibmMap
	noMAP.IE = scsi.ElementRange{}
	m, _ = managerFor(newSim(noMAP))
	if inv, err = m.Inventory(testLib()); err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if inv.MAPOpen != nil {
		t.Fatalf("library without a MAP: MAPOpen = %v, want nil", *inv.MAPOpen)
	}
}

func TestInventoryLocate(t *testing.T) {
	inv := Inventory{
		Drives: []DriveState{{Number: 1, Full: true, Barcode: "D1"}},
		Slots:  []SlotState{{Number: 1}, {Number: 2, Full: true, Barcode: "S2"}},
		MAPs:   []SlotState{{Number: 1, Full: true, Barcode: "M1"}},
	}
	for bc, want := range map[string]Location{"D1": {"drive", 1}, "S2": {"slot", 2}, "M1": {"map", 1}} {
		if got, ok := inv.Locate(bc); !ok || got != want {
			t.Errorf("Locate(%s) = %v, %v; want %v", bc, got, ok, want)
		}
	}
	if _, ok := inv.Locate("NOPE"); ok {
		t.Error("Locate found a barcode that is not there")
	}
	if _, ok := inv.Locate(""); ok {
		t.Error("Locate matched an empty barcode")
	}
}

func TestTransfer(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0401] = "A00001L8"
	m, dev := managerFor(s)

	mv, err := m.Transfer(testLib(), Location{"slot", 1}, Location{"slot", 7})
	if err != nil {
		t.Fatalf("slot -> slot: %v", err)
	}
	if mv.From != (Location{"slot", 1}) || mv.To != (Location{"slot", 7}) {
		t.Errorf("move = %+v", mv)
	}
	if _, err := m.Transfer(testLib(), Location{"slot", 7}, Location{"map", 3}); err != nil {
		t.Fatalf("slot -> map: %v", err)
	}
	want := [][]byte{scsi.MoveMediumCDB(1, 0x0401, 0x0407), scsi.MoveMediumCDB(1, 0x0407, 0x0303)}
	got := moves(dev)
	if len(got) != 2 || !bytes.Equal(got[0], want[0]) || !bytes.Equal(got[1], want[1]) {
		t.Fatalf("MOVE MEDIUM = % x, want % x", got, want)
	}

	s.mapOpen = true
	if _, err := m.Transfer(testLib(), Location{"map", 3}, Location{"slot", 1}); !errors.Is(err, ErrMAPOpen) {
		t.Errorf("open MAP: err = %v, want ErrMAPOpen", err)
	}
	if _, err := m.Transfer(testLib(), Location{"slot", 1}, Location{"drive", 1}); !errors.Is(err, ErrBadLocation) {
		t.Errorf("to a drive: err = %v, want ErrBadLocation", err)
	}
	if _, err := m.Transfer(testLib(), Location{"slot", 21}, Location{"slot", 1}); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("slot 21 of 20: err = %v, want ErrOutOfRange", err)
	}
	if len(moves(dev)) != 2 {
		t.Error("a refused transfer reached the changer")
	}
}

func TestExport(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0402] = "A00002L8"
	s.full[0x0403] = "A00003L8"
	s.full[0x0104] = "A00009L8" // drive id 13 sits at position 4
	s.full[0x0301] = "OLDMAP01"
	m, dev := managerFor(s)

	mv, err := m.Export(testLib(), ExportSource{Slot: 2})
	if err != nil {
		t.Fatalf("export slot: %v", err)
	}
	if mv.From != (Location{"slot", 2}) || mv.To != (Location{"map", 2}) || mv.Barcode != "A00002L8" {
		t.Errorf("slot export = %+v (want the first EMPTY MAP slot, 2)", mv)
	}
	if mv, err = m.Export(testLib(), ExportSource{Drive: 13}); err != nil {
		t.Fatalf("export drive: %v", err)
	}
	if mv.From != (Location{"drive", 4}) || mv.To != (Location{"map", 3}) || mv.Barcode != "A00009L8" {
		t.Errorf("drive export = %+v", mv)
	}
	if mv, err = m.Export(testLib(), ExportSource{Barcode: "A00003L8"}); err != nil {
		t.Fatalf("export barcode: %v", err)
	}
	if mv.From != (Location{"slot", 3}) || mv.To != (Location{"map", 4}) {
		t.Errorf("barcode export = %+v", mv)
	}
	want := [][]byte{
		scsi.MoveMediumCDB(1, 0x0402, 0x0302),
		scsi.MoveMediumCDB(1, 0x0104, 0x0303),
		scsi.MoveMediumCDB(1, 0x0403, 0x0304),
	}
	got := moves(dev)
	for i := range want {
		if i >= len(got) || !bytes.Equal(got[i], want[i]) {
			t.Fatalf("MOVE MEDIUM = % x, want % x", got, want)
		}
	}

	s.full[0x0405] = "A00005L8"
	if _, err := m.Export(testLib(), ExportSource{Slot: 5}); !errors.Is(err, ErrMAPFull) {
		t.Errorf("full MAP: err = %v, want ErrMAPFull", err)
	}
	if _, err := m.Export(testLib(), ExportSource{Barcode: "OLDMAP01"}); !errors.Is(err, ErrBadLocation) {
		t.Errorf("already in the MAP: err = %v, want ErrBadLocation", err)
	}
	if _, err := m.Export(testLib(), ExportSource{Barcode: "MISSING1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown barcode: err = %v, want ErrNotFound", err)
	}
	if _, err := m.Export(testLib(), ExportSource{Drive: 99}); !errors.Is(err, ErrUnknownDrive) {
		t.Errorf("unknown drive: err = %v, want ErrUnknownDrive", err)
	}
	s.mapOpen = true
	if _, err := m.Export(testLib(), ExportSource{Slot: 5}); !errors.Is(err, ErrMAPOpen) {
		t.Errorf("open MAP: err = %v, want ErrMAPOpen", err)
	}
	if len(moves(dev)) != 3 {
		t.Error("a refused export reached the changer")
	}
}

func TestStore(t *testing.T) {
	s := newSim(ibmMap)
	s.full[0x0401] = "A00001L8"
	s.full[0x0303] = "IMPORT01"
	m, dev := managerFor(s)

	mv, err := m.Store(testLib(), "IMPORT01")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if mv.From != (Location{"map", 3}) || mv.To != (Location{"slot", 2}) || mv.Barcode != "IMPORT01" {
		t.Errorf("store = %+v", mv)
	}
	if got := moves(dev); len(got) != 1 || !bytes.Equal(got[0], scsi.MoveMediumCDB(1, 0x0303, 0x0402)) {
		t.Fatalf("MOVE MEDIUM = % x", got)
	}
	if _, err := m.Store(testLib(), "IMPORT01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("not in the MAP any more: err = %v, want ErrNotFound", err)
	}

	full := newSim(scsi.ElementMap{
		Transport: ibmMap.Transport, DTE: ibmMap.DTE, IE: ibmMap.IE,
		Storage: scsi.ElementRange{First: 0x0401, Count: 1},
	})
	full.full[0x0401] = "A00001L8"
	full.full[0x0301] = "IMPORT02"
	m, _ = managerFor(full)
	if _, err := m.Store(testLib(), "IMPORT02"); !errors.Is(err, ErrNoEmptySlot) {
		t.Errorf("no empty slot: err = %v, want ErrNoEmptySlot", err)
	}
}

func TestExportWithoutMAP(t *testing.T) {
	noMAP := ibmMap
	noMAP.IE = scsi.ElementRange{}
	s := newSim(noMAP)
	s.full[0x0401] = "A00001L8"
	m, dev := managerFor(s)
	if _, err := m.Export(testLib(), ExportSource{Slot: 1}); !errors.Is(err, ErrNoMAP) {
		t.Fatalf("err = %v, want ErrNoMAP", err)
	}
	if len(moves(dev)) != 0 {
		t.Error("export reached the changer")
	}
}
