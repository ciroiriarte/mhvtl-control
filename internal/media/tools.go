package media

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Densities are the density names mktape and edit_tape accept (mhvtl
// set_media_params in usr/vtllib.c).
var Densities = []string{
	"LTO1", "LTO2", "LTO3", "LTO4", "LTO5", "LTO6", "LTO7", "LTO8", "LTO8M8", "LTO9", "LTO10", "LTO10P",
	"AIT1", "AIT2", "AIT3", "AIT4",
	"DDS1", "DDS2", "DDS3", "DDS4",
	"DLT3", "DLT4", "SDLT1", "SDLT220", "SDLT320", "SDLT600",
	"T10KA", "T10KB", "T10KC",
	"9840A", "9840B", "9840C", "9840D", "9940A", "9940B",
	"J1A", "E05", "E06", "E07",
}

// Types are the cartridge types mktape and edit_tape accept.
var Types = []string{"data", "WORM", "clean", "NULL"}

// ValidDensity reports whether d is one of Densities.
func ValidDensity(d string) bool { return slices.Contains(Densities, d) }

// ValidType reports whether t is one of Types.
func ValidType(t string) bool { return slices.Contains(Types, t) }

// DataOnlyDensities exist only as data cartridges. LTO-8 Type M (M8) has no
// WORM or cleaning form (IBM LTO SCSI Reference GA32-0928-07, Table 5, p.21),
// and mktape and edit_tape refuse any other type with it (mhvtl
// usr/cmd/mktape.c:223-228, usr/cmd/edit_tape.c:262-269, upstream master
// ebbda22).
var DataOnlyDensities = []string{"LTO8M8"}

// DataOnly reports whether d is one of DataOnlyDensities.
func DataOnly(d string) bool { return slices.Contains(DataOnlyDensities, d) }

// ValidBarcode reports whether s is a barcode mhVTL can carry: 1-16 letters or
// digits.
func ValidBarcode(s string) bool { return validBarcode(s) }

// toolTimeout bounds one mktape or edit_tape run.
const toolTimeout = 2 * time.Minute

// Spec describes a cartridge to create. SizeMB zero asks for the native
// capacity of the density.
type Spec struct {
	Barcode string
	Type    string
	Density string
	SizeMB  uint64
}

// Change describes an edit of an existing cartridge; nil fields are left as
// they are. SizeMB zero asks for the native capacity.
type Change struct {
	WriteProtect *bool
	Type         *string
	Density      *string
	SizeMB       *uint64
}

// Empty reports whether c changes nothing.
func (c Change) Empty() bool {
	return c.WriteProtect == nil && c.Type == nil && c.Density == nil && c.SizeMB == nil
}

// Runner runs a tool with extra environment and returns its combined output.
type Runner func(ctx context.Context, name string, args, env []string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, name string, args, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// Tools creates and edits cartridges with mhVTL's mktape and edit_tape, which
// write the media files and MAM in mhVTL's own format.
type Tools struct {
	Mktape    string // mktape path
	EditTape  string // edit_tape path
	ConfigDir string // passed as MHVTL_CONFIG_PATH so the tools read our device.conf
	Run       Runner // nil means ExecRunner
}

// ToolError is a tool that failed, with its output.
type ToolError struct {
	Tool   string
	Err    error
	Output string
}

func (e *ToolError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("%s: %v", e.Tool, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Tool, e.Err, e.Output)
}

func (e *ToolError) Unwrap() error { return e.Err }

// Create makes the cartridge s in library lib, whose media live in home.
// mktape reports success when the cartridge already exists, so callers check
// Exists first.
func (t Tools) Create(ctx context.Context, lib int, home string, s Spec) error {
	args := []string{
		"-l", strconv.Itoa(lib),
		"-m", s.Barcode,
		"-s", strconv.FormatUint(s.SizeMB, 10),
		"-t", s.Type,
		"-d", s.Density,
	}
	if home != "" {
		args = append(args, "-H", home)
	}
	return t.run(ctx, t.Mktape, args, home)
}

// Edit applies c to the cartridge barcode of library lib. The cartridge must
// not be loaded in a drive: mhVTL has no lock on media, and vtltape rewrites
// the MAM when it unloads.
func (t Tools) Edit(ctx context.Context, lib int, home, barcode string, c Change) error {
	args := []string{"-l", strconv.Itoa(lib), "-m", barcode}
	if c.WriteProtect != nil {
		args = append(args, "-w", map[bool]string{true: "on", false: "off"}[*c.WriteProtect])
	}
	if c.Type != nil {
		args = append(args, "-t", *c.Type)
	}
	if c.Density != nil {
		args = append(args, "-d", *c.Density)
	}
	if c.SizeMB != nil {
		args = append(args, "-s", strconv.FormatUint(*c.SizeMB, 10))
	}
	return t.run(ctx, t.EditTape, args, home)
}

func (t Tools) run(ctx context.Context, tool string, args []string, home string) error {
	run := t.Run
	if run == nil {
		run = ExecRunner
	}
	env := []string{"MHVTL_CONFIG_PATH=" + t.ConfigDir}
	if home != "" {
		env = append(env, "MHVTL_HOME_PATH="+home)
	}
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := run(ctx, tool, args, env)
	if err != nil {
		return &ToolError{Tool: tool, Err: err, Output: strings.TrimSpace(string(bytes.ToValidUTF8(out, nil)))}
	}
	return nil
}
