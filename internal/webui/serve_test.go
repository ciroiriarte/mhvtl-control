package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mhvtld/internal/api"
	"mhvtld/internal/webui"
)

// newServer builds the real API handler with the embedded dashboard wired in,
// exactly as cmd/mhvtld does.
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
		"Import / Export (MAP)",                    // distinct MAP area (US-F7)
		"export/eject not available in this build", // disabled export/eject
		"compression",                              // drive compression telemetry badge (US-F7)
		"scsi ",                                    // SCSI channel:target:lun badge
		"naa ",                                     // NAA badge
		"configured",                               // truthful "configured topology" qualifier
		"provisioned ",                             // relabelled capacity (US-F7)
		"progressbar",                              // capacity bar ARIA role (US-F9)
		"aria-valuenow",                            // ...with value semantics
		"MVTL cartridge",                           // US-F8 distinct disk-status labels
		"No MVTL (unformatted)",
		"Probe failed",
		"In use (mounted)",
		"filterSlots", // US-F6 pure helpers are wired into the client
		"countSlots",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js missing marker %q", want)
		}
	}
}

func TestCoreJSHasSlotHelpers(t *testing.T) {
	h := newServer()
	_, body := get(t, h, "/static/js/core.js")
	for _, want := range []string{"filterSlots", "countSlots", "slotIsOccupied"} {
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
