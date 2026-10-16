package validate

import (
	"testing"

	"mhvtl-control/internal/config"
)

func TestPresets(t *testing.T) {
	const l700Why = "the STK L-series (L180/L700) personality (init_stklxx, selected at usr/cmd/vtllibrary.c:1497) has no such page; use a model that has it, such as IBM 03584 or Quantum Scalar"
	for _, tc := range []struct {
		name          string
		lib           config.Library
		app           string
		errors, warns []string
	}{
		{"PBS on an IBM 03584", lib(10, "IBM", "03584L32", 1), PresetPBS, nil, nil},
		{"PBS on a Quantum Scalar", lib(10, "QUANTUM", "Scalar i6000", 1), PresetPBS, nil, nil},
		{"PBS on an STK L700", lib(10, "STK", "L700", 1), PresetPBS,
			[]string{"library 10 is set up for Proxmox Backup Server, but it finds changers by udev's ID_SCSI_SERIAL, which comes from VPD page 83h, and " + l700Why}, nil},
		{"no target: an L700 warns", lib(10, "STK", "L700", 1), "", nil,
			[]string{"library 10 would not work with Proxmox Backup Server: it finds changers by udev's ID_SCSI_SERIAL"}},
		{"no target: an 03584 is quiet", lib(10, "IBM", "03584L32", 1), "", nil, nil},
		{"unknown target", lib(10, "IBM", "03584L32", 1), "veeam", []string{`library 10: unknown target application "veeam"`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Validate(config.Inventory{}, inv(tc.lib), nil, nil, Options{TargetApps: map[int]string{10: tc.app}})
			expect(t, "errors", family(r.Errors, FamilyPreset), tc.errors)
			expect(t, "warnings", family(r.Warnings, FamilyPreset), tc.warns)
		})
	}
}
