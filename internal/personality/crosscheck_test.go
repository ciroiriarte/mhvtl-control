package personality

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The cross-checks below read mhVTL's C sources and fail on any difference
// in value or dispatch order from the Go tables, naming the C line. CI runs
// them against upstream master ($MHVTL_SRC), so a drift there fails the
// build. A cited line that merely moved is logged as citation drift and only
// fails once it is more than maxCitationDrift lines off
// (TestCitationsWithinReach), so a citation cannot end up pointing at a
// different function.

// mhvtlRepo locates the mhVTL source: $MHVTL_SRC, else the sibling submodule
// of the superproject.
func mhvtlRepo(t *testing.T) string {
	t.Helper()
	src := os.Getenv("MHVTL_SRC")
	if src == "" {
		src = "../../../mhvtl"
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(abs, "usr", "cmd", "vtllibrary.c")); err != nil {
		t.Skipf("mhvtl submodule not found at %s: %v", abs, err)
	}
	return abs
}

// cfile is a C source split into lines; line n is lines[n-1].
type cfile struct {
	rel   string
	lines []string
}

func readC(t *testing.T, root, rel string) cfile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return cfile{rel: rel, lines: strings.Split(string(b), "\n")}
}

func (f cfile) at(n int) string { return fmt.Sprintf("%s:%d", f.rel, n) }

// maxCitationDrift is how far a cited line may be from where the C source has
// it now before the citation counts as wrong.
const maxCitationDrift = 50

// splitCitation parses "file:line".
func splitCitation(c string) (string, int) {
	i := strings.LastIndexByte(c, ':')
	if i < 0 {
		return c, -1
	}
	n, err := strconv.Atoi(c[i+1:])
	if err != nil {
		return c, -1
	}
	return c[:i], n
}

// logDrift logs a citation that no longer names the line the C source has.
func logDrift(t *testing.T, cited, found string) {
	t.Helper()
	if cited != found {
		t.Logf("citation drift: table says %s, found at %s", cited, found)
	}
}

// body returns the 1-based line range of the function named fn: the line
// after its definition through the line before the closing brace in column 1.
// ok is false when fn is not defined in f.
func (f cfile) body(fn string) (first, last int, ok bool) {
	def := regexp.MustCompile(`^(static )?void ` + regexp.QuoteMeta(fn) + `\(struct lu_phy_attr \*lu\) \{`)
	for i, l := range f.lines {
		if !def.MatchString(l) {
			continue
		}
		for j := i + 1; j < len(f.lines); j++ {
			if strings.HasPrefix(f.lines[j], "}") {
				return i + 2, j, true
			}
		}
	}
	return 0, 0, false
}

