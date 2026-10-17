package api

import (
	"crypto/subtle"
	"log"
	"net/http"
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
