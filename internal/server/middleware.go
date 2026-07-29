package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/datcal/ambar/internal/httpx"
)

type requestIDContextKey struct{}

// RequestIDHeader is echoed back so a user can quote it when reporting a
// problem, and it ties a log line to a response.
const RequestIDHeader = "X-Request-Id"

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			// Not worth failing a request over; an empty ID is survivable.
			next.ServeHTTP(w, r)
			return
		}
		id := base64.RawURLEncoding.EncodeToString(raw)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id)))
	})
}

// RequestIDFromContext returns the current request's ID, or "".
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDContextKey{}).(string); ok {
		return id
	}
	return ""
}

// recoverPanic turns a panic into a 500.
//
// §16: "Recover middleware exists, but a recovered panic is a bug to fix, not a
// handled case." So it logs at Error with the stack, deliberately loudly.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A client disconnecting mid-write is not a bug in this code.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			s.log.ErrorContext(r.Context(), "panic recovered in HTTP handler",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", RequestIDFromContext(r.Context()),
				"stack", string(debug.Stack()),
			)
			// The stack never reaches the client.
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures what was actually sent, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *statusRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// matters for the streamed file responses arriving in M1.
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// accessLog emits one structured line per request (§12).
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		// The liveness probe runs constantly; logging it at Info would bury
		// everything else in the container log.
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug
		}
		if rec.status >= 500 {
			level = slog.LevelError
		}

		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			// The resolved client address, which honours AMBAR_TRUSTED_PROXIES
			// rather than blindly trusting a header.
			"ip", httpx.ClientIPString(r.Context()),
			"request_id", RequestIDFromContext(r.Context()),
		)
	})
}

// securityHeaders applies the §11 response headers to everything.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// §11: an uploaded .html or .svg served inline from the app origin is
		// stored XSS. nosniff is half the defence; the other half is
		// Content-Disposition: attachment on library content, which arrives with
		// file serving in M1.
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// No 'unsafe-inline' anywhere: all CSS and JS is served from /static.
		// The 3D viewer and other JS islands (§8) must keep to that rule.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; object-src 'none'; "+
				"base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
