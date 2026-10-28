package api

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// statusRecorder captures the response status for the logging middleware,
// and the matched route pattern (set by routeMux; empty when none matched).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
	route  string
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// recoverMW turns a panicking handler into a 500 error envelope so the daemon
// never crashes on a single bad request.
func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rv)
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logging writes one line per request: method, path, status, duration. It
// also passes the route pattern and status to count, when non-nil.
func logging(next http.Handler, count func(route string, status int)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
		if count != nil {
			count(rec.route, rec.status)
		}
	})
}

// cors sets CORS headers and answers OPTIONS preflight requests when a CORS
// origin is configured; otherwise it is a no-op passthrough.
func (s *Server) cors(next http.Handler) http.Handler {
	origin := s.cfg.CORSOrigin
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin refuses state-changing requests a browser sent from another
// site: without it any page the operator visits can POST mount, move or
// format (as text/plain, which needs no preflight) to a daemon on loopback
// with no token. Browsers send Sec-Fetch-Site and Origin on such requests
// (Fetch Metadata, https://www.w3.org/TR/fetch-metadata/; Origin,
// https://fetch.spec.whatwg.org/#origin-header); curl and scripts send
// neither and pass. The configured CORS origin is allowed cross-site; "*"
// only with a token, which a forged request cannot carry.
func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		allowed := s.cfg.CORSOrigin != "" &&
			(origin == s.cfg.CORSOrigin || (s.cfg.CORSOrigin == "*" && s.cfg.Token != ""))
		ok := true
		switch site := r.Header.Get("Sec-Fetch-Site"); {
		case site == "same-origin" || site == "none":
		case site != "":
			ok = allowed
		case origin != "":
			u, err := url.Parse(origin)
			ok = allowed || (err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host))
		}
		if !ok {
			writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost refuses requests whose Host is not a loopback name when the
// daemon listens on loopback with no token. A page on another site that
// re-resolves its own name to 127.0.0.1 (DNS rebinding) is same-origin to the
// browser, so sameOrigin cannot stop it and it could read and write the API;
// its Host header still names that site. Off when RequireLoopbackHost is
// false.
func (s *Server) loopbackHost(next http.Handler) http.Handler {
	if !s.cfg.RequireLoopbackHost {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			writeError(w, http.StatusMisdirectedRequest, "host_not_allowed",
				"Host must be a loopback name without an API token; set --api-token to serve other names")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth enforces a bearer token on /api/v1/*, /metrics and /readyz when a
// token is configured: metrics and readiness checks name libraries, homes,
// barcodes and errors. "/", "/static/" and "/healthz" are always exempt. It
// is a no-op passthrough when no token is set.
func (s *Server) auth(next http.Handler) http.Handler {
	token := s.cfg.Token
	if token == "" {
		return next
	}
	tokenBytes := []byte(token)
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") && r.URL.Path != "/metrics" && r.URL.Path != "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), tokenBytes) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
