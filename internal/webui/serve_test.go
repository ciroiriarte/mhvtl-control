package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mhvtl-control/internal/api"
	"mhvtl-control/internal/webui"
)

// newServer builds the real API handler with the embedded dashboard wired in,
// exactly as cmd/mhvtl-control does.
func newServer() http.Handler {
	return api.NewServer(api.Config{StaticFS: webui.FS()}).Handler()
}

func get(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res, string(body)
}

func TestIndexServesDashboard(t *testing.T) {
	h := newServer()
	res, body := get(t, h, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET / content-type = %q, want text/html", ct)
	}
	// The three Workstream-E panels and the theme/remote controls must render.
	for _, id := range []string{"library-rack", "disk-dock", "system-status", "service-status", "theme-select", "target-dialog"} {
		if !strings.Contains(body, id) {
			t.Errorf("index.html missing expected element id %q", id)
		}
	}
	// Referenced assets must be embedded under static/.
	for _, ref := range []string{"static/css/themes.css", "static/js/app.js"} {
		if !strings.Contains(body, ref) {
			t.Errorf("index.html does not reference %q", ref)
		}
	}
}

func TestIndexHasHardeningStructure(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/")
	// Active-node badge, the mount-picker and format dialogs, the per-panel
	// status regions, and the Target-dialog Test/Remember/Forget controls.
	for _, want := range []string{
		"node-badge",
		"mount-dialog",
		"format-dialog",
		"format-confirm-input", // typed-confirm field
		"target-test",          // Test Connection
		"target-remember",      // Remember checkbox
		"target-forget",        // Forget control
		"lib-status",           // per-panel status containers
		"system-status-region",
		"disk-status",
		"data-panel-status",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing hardening element %q", want)
		}
	}
	// core.js must be referenced and loaded BEFORE app.js.
	ci := strings.Index(body, "static/js/core.js")
	ai := strings.Index(body, "static/js/app.js")
	if ci < 0 {
		t.Errorf("index.html does not reference core.js")
	}
	if ai < 0 {
		t.Errorf("index.html does not reference app.js")
	}
	if ci >= 0 && ai >= 0 && ci > ai {
		t.Errorf("core.js (%d) must load before app.js (%d)", ci, ai)
	}
}

// TestIndexHasMediaAndMapStructure asserts the media panel and the dialogs
// for media create/edit, MAP import, slot moves, confirmations and drive
// statistics are declared in index.html.
func TestIndexHasMediaAndMapStructure(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/")
	for _, id := range []string{
		"media-panel",
		"media-list",
		"media-status",
		"media-create-dialog",
		"media-edit-dialog",
		"import-dialog",
		"move-dialog",
		"confirm-dialog",
		"stats-dialog",
	} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("index.html missing element id %q", id)
		}
	}
	// The media status region announces like the other panels.
	if !strings.Contains(body, `id="media-status" data-panel-status`) {
		t.Errorf("media-status is not a data-panel-status region")
	}
	if !strings.Contains(body, "TapeAlert flags are not read") {
		t.Errorf("stats dialog missing the TapeAlert footnote")
	}
}

func TestAppJSHasMediaAndMapBehavior(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/static/js/app.js")
	for _, want := range []string{
		"/media",      // media list / create / edit
		"/map/export", // export from slot, drive or barcode
		"/map/import", // import from the shelf
		"/map/eject",  // eject the MAP to the shelf
		"/move",       // slot <-> MAP moves
		"/stats",      // drive statistics
		"expandBarcodes",
		"buildMediaPatch",
		"fmtRatio",
		"openConfirm",
		// Partial failures (placing stopped, import not stored) survive the
		// refresh that follows every action.
		`mediaStatus.warn("Placing stopped`,
		`mediaStatus.warn("Imported`,
		"keepSticky",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js missing marker %q", want)
		}
	}
	for _, banned := range []string{"window.confirm", "alert(", "confirm(\"", ".innerHTML"} {
		if strings.Contains(body, banned) {
			t.Errorf("app.js must not use %q", banned)
		}
	}
}

func TestAppJSHasHardeningBehavior(t *testing.T) {
	h := newServer()
	res, body := get(t, h, "/static/js/app.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET app.js status = %d, want 200", res.StatusCode)
	}
	// The separate MAP control (distinct from Mount) and the mount/format flows.
	for _, want := range []string{
		"Load from MAP", // separate, labeled MAP control
		"buildMountBody",
		"mount-dialog",
		"format-dialog",
		"generation",      // node-isolation generation guard
		"canSendMutation", // hard per-mutation cross-node generation gate
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js missing expected marker %q", want)
		}
	}
}