// pmFiles returns every usr/pm/*_pm.c.
func pmFiles(t *testing.T, root string) []cfile {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "usr", "pm", "*_pm.c"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no usr/pm/*_pm.c under %s: %v", root, err)
	}
	sort.Strings(paths)
	files := make([]cfile, 0, len(paths))
	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)
		files = append(files, readC(t, root, rel))
	}
	return files
}

// findInit returns the file defining fn and the range of its body.
func findInit(t *testing.T, files []cfile, fn string) (cfile, int, int) {
	t.Helper()
	for _, f := range files {
		if first, last, ok := f.body(fn); ok {
			return f, first, last
		}
	}
	t.Fatalf("%s is not defined in any usr/pm/*_pm.c", fn)
	return cfile{}, 0, 0
}

var (
	termRe   = regexp.MustCompile(`strncasecmp\(lu->(vendor|product)_id,\s*"([^"]*)",\s*(\d+)\)`)
	actionRe = regexp.MustCompile(`\b((?:init|customise)_\w+)\(lu\);`)
)

type term struct {
	field, prefix string
	line          int
}

// dispatch flattens the if/else-if chain of fn into rules, following
// customise_*_lu helpers. outer holds the terms that led into fn.
func dispatch(t *testing.T, f cfile, fn string, outer []term) []Rule {
	t.Helper()
	first, last, ok := f.body(fn)
	if !ok {
		t.Fatalf("%s: %s not found", f.rel, fn)
	}
	var rules []Rule
	var pending []term
	for n := first; n <= last; n++ {
		l := f.lines[n-1]
		for _, m := range termRe.FindAllStringSubmatch(l, -1) {
			k, _ := strconv.Atoi(m[3])
			p := m[2]
			if k < len(p) {
				p = p[:k] // strncasecmp compares k characters only
			}
			pending = append(pending, term{field: m[1], prefix: p, line: n})
		}
		m := actionRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		alts := pending
		if len(alts) == 0 {
			alts = []term{{line: n}} // else branch
		}
		for _, tm := range alts {
			terms := append(append([]term(nil), outer...), tm)
			if strings.HasPrefix(m[1], "customise_") {
				rules = append(rules, dispatch(t, f, m[1], terms)...)
				continue
			}
			r := Rule{InitFn: m[1], Source: f.at(tm.line)}
			for _, x := range terms {
				switch x.field {
				case "vendor":
					r.Vendor = x.prefix
				case "product":
					r.ProductPrefix = x.prefix
				}
			}
			rules = append(rules, r)
		}
		pending = nil
	}
	return rules
}

func TestCrossCheckLibraryDispatch(t *testing.T) {
	root := mhvtlRepo(t)
	f := readC(t, root, "usr/cmd/vtllibrary.c")
	got := dispatch(t, f, "customise_lu", nil)
	for i := 0; i < max(len(got), len(Rules)); i++ {
		switch {
		case i >= len(got):
			t.Errorf("Rules[%d] %+v has no counterpart in customise_lu", i, Rules[i])
		case i >= len(Rules):
			t.Errorf("%s: dispatch rule %+v is missing from Rules", got[i].Source, got[i])
		default:
			g, w := got[i], Rules[i]
			g.Source, w.Source = "", ""
			if g != w {
				t.Errorf("%s: dispatch rule %+v; Rules[%d] = %+v", got[i].Source, got[i], i, Rules[i])
				continue
			}
			logDrift(t, Rules[i].Source, got[i].Source)
		}
	}
}

var (
	assignRe = regexp.MustCompile(`smc_pm\.(\w+)\s*=\s*([^;]+);`)
	initRe   = regexp.MustCompile(`^\s*\.(\w+)\s*=\s*([^,]+),`)
	callRe   = regexp.MustCompile(`\b(\w+)\(lu\)`)
	allocRe  = regexp.MustCompile(`\balloc_vpd\(`) // not dealloc_vpd
)

type cvalue struct {
	v    int64
	line int
}

func parseCValue(s string) (int64, error) {
	switch s = strings.TrimSpace(s); s {
	case "TRUE":
		return 1, nil
	case "FALSE":
		return 0, nil
	}
	return strconv.ParseInt(s, 0, 64)
}

// smcFields returns each smc_pm field the personality ends up with: the last
// assignment in its init function, else the file's static initializer.
func smcFields(t *testing.T, f cfile, first, last int) map[string]cvalue {
	t.Helper()
	vals := map[string]cvalue{}
	in := false
	for n, l := range f.lines {
		switch {
		case strings.HasPrefix(l, "static struct smc_personality_template smc_pm = {"):
			in = true
		case in && strings.HasPrefix(l, "};"):
			in = false
		case in:
			if m := initRe.FindStringSubmatch(l); m != nil {
				if v, err := parseCValue(m[2]); err == nil {
					vals[m[1]] = cvalue{v, n + 1}
				}
			}
		}
	}
	for n := first; n <= last; n++ {
		if m := assignRe.FindStringSubmatch(f.lines[n-1]); m != nil {
			if v, err := parseCValue(m[2]); err == nil {
				vals[m[1]] = cvalue{v, n}
			}
		}
	}
	return vals
}

// windowSource is the line of the first start_* value a personality uses.
func windowSource(f cfile, vals map[string]cvalue) string {
	ws := 0
	for _, k := range []string{"start_picker", "start_map", "start_drive", "start_storage"} {
		if v, ok := vals[k]; ok && (ws == 0 || v.line < ws) {
			ws = v.line
		}
	}
	return f.at(ws)
}

// hasVPD83 reports whether the init function, or a function it calls in the
// same file, allocates VPD page 83h.
func hasVPD83(f cfile, first, last int) bool {
	allocs := func(a, b int) bool {
		text := strings.Join(f.lines[a-1:b], "\n")
		return strings.Contains(text, "PCODE_OFFSET(0x83)") && allocRe.MatchString(text)
	}
	if allocs(first, last) {
		return true
	}
	for n := first; n <= last; n++ {
		for _, m := range callRe.FindAllStringSubmatch(f.lines[n-1], -1) {
			if a, b, ok := f.body(m[1]); ok && allocs(a, b) {
				return true
			}
		}
	}
	return false
}

func TestCrossCheckLibraryModels(t *testing.T) {
	root := mhvtlRepo(t)
	files := pmFiles(t, root)
	used := map[string]bool{}
	for _, r := range Rules {
		used[r.InitFn] = true
	}
	for fn := range Models {
		if !used[fn] {
			t.Errorf("Models[%q] is not selected by any rule", fn)
		}
	}
	for fn := range used {
		m, ok := Models[fn]
		if !ok {
			t.Errorf("Models has no entry for %s", fn)
			continue
		}
		f, first, last := findInit(t, files, fn)
		vals := smcFields(t, f, first, last)
		check := func(field string, want int64) {
			got := vals[field] // absent: zero, as in a static struct
			if got.v != want {
				where := f.at(first - 1)
				if got.line != 0 {
					where = f.at(got.line)
				}
				t.Errorf("%s: %s %s = %d; Models[%q] has %d", where, fn, field, got.v, fn, want)
			}
		}
		b := func(x bool) int64 {
			if x {
				return 1
			}
			return 0
		}
		check("start_picker", int64(m.StartPicker))
		check("start_map", int64(m.StartMAP))
		check("start_drive", int64(m.StartDrive))
		check("start_storage", int64(m.StartStorage))
		check("dvcid_len", int64(m.DvcidLen))
		check("dvcid_serial_only", b(m.DvcidSerialOnly))
		check("dvcid_nul_terminated", b(m.DvcidNulTerminated))
		check("stk_dte_layout", b(m.StkDteLayout))

		logDrift(t, m.WindowSource, windowSource(f, vals))
		if got := hasVPD83(f, first, last); got != m.VPD83 {
			t.Errorf("%s: %s allocates VPD 83h: %v; Models[%q].VPD83 = %v", f.at(first-1), fn, got, fn, m.VPD83)
		}
	}
}

var (
	driveRowRe   = regexp.MustCompile(`\{"([^"]+)",\s*(init_\w+)\}`)
	driveInitRe  = regexp.MustCompile(`drive_init\)\(struct lu_phy_attr \*\) = (\w+);`)
	mediaCallRe  = regexp.MustCompile(`add_drive_media_list\(lu, LOAD_(RW|RO), "([^"]+)"\)`)
	arraySizeRe  = regexp.MustCompile(`ARRAY_SIZE\((\w+)\)`)
	genMediaRe   = regexp.MustCompile(`\{&\w+,\s*([01]),\s*"([^"]+)",\s*([01])\}`)
	genArrayDefR = regexp.MustCompile(`^static const struct ult_gen_media (\w+)\[\] = \{`)
)

// driveProducts parses tape_drives[] and the default drive_init of vtltape.c.
func driveProducts(t *testing.T, root string) (rows []DriveProduct, def string) {
	t.Helper()
	f := readC(t, root, "usr/cmd/vtltape.c")
	for n, l := range f.lines {
		if m := driveRowRe.FindStringSubmatch(l); m != nil {
			rows = append(rows, DriveProduct{Product: strings.TrimRight(m[1], " "), InitFn: m[2], Source: f.at(n + 1)})
		}
		if m := driveInitRe.FindStringSubmatch(l); m != nil {
			def = m[1]
		}
	}
	return rows, def
}

func TestCrossCheckDriveProducts(t *testing.T) {
	root := mhvtlRepo(t)
	got, def := driveProducts(t, root)
	if def != DefaultDriveInit {
		t.Errorf("vtltape.c drive_init defaults to %q; DefaultDriveInit = %q", def, DefaultDriveInit)
	}
	for i := 0; i < max(len(got), len(DriveProducts)); i++ {
		switch {
		case i >= len(got):
			t.Errorf("DriveProducts[%d] %+v is not in tape_drives", i, DriveProducts[i])
		case i >= len(DriveProducts):
			t.Errorf("%s: tape_drives row %+v is missing from DriveProducts", got[i].Source, got[i])
		default:
			g, w := got[i], DriveProducts[i]
			g.Source, w.Source = "", ""
			if g != w {
				t.Errorf("%s: tape_drives row %+v; DriveProducts[%d] = %+v", got[i].Source, got[i], i, DriveProducts[i])
				continue
			}
			logDrift(t, DriveProducts[i].Source, got[i].Source)
		}
	}
}

// cmedia is an add_drive_media_list() call and the line it comes from.
type cmedia struct {
	Media
	line int
}

// genArrays returns the ult_gen_media tables of f by name, each entry
// expanded the way init_ult3580_lbp adds it.
func genArrays(f cfile) map[string][]cmedia {
	arrays := map[string][]cmedia{}
	name := ""
	for n, l := range f.lines {
		if m := genArrayDefR.FindStringSubmatch(l); m != nil {
			name = m[1]
			continue
		}
		if name == "" {
			continue
		}
		if strings.HasPrefix(l, "};") {
			name = ""
			continue
		}
		if m := genMediaRe.FindStringSubmatch(l); m != nil {
			e := []cmedia{{Media{m[2], Load(m[1] == "1")}, n + 1}}
			if m[3] == "1" {
				e = append(e,
					cmedia{Media{m[2] + " Clean", RO}, n + 1},
					cmedia{Media{m[2] + " WORM", RW}, n + 1},
					cmedia{Media{m[2] + " ENCR", RW}, n + 1})
			}
			arrays[name] = append(arrays[name], e...)
		}
	}
	return arrays
}

// checkLBPHelper fails if init_ult3580_lbp no longer adds the media of its
// table the way genArrays expands them.
func checkLBPHelper(t *testing.T, f cfile) {
	t.Helper()
	first, last, ok := f.body("init_ult3580_lbp")
	if !ok {
		first, last, ok = bodyAnyProto(f, "init_ult3580_lbp")
	}
	if !ok {
		t.Fatalf("%s: init_ult3580_lbp not found", f.rel)
	}
	want := []struct{ snippet, load string }{
		{"media[i].writable ? LOAD_RW : LOAD_RO", ""},
		{`"%s Clean"`, "LOAD_RO"},
		{`"%s WORM"`, "LOAD_RW"},
		{`"%s ENCR"`, "LOAD_RW"},
	}
	n := first
	for _, w := range want {
		for ; n <= last && !strings.Contains(f.lines[n-1], w.snippet); n++ {
		}
		if n > last {
			t.Fatalf("%s: init_ult3580_lbp no longer has %s in the expected order", f.at(first-1), w.snippet)
		}
		if w.load == "" {
			continue
		}
		for ; n <= last && !strings.Contains(f.lines[n-1], "add_drive_media_list("); n++ {
		}
		if n > last || !strings.Contains(f.lines[n-1], w.load) {
			t.Fatalf("%s: init_ult3580_lbp adds %s without %s", f.at(n), w.snippet, w.load)
		}
	}
}

// bodyAnyProto finds a static function whose prototype spans several lines.
func bodyAnyProto(f cfile, fn string) (int, int, bool) {
	start := regexp.MustCompile(`^static void ` + regexp.QuoteMeta(fn) + `\(`)
	for i, l := range f.lines {
		if !start.MatchString(l) {
			continue
		}
		for j := i; j < len(f.lines); j++ {
			if strings.HasSuffix(strings.TrimSpace(f.lines[j]), "{") {
				for k := j + 1; k < len(f.lines); k++ {
					if strings.HasPrefix(f.lines[k], "}") {
						return j + 2, k, true
					}
				}
			}
		}
	}
	return 0, 0, false
}

func TestCrossCheckDriveMedia(t *testing.T) {
	root := mhvtlRepo(t)
	files := pmFiles(t, root)
	want := map[string]bool{DefaultDriveInit: true}
	for _, d := range DriveProducts {
		want[d.InitFn] = true
	}
	seen := map[string]bool{}
	for _, dm := range DriveModels {
		if seen[dm.InitFn] {
			t.Errorf("DriveModels lists %s twice", dm.InitFn)
		}
		seen[dm.InitFn] = true
		if !want[dm.InitFn] {
			t.Errorf("DriveModels has %s, which no tape_drives row selects", dm.InitFn)
		}
	}
	for fn := range want {
		if !seen[fn] {
			t.Errorf("DriveModels has no entry for %s", fn)
		}
	}
	for _, dm := range DriveModels {
		f, first, last := findInit(t, files, dm.InitFn)
		logDrift(t, dm.Source, f.at(first-1))
		arrays := genArrays(f)
		var got []cmedia
		for n := first; n <= last; n++ {
			l := f.lines[n-1]
			if m := mediaCallRe.FindStringSubmatch(l); m != nil {
				got = append(got, cmedia{Media{m[2], Load(m[1] == "RW")}, n})
			}
			if m := arraySizeRe.FindStringSubmatch(l); m != nil {
				if !strings.Contains(strings.Join(f.lines[max(first, n-2)-1:n], " "), "init_ult3580_lbp(") {
					t.Fatalf("%s: ARRAY_SIZE outside an init_ult3580_lbp call", f.at(n))
				}
				checkLBPHelper(t, f)
				arr, ok := arrays[m[1]]
				if !ok {
					t.Fatalf("%s: ult_gen_media table %s not found", f.at(n), m[1])
				}
				got = append(got, arr...)
			}
		}
		for i := 0; i < max(len(got), len(dm.Media)); i++ {
			switch {
			case i >= len(got):
				t.Errorf("%s: DriveModels %s media[%d] %+v is not added by mhVTL", f.at(last+1), dm.InitFn, i, dm.Media[i])
			case i >= len(dm.Media):
				t.Errorf("%s: %s adds %+v, missing from DriveModels", f.at(got[i].line), dm.InitFn, got[i].Media)
			case got[i].Media != dm.Media[i]:
				t.Errorf("%s: %s adds %+v; DriveModels has %+v", f.at(got[i].line), dm.InitFn, got[i].Media, dm.Media[i])
			}
		}
	}
}

var (
	densityRe   = regexp.MustCompile(`str(?:n)?cmp\(density, "([^"]+)"`)
	mediaTypeRe = regexp.MustCompile(`mamp->MediaType\s*=\s*(Media_\w+);`)
	nameTypeRe  = regexp.MustCompile(`\{"([^"]+)",\s*(Media_\w+),`)
)

// TestCrossCheckDensityMedia checks that each density mktape accepts maps to
// the media_handling name of the same media type: vtllib.c sets the MAM
// MediaType from the density, and the drive personalities name the types
// they load in their name_to_media_info tables.
func TestCrossCheckDensityMedia(t *testing.T) {
	root := mhvtlRepo(t)
	f := readC(t, root, "usr/vtllib.c")
	type den struct {
		typ  string
		line int
	}
	dens := map[string]den{}
	cur := ""
	for n, l := range f.lines {
		if m := densityRe.FindStringSubmatch(l); m != nil {
			cur = m[1]
			dens[cur] = den{line: n + 1}
			continue
		}
		if m := mediaTypeRe.FindStringSubmatch(l); m != nil && cur != "" && dens[cur].typ == "" {
			dens[cur] = den{m[1], dens[cur].line}
		}
	}
	if len(dens) == 0 {
		t.Fatal("no density branches found in usr/vtllib.c")
	}
	names := map[string]map[string]bool{} // name → media types
	for _, pf := range pmFiles(t, root) {
		for _, l := range pf.lines {
			if m := nameTypeRe.FindStringSubmatch(l); m != nil {
				if names[m[1]] == nil {
					names[m[1]] = map[string]bool{}
				}
				names[m[1]][m[2]] = true
			}
		}
	}
	for d, v := range dens {
		name, ok := DensityMedia[d]
		if !ok {
			t.Errorf("%s: density %q (%s) is missing from DensityMedia", f.at(v.line), d, v.typ)
			continue
		}
		if !names[name][v.typ] {
			t.Errorf("%s: density %q is %s, but no media_handling table names %q with that type", f.at(v.line), d, v.typ, name)
		}
	}
	for d := range DensityMedia {
		if _, ok := dens[d]; !ok {
			t.Errorf("DensityMedia has %q, which usr/vtllib.c does not accept", d)
		}
	}
}

// TestCrossCheckBarcodeRules runs make_vtl_media's own set_density and
// set_media_type on a corpus of barcodes and compares them with the Go port.
func TestCrossCheckBarcodeRules(t *testing.T) {
	root := mhvtlRepo(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found; cannot run make_vtl_media's functions")
	}
	b, err := os.ReadFile(filepath.Join(root, "usr", "cmd", "make_vtl_media.in"))
	if err != nil {
		t.Fatal(err)
	}
	script := shellFunc(t, string(b), "set_media_type") + shellFunc(t, string(b), "set_density")

	var corpus []string
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for _, k := range "SLXTJDPQ" {
		for _, c := range chars {
			corpus = append(corpus, "E01001"+string(k)+string(c))
		}
	}
	corpus = append(corpus, "CLN001L4", "CLN101L8", "W00001L6", "WORM01JA", "A1L6", "E01001L", "e01001l6",
		"ABCDEFGHL8", "ABCDEFGHIL8", "TAPE01LA", "TAPE01LH", "TAPE01PA", "TAPE01PB", "E01001JW")
	script += "for b in " + strings.Join(corpus, " ") + "; do set_density $b; set_media_type $b; echo \"$b $DENSITY $MEDIA_TYPE\"; done\n"
	cmd := exec.Command(bash, "-c", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash: %v: %s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(corpus) {
		t.Fatalf("bash printed %d lines for %d barcodes", len(lines), len(corpus))
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) != 3 {
			t.Fatalf("bash output %q", l)
		}
		if got := BarcodeDensity(f[0]); got != f[1] {
			t.Errorf("make_vtl_media.in set_density(%s) = %s; BarcodeDensity = %s", f[0], f[1], got)
		}
		if got := BarcodeMediaType(f[0]); got != f[2] {
			t.Errorf("make_vtl_media.in set_media_type(%s) = %s; BarcodeMediaType = %s", f[0], f[2], got)
		}
	}
}

// shellFunc returns the shell function name() { ... } from script.
func shellFunc(t *testing.T, script, name string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != name+"()" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == "}" {
				return strings.Join(lines[i:j+1], "\n") + "\n"
			}
		}
	}
	t.Fatalf("make_vtl_media.in: function %s not found", name)
	return ""
}

// TestCitationsWithinReach fails when a citation names another file, or a
// line more than maxCitationDrift lines from where the C source has the cited
// statement now: at that distance it likely points into another function.
func TestCitationsWithinReach(t *testing.T) {
	root := mhvtlRepo(t)
	type pair struct{ what, cited, found string }
	var pairs []pair

	rules := dispatch(t, readC(t, root, "usr/cmd/vtllibrary.c"), "customise_lu", nil)
	for i := 0; i < min(len(rules), len(Rules)); i++ {
		pairs = append(pairs, pair{fmt.Sprintf("Rules[%d]", i), Rules[i].Source, rules[i].Source})
	}
	files := pmFiles(t, root)
	for fn, m := range Models {
		f, first, last := findInit(t, files, fn)
		pairs = append(pairs, pair{fmt.Sprintf("Models[%q].WindowSource", fn), m.WindowSource, windowSource(f, smcFields(t, f, first, last))})
	}
	rows, _ := driveProducts(t, root)
	for i := 0; i < min(len(rows), len(DriveProducts)); i++ {
		pairs = append(pairs, pair{fmt.Sprintf("DriveProducts[%d]", i), DriveProducts[i].Source, rows[i].Source})
	}
	for _, dm := range DriveModels {
		f, first, _ := findInit(t, files, dm.InitFn)
		pairs = append(pairs, pair{fmt.Sprintf("DriveModels %s", dm.InitFn), dm.Source, f.at(first - 1)})
	}

	for _, p := range pairs {
		cf, cl := splitCitation(p.cited)
		ff, fl := splitCitation(p.found)
		switch {
		case cl < 0:
			t.Errorf("%s: citation %q is not file:line", p.what, p.cited)
		case cf != ff:
			t.Errorf("%s cites %s, but the C source has it in %s", p.what, p.cited, p.found)
		case cl-fl > maxCitationDrift || fl-cl > maxCitationDrift:
			t.Errorf("%s cites %s, %d lines from where the C source has it (%s); update the citation", p.what, p.cited, abs(cl-fl), p.found)
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
