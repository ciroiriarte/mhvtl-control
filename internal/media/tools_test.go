package media

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
	env  []string
}

func recorder(calls *[]call, out string, err error) Runner {
	return func(_ context.Context, name string, args, env []string) ([]byte, error) {
		*calls = append(*calls, call{name, args, env})
		return []byte(out), err
	}
}

func TestToolsCreate(t *testing.T) {
	var calls []call
	tl := Tools{Mktape: "/usr/bin/mktape", ConfigDir: "/etc/mhvtl", Run: recorder(&calls, "", nil)}
	if err := tl.Create(context.Background(), 10, "/var/lib/mhvtl", Spec{Barcode: "E01001L8", Type: "WORM", Density: "LTO8"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := calls[0]
	if c.name != "/usr/bin/mktape" ||
		strings.Join(c.args, " ") != "-l 10 -m E01001L8 -s 0 -t WORM -d LTO8 -H /var/lib/mhvtl" {
		t.Errorf("ran %s %q", c.name, c.args)
	}
	if strings.Join(c.env, " ") != "MHVTL_CONFIG_PATH=/etc/mhvtl MHVTL_HOME_PATH=/var/lib/mhvtl" {
		t.Errorf("env = %q", c.env)
	}
}

func TestToolsEdit(t *testing.T) {
	var calls []call
	tl := Tools{EditTape: "edit_tape", ConfigDir: "/etc/mhvtl", Run: recorder(&calls, "", nil)}
	wp, typ, dens, size := true, "data", "LTO7", uint64(0)
	if err := tl.Edit(context.Background(), 10, "", "E01001L8", Change{WriteProtect: &wp, Type: &typ, Density: &dens, SizeMB: &size}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := strings.Join(calls[0].args, " "); got != "-l 10 -m E01001L8 -w on -t data -d LTO7 -s 0" {
		t.Errorf("args = %q", got)
	}
	if got := strings.Join(calls[0].env, " "); got != "MHVTL_CONFIG_PATH=/etc/mhvtl" {
		t.Errorf("env = %q (no home: the tool takes it from device.conf)", got)
	}
	wp = false
	if err := tl.Edit(context.Background(), 10, "", "E01001L8", Change{WriteProtect: &wp}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := strings.Join(calls[1].args, " "); got != "-l 10 -m E01001L8 -w off" {
		t.Errorf("args = %q", got)
	}
	if !(Change{}).Empty() || (Change{WriteProtect: &wp}).Empty() {
		t.Error("Change.Empty is wrong")
	}
}

func TestToolsFailureCarriesOutput(t *testing.T) {
	var calls []call
	boom := errors.New("exit status 1")
	tl := Tools{Mktape: "mktape", Run: recorder(&calls, "error: 'LTOX' is not a density this build knows\n", boom)}
	err := tl.Create(context.Background(), 10, "", Spec{Barcode: "A1", Type: "data", Density: "LTOX"})
	var te *ToolError
	if !errors.As(err, &te) || !errors.Is(err, boom) || te.Output != "error: 'LTOX' is not a density this build knows" {
		t.Fatalf("err = %#v", err)
	}
}

func TestValidators(t *testing.T) {
	if !ValidDensity("LTO10P") || ValidDensity("lto8") || !ValidType("WORM") || ValidType("worm") {
		t.Error("density/type validation")
	}
	if !ValidBarcode("A00001L8") || ValidBarcode("A0000 1") || ValidBarcode("ABCDEFGHIJKLMNOPQ") {
		t.Error("barcode validation")
	}
}