func TestCoreJSServed(t *testing.T) {
	h := newServer()
	res, body := get(t, h, "/static/js/core.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/js/core.js status = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(body, "buildMountBody") {
		t.Errorf("core.js served but missing buildMountBody helper")
	}
}

func TestJSTestsNotEmbedded(t *testing.T) {
	h := newServer()
	// The Node test suite lives under internal/webui/jstest, OUTSIDE assets/,
	// so it must never be reachable through the served static tree.
	for _, path := range []string{
		"/static/js/core.test.cjs",
		"/jstest/core.test.cjs",
		"/static/../jstest/core.test.cjs",
	} {
		res, _ := get(t, h, path)
		if res.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200, want non-200 (test files must not be embedded/served)", path)
		}
	}
}

func TestBothThemesPresent(t *testing.T) {
	h := newServer()
	res, css := get(t, h, "/static/css/themes.css")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET themes.css status = %d, want 200", res.StatusCode)
	}
	// Both the modern default and the classic (mhvtl-gui) theme must be defined.
	for _, marker := range []string{`data-theme="modern"`, `data-theme="classic"`} {
		if !strings.Contains(css, marker) {
			t.Errorf("themes.css missing theme selector %q", marker)
		}
	}
	// A couple of classic-palette tokens, proving it mimics mhvtl-gui.
	for _, token := range []string{"#00ccff", "Trebuchet MS"} {
		if !strings.Contains(css, token) {
			t.Errorf("classic theme missing legacy mhvtl-gui token %q", token)
		}
	}
}

func TestIndexHasScalableNavStructure(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/")
	// US-F6 scalable/searchable/multi-library controls + US-F9 ARIA scaffolding.
	for _, want := range []string{
		"slot-search",        // barcode search input
		"slot-state-filter",  // All/Occupied/Empty state filter
		"view-mode-toggle",   // compact table-mode toggle
		"library-selector",   // multi-library navigation
		`role="alert"`,       // status regions announce (US-F9)
		`aria-live="polite"`, // ...politely
		"aria-labelledby",    // dialogs labelled by their heading (US-F9)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html missing scalable/aria marker %q", want)
		}
	}
}

func TestAppJSHasTelemetryAndTruthfulLabels(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/static/js/app.js")
	for _, want := range []string{
		"Import / Export (MAP)", // distinct MAP area (US-F7)
		"Eject all to shelf",    // MAP eject (confirmed in confirm-dialog)
		"MAP OPEN",              // MAP door state
		"compression",           // drive compression telemetry badge (US-F7)
		"scsi ",                 // SCSI channel:target:lun badge
		"naa ",                  // NAA badge
		"configured",            // truthful "configured topology" qualifier
		"provisioned ",          // relabelled capacity (US-F7)
		"progressbar",           // capacity bar ARIA role (US-F9)
		"aria-valuenow",         // ...with value semantics
		"MVTL cartridge",        // US-F8 distinct disk-status labels
		"No MVTL (unformatted)",
		"Probe failed",
		"In use (mounted)",
		"filterSlots", // US-F6 pure helpers are wired into the client
		"countSlots",
		"unknown (changer not reachable)", // no load claimed without live changer state
		"driveState",                      // load state only from the live view
		"mountCandidates",                 // picker lists live storage + MAP slots
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js missing marker %q", want)
		}
	}
}

func TestCoreJSHasSlotHelpers(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/static/js/core.js")
	for _, want := range []string{"filterSlots", "countSlots", "slotIsOccupied", "driveState", "firstEmptyDrive", "mountCandidates", "expandBarcodes", "buildMediaPatch", "fmtRatio", "emptyLocations"} {
		if !strings.Contains(body, want) {
			t.Errorf("core.js missing exported helper %q", want)
		}
	}
}

func TestThemesResponsiveGridFixAndContrast(t *testing.T) {
	h := newServer()
	_, css := get(t, h, "/static/css/themes.css")
	// US-F10: the broken prefix selector must be GONE...
	if strings.Contains(css, `[class^="col-"]`) {
		t.Errorf("themes.css still contains the broken [class^=\"col-\"] grid rule")
	}
	// ...and replaced by a rule that targets the col-* classes directly.
	if !strings.Contains(css, ".grid > .col-8") {
		t.Errorf("themes.css missing the new .grid > .col-8 responsive rule")
	}
	// US-F9: the AA-fixed foreground tokens must be present.
	for _, token := range []string{"#94a3b8", "#a8002a", "#00698f"} {
		if !strings.Contains(css, token) {
			t.Errorf("themes.css missing WCAG-AA token %q", token)
		}
	}
}

