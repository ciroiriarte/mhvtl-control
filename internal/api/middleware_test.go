package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthRequiresBearer(t *testing.T) {
	h := NewServer(Config{Token: "s3cret"}).Handler()

	// Missing token.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", env.Error.Code)
	}

	// Wrong token.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token status = %d, want 401", rec.Code)
	}

	// Correct token.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("good-token status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAuthExemptsHealthz(t *testing.T) {
	h := NewServer(Config{Token: "s3cret"}).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := NewServer(Config{CORSOrigin: "https://ui.example"}).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/system", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ui.example" {
		t.Errorf("Allow-Origin = %q, want https://ui.example", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET,POST,PATCH,DELETE,OPTIONS" {
		t.Errorf("Allow-Methods = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Authorization,Content-Type" {
		t.Errorf("Allow-Headers = %q", got)
	}
}

func TestRecoverMiddleware(t *testing.T) {
	h := recoverMW(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "internal" {
		t.Errorf("code = %q, want internal", env.Error.Code)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusTeapot, "some_code", "some message")

	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var raw map[string]map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	e, ok := raw["error"]
	if !ok {
		t.Fatalf("no top-level error key: %v", raw)
	}
	if e["code"] != "some_code" || e["message"] != "some message" {
		t.Errorf("error = %v, want code/message", e)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestSameOriginRefusesCrossSiteWrites(t *testing.T) {
	cases := []struct {
		name    string
		cors    string
		token   string
		method  string
		headers map[string]string
		want    int
	}{
		{"curl, no browser headers", "", "", http.MethodPost, nil, http.StatusOK},
		{"same-origin fetch", "", "", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8080"}, http.StatusOK},
		{"typed URL", "", "", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"cross-site form", "", "", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-site other port", "", "", http.MethodPatch, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://127.0.0.1:9999"}, http.StatusForbidden},
		{"cross-site GET passes", "", "", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusOK},
		{"old browser, Origin matches Host", "", "", http.MethodPost, map[string]string{"Origin": "http://127.0.0.1:8080"}, http.StatusOK},
		{"old browser, foreign Origin", "", "", http.MethodDelete, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"opaque Origin", "", "", http.MethodPost, map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"configured CORS origin", "https://ui.example", "", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://ui.example"}, http.StatusOK},
		{"other origin with CORS set", "https://ui.example", "", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"CORS * without token", "*", "", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"CORS * with token", "*", "s3cret", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://ui.example"}, http.StatusOK},
		{"cross-site HEAD passes", "", "", http.MethodHead, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusOK},
		{"cross-site PUT", "", "", http.MethodPut, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewServer(Config{CORSOrigin: c.cors, Token: c.token}).sameOrigin(okHandler())
			req := httptest.NewRequest(c.method, "http://127.0.0.1:8080/api/v1/libraries/10/mount", nil)
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1:8080", http.StatusOK},
		{"localhost:8080", http.StatusOK},
		{"LOCALHOST", http.StatusOK},
		{"[::1]:8080", http.StatusOK},
		{"127.1.2.3", http.StatusOK},
		{"rebind.evil.example:8080", http.StatusMisdirectedRequest},
		{"192.168.1.10:8080", http.StatusMisdirectedRequest},
		{"localhost.evil.example", http.StatusMisdirectedRequest},
		{"localhost.", http.StatusMisdirectedRequest},
		{"127.0.0.1.", http.StatusMisdirectedRequest},
		{"foo.localhost", http.StatusMisdirectedRequest},
		{"0.0.0.0:8080", http.StatusMisdirectedRequest},
		{"[::1%25lo]:8080", http.StatusMisdirectedRequest},
		{"[::ffff:127.0.0.1]:8080", http.StatusOK},
		{"", http.StatusMisdirectedRequest},
	}
	h := NewServer(Config{RequireLoopbackHost: true}).loopbackHost(okHandler())
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
		req.Host = c.host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("Host %q: status = %d, want %d", c.host, rec.Code, c.want)
		}
	}

	// Off unless asked for: a token or a non-loopback listener disables it.
	h = NewServer(Config{}).loopbackHost(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	req.Host = "vtl.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled: status = %d, want 200", rec.Code)
	}
}

// The checks sit in the Handler chain, ahead of the routes.
func TestHandlerRefusesCrossSiteAndRebinding(t *testing.T) {
	h := NewServer(Config{RequireLoopbackHost: true}).Handler()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/v1/storage/disks/format", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site format: status = %d, want 403", rec.Code)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil || env.Error.Code != "cross_origin" {
		t.Fatalf("envelope = %+v (%v), want code cross_origin", env, err)
	}

	req = httptest.NewRequest(http.MethodGet, "http://rebind.evil.example:8080/api/v1/system", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding GET: status = %d, want 421", rec.Code)
	}
}

// With --cors-origin, the preflight is answered, the allowed origin reaches
// the route, and a refusal still carries Access-Control-Allow-Origin so the
// other node's dashboard can show the error.
func TestHandlerCrossOriginWithCORS(t *testing.T) {
	h := NewServer(Config{CORSOrigin: "https://ui.example"}).Handler()

	req := httptest.NewRequest(http.MethodOptions, "http://vtl.example:8080/api/v1/libraries/10/mount", nil)
	req.Header.Set("Origin", "https://ui.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: status = %d, want 204", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://vtl.example:8080/api/v1/libraries/10/mount", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://ui.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("allowed origin refused: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "http://vtl.example:8080/api/v1/libraries/10/mount", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other origin: status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ui.example" {
		t.Errorf("403 Access-Control-Allow-Origin = %q, want https://ui.example", got)
	}
}
