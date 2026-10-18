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
	for _, id := range []string{"library-rack", "disk-dock", "system-status", "theme-select", "target-dialog"} {
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
		"setApplying(st, true)", "discard.disabled = !editable"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js missing marker %q", want)
		}
	}
	// Reload after a conflict drops the stage on the daemon too (DELETE
	// /config/stages/{id}), not only here.
	for _, c := range []struct{ head, want string }{
		{`function dropServerStage(st) {`, `api(cfgPath("/stages/" + encodeURIComponent(st.id)), { method: "DELETE" })`},
		{`editButton("Reload", "Reload the configuration and drop the pending changes", function () {`, "dropServerStage(st);"},
		// A lost apply whose outcome could not be read keeps edits held
		// and offers Check again.
		{"if (outcomeUnread(e)) {", "setApplying(st, true);"},
		{"if (outcomeUnread(e)) {", "offerCheckAgain(st, gen);"},
		{"function offerCheckAgain(st, gen) {", "learnOutcome(st, gen);"},
	} {
		if body, ok := jsBody(app, c.head); !ok {
			t.Errorf("app.js missing %q", c.head)
		} else if !strings.Contains(body, c.want) {
			t.Errorf("app.js %q does not run %q", c.head, c.want)
		}
	}
	// Every configuration request other than a GET is the validation, a dry
	// run or apply of the stage, a stage delete, or an edit sent through
	// stageEdit, which refuses unless cfgEditable. The other requests that
	// send a method go to the library and storage routes.
	sends := map[string]bool{`cfgPath("/apply")`: true, `cfgPath("/stages/" + encodeURIComponent(st.id))`: true,
		`"/api/v1/config/validate"`: true, "path": true, "o.path": true}
	for _, line := range strings.Split(app, "\n") {
		i := strings.Index(line, "api(")
		if i < 0 || !strings.Contains(line, "method:") {
			continue
		}
		arg := jsFirstArg(line[i+len("api("):])
		if !sends[arg] && !strings.HasPrefix(arg, `"/api/v1/libraries/`) && !strings.HasPrefix(arg, `"/api/v1/storage/`) {
			t.Errorf("app.js sends a configuration request outside stageEdit and the apply flow: %s", strings.TrimSpace(line))
		}
	}
	if body, _ := jsBody(app, "function stageEdit(method, path, body, done) {"); strings.Count(app, "api(path, {") != 1 ||
		!strings.Contains(body, "api(path, { method: method, body: body })") {
		t.Error("app.js sends api(path, ...) outside stageEdit")
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

// jsBody returns the code between the brace that ends head and the brace
// that closes it, without its whole-line comments, so a commented-out
// statement does not count.
func jsBody(src, head string) (string, bool) {
	i := strings.Index(src, head)
	if i < 0 || !strings.HasSuffix(head, "{") {
		return "", false
	}
	start, depth := i+len(head), 1
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var keep []string
				for _, l := range strings.Split(src[start:j], "\n") {
					if !strings.HasPrefix(strings.TrimSpace(l), "//") {
						keep = append(keep, l)
					}
				}
				return strings.Join(keep, "\n"), true
			}
		}
	}
	return "", false
}

// jsFirstArg returns a call's first argument, given the text after its
// opening parenthesis.
func jsFirstArg(s string) string {
	depth := 0
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return strings.TrimSpace(s[:j])
			}
			depth--
		case ',':
			if depth == 0 {
				return strings.TrimSpace(s[:j])
			}
		}
	}
	return strings.TrimSpace(s)
}