// TestClassicThemeChrome asserts the classic theme carries the faithful
// mhvtl-gui skin (blue gradient banner, purple section headers, black terminal
// System pane, beveled buttons) rather than a plain light palette.
func TestClassicThemeChrome(t *testing.T) {
	h := newServer()
	_, css := get(t, h, "/static/css/themes.css")
	for _, marker := range []string{
		`[data-theme="classic"] .topbar`,             // blue gradient banner
		`[data-theme="classic"] #system-panel .body`, // black terminal pane
		`[data-theme="classic"] .panel > header`,     // purple section header
		`[data-theme="classic"] .btn {`,              // beveled buttons
		"#8a008a",                                    // purple header color (AA 6.87:1 on #e6e6e6)
	} {
		if !strings.Contains(css, marker) {
			t.Errorf("classic theme missing faithful-skin marker %q", marker)
		}
	}
}

func TestReferencedAssetsDoNotFourOhFour(t *testing.T) {
	h := newServer()
	for _, path := range []string{"/static/css/themes.css", "/static/js/app.js"} {
		res, _ := get(t, h, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, res.StatusCode)
		}
	}
}

// The configuration view has its panels and the banner. Its edits are
// offered only while capabilities.writes is "ok" (plan 4.2): every edit goes
// through stageEdit, which refuses unless cfgEditable, and the stage panel
// shows the diff and the dry run before an apply.
func TestConfigView(t *testing.T) {
	h := newServer()
	_, index := get(t, h, "/")
	for _, id := range []string{"config-panel", "config-status", "config-notice", "config-library", "config-target-app",
		"config-view", "config-mode", "findings-panel", "findings-status", "config-findings",
		"stage-panel", "stage-status", "stage-view", "stage-count"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("index.html missing element id %q", id)
		}
	}
	_, app := get(t, h, "/static/js/app.js")
	for _, want := range []string{"/api/v1/config/capabilities", "/api/v1/config/personalities", "/api/v1/config/validate", "config.slots", "capabilityNotice",
		"writesEditable(lastCfg.caps)", `if (!cfgEditable()) { done(new Error("Editing is not available now.")); return; }`, `cfgPath("/apply")`, "dry_run: true",
		`api(cfgPath("/stages/" + encodeURIComponent(st.id)), { method: "DELETE" })`, "cfgStage.data.notes",
		"setApplying(st, true)", "discard.disabled = !editable",
		`editButton("New library…"`, `stageEdit("PATCH", cfgPath("/system"), body, done)`, "wizardRequests(v.w)",
		// The wizard reads the stage when it checks, not when it opened.
		"function staged() { return cfgStage && cfgStage.data; }", "usedAddresses(config, staged())", "usedNAAs(config, st)"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js missing marker %q", want)
		}
	}
	// Reload after a conflict drops the stage on the daemon too (DELETE
	// /config/stages/{id}), not only here. A pin with in is looked for in
	// that block's body.
	for _, c := range []struct{ in, head, want string }{
		{"", `function dropServerStage(st) {`, `api(cfgPath("/stages/" + encodeURIComponent(st.id)), { method: "DELETE" })`},
		{"", `editButton("Reload", "Reload the configuration and drop the pending changes", function () {`, "dropServerStage(st);"},
		// A lost apply whose outcome could not be read keeps edits held
		// and offers Check again.
		{"", "if (outcomeUnread(e)) {", "setApplying(st, true);"},
		{"", "if (outcomeUnread(e)) {", "offerCheckAgain(st, gen);"},
		{"", "function offerCheckAgain(st, gen) {", "learnOutcome(st, gen);"},
		// A create sequence that fails part way through a stage it made
		// drops that stage on the daemon too.
		{"", "if (dropsPartialStage(fresh, i, stageId())) {", "dropServerStage(cfgStage);"},
		{"", "if (dropsPartialStage(fresh, i, stageId())) {", "cfgStage = null;"},
		// The target app's check sends validation only the files it takes.
		{"", "function checkForApp(libID, app) {", "files: validateFiles(st.data.files)"},
		// The wizard reads the stage when its OK is clicked.
		{"function wizardDialog() {", `ok.addEventListener("click", function () {`, "var st = staged();"},
	} {
		src, ok := app, true
		if c.in != "" {
			if src, ok = jsBody(app, c.in); !ok {
				t.Errorf("app.js missing %q", c.in)
				continue
			}
		}
		if body, ok := jsBody(src, c.head); !ok {
			t.Errorf("app.js missing %q", c.head)
		} else if !strings.Contains(body, c.want) {
			t.Errorf("app.js %q does not run %q", c.head, c.want)
		}
	}
	// Every configuration request other than a GET is the validation, a dry
	// run or apply of the stage, a stage delete, or an edit sent through
	// stageEdit, which refuses unless cfgEditable. The other requests that
	// send a method go to the library and storage routes, or are a service
	// action (POST /api/v1/service/...). A call with a second argument other
	// than { signal: signal } may send one.
	sends := map[string]bool{`cfgPath("/apply")`: true, `cfgPath("/stages/" + encodeURIComponent(st.id))`: true,
		`"/api/v1/config/validate"`: true, `cfgPath("/validate")`: true, "path": true, "o.path": true, "servicePath(scope, action)": true}
	pathCalls := 0
	for _, args := range jsCalls(app, "api") {
		if args[0] == "path" {
			pathCalls++
		}
		if len(args) < 2 || args[1] == "{ signal: signal }" {
			continue
		}
		if !sends[args[0]] && !strings.HasPrefix(args[0], `"/api/v1/libraries/`) && !strings.HasPrefix(args[0], `"/api/v1/storage/`) {
			t.Errorf("app.js sends a configuration request outside stageEdit and the apply flow: api(%s)", strings.Join(strings.Fields(strings.Join(args, ", ")), " "))
		}
	}
	body, _ := jsBody(app, "function stageEdit(method, path, body, done) {")
	if pathCalls != 1 || !strings.Contains(body, "api(path, { method: method, body: body })") {
		t.Error("app.js sends api(path, ...) outside stageEdit")
	}
	// An answer for another node still ends the edit, so the dialog that
	// sent it takes input again.
	if n := strings.Count(body, `if (isStale(gen, NodeState.generation)) { done(new Error("The node changed.")); return; }`); n != 2 {
		t.Errorf("stageEdit calls done on a node change in %d of its 2 handlers", n)
	}
	for _, p := range []string{"path: cfgPath(", "path: libCfgPath(", `path: "/api/v1/config`} {
		if strings.Contains(app, p) {
			t.Errorf("app.js sends a configuration request through mutate: %q", p)
		}
	}
	_, core := get(t, h, "/static/js/core.js")
	if !strings.Contains(core, "systemctl enable --now mhvtl-control-apply.socket") {
		t.Error("core.js does not say how to enable the apply helper")
	}
}

