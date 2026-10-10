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

func TestReferencedAssetsDoNotFourOhFour(t *testing.T) {
	h := newServer()
	for _, path := range []string{"/static/css/themes.css", "/static/js/app.js"} {
		res, _ := get(t, h, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, res.StatusCode)
		}
	}
}