// jsBody and jsCalls read past comments, strings and regexes, and leave
// out a function the block only defines.
func TestJSBody(t *testing.T) {
	src := `function f(st) {
  var a = "}", b = "//", c = "http://x", d = '{', e = /[}\/]/g, g = 4 / 2 / 1;
  /* drop(st); */ keep(a); // drop(st);
  function unused() { drop(st); }
  var h = function () { drop(st); };
  api(cfgPath("/libraries/1"),
    { method: "DELETE" }).then(function () { done(st); });
  tail(st);
}`
	body, ok := jsBody(src, "function f(st) {")
	if !ok {
		t.Fatal("jsBody did not find f")
	}
	for _, want := range []string{`c = "http://x"`, "keep(a);", "done(st);", "tail(st);"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "drop(st)") {
		t.Errorf("body runs a commented or uncalled drop(st):\n%s", body)
	}
	calls := jsCalls(src, "api")
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != `cfgPath("/libraries/1")` || calls[0][1] != `{ method: "DELETE" }` {
		t.Errorf("jsCalls = %q", calls)
	}
}

// jsBody returns the code a block runs: what lies between the brace that
// ends head and the brace that closes it, without comments and without the
// functions it only defines. A function there that follows "(" or "," is
// an argument, a callback such as the ones api().then and editButton run,
// and stays. Any other, a declaration or one assigned to a name, runs only
// when called, so a statement pinned inside it does not count.
func jsBody(src, head string) (string, bool) {
	code, shape := jsLex(src)
	i := strings.Index(code, head)
	if i < 0 || !strings.HasSuffix(head, "{") {
		return "", false
	}
	start := i + len(head)
	end := jsClose(shape, start, '{', '}')
	if end < 0 {
		return "", false
	}
	var b strings.Builder
	from := start
	for k := start; k < end; k++ {
		if !jsWord(shape, k, "function") {
			continue
		}
		if p := strings.TrimRight(shape[:k], " \t\r\n"); strings.HasSuffix(p, "(") || strings.HasSuffix(p, ",") {
			continue
		}
		open := strings.IndexByte(shape[k:end], '{')
		if open < 0 {
			break
		}
		close := jsClose(shape, k+open+1, '{', '}')
		if close < 0 || close >= end {
			break
		}
		b.WriteString(code[from:k])
		from, k = close+1, close
	}
	b.WriteString(code[from:end])
	return b.String(), true
}

// jsCalls returns the arguments of every call to name in src, as code,
// split at the commas outside brackets, so a call split over lines is one
// call. The function's own definition is not a call.
func jsCalls(src, name string) [][]string {
	code, shape := jsLex(src)
	var calls [][]string
	for k := 0; k < len(shape); k++ {
		if !jsWord(shape, k, name) || !strings.HasPrefix(shape[k+len(name):], "(") ||
			strings.HasSuffix(strings.TrimRight(shape[:k], " \t\r\n"), "function") {
			continue
		}
		open := k + len(name) + 1
		end := jsClose(shape, open, '(', ')')
		if end < 0 {
			break
		}
		var args []string
		depth, from := 0, open
		for j := open; j < end; j++ {
			switch shape[j] {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				depth--
			case ',':
				if depth == 0 {
					args = append(args, strings.TrimSpace(code[from:j]))
					from = j + 1
				}
			}
		}
		calls = append(calls, append(args, strings.TrimSpace(code[from:end])))
	}
	return calls
}

// jsLex returns two copies of src, each as long as it: code, with its
// comments blanked, and shape, which also blanks what string, template and
// regex literals hold. Brackets and words are found in shape and the text
// between them read from code, so a commented-out statement does not count
// and a "}" or "//" in a string does not end a block or start a comment. A
// slash after an operator, an opening bracket, a comma or return starts a
// regex; elsewhere it divides. A template literal is taken whole, ${}
// included. It is a lexer for app.js, not a JavaScript parser.
func jsLex(src string) (code, shape string) {
	c, s := []byte(src), []byte(src)
	blank := func(b []byte, from, to int) {
		for k := from; k < to; k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	var last byte // the last character that is not space or comment
	word := ""    // the word last ends, if any
	for j := 0; j < len(src); {
		ch, end := src[j], j+1
		switch {
		case strings.HasPrefix(src[j:], "//"):
			if end = strings.IndexByte(src[j:], '\n'); end < 0 {
				end = len(src)
			} else {
				end += j
			}
			blank(c, j, end)
			blank(s, j, end)
			j = end
			continue
		case strings.HasPrefix(src[j:], "/*"):
			if end = strings.Index(src[j+2:], "*/"); end < 0 {
				end = len(src)
			} else {
				end += j + 4
			}
			blank(c, j, end)
			blank(s, j, end)
			j = end
			continue
		case ch == '"' || ch == '\'' || ch == '`' ||
			ch == '/' && (last == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", last) >= 0 || word == "return" || word == "typeof"):
			end = jsLitEnd(src, j)
			blank(s, j+1, end-1)
			word = ""
		case jsIdent(ch):
			for end < len(src) && jsIdent(src[end]) {
				end++
			}
			word = src[j:end]
		case ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n':
			j++
			continue
		default:
			word = ""
		}
		last = src[end-1]
		j = end
	}
	return string(c), string(s)
}

// jsLitEnd returns the index just past the string, template or regex
// literal that starts at j. A string ends at a line end if not closed.
func jsLitEnd(src string, j int) int {
	q, class := src[j], false
	for k := j + 1; k < len(src); k++ {
		switch ch := src[k]; {
		case ch == '\\':
			k++
		case q == '/' && ch == '[':
			class = true
		case q == '/' && ch == ']':
			class = false
		case ch == q && !class:
			return k + 1
		case ch == '\n' && q != '`':
			return k
		}
	}
	return len(src)
}

// jsClose returns the index of the close that ends the bracket opened just
// before from, or -1.
func jsClose(shape string, from int, open, close byte) int {
	depth := 1
	for j := from; j < len(shape); j++ {
		switch shape[j] {
		case open:
			depth++
		case close:
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// jsWord reports whether the word w stands alone at k in s.
func jsWord(s string, k int, w string) bool {
	return strings.HasPrefix(s[k:], w) && (k == 0 || !jsIdent(s[k-1])) &&
		(k+len(w) == len(s) || !jsIdent(s[k+len(w)]))
}

func jsIdent(ch byte) bool {
	return ch == '_' || ch == '$' || ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
}
