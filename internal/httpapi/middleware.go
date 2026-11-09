package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type contextKey int

const requestIDKey contextKey = iota

// requestIDHeader carries the request identifier in both directions.
const requestIDHeader = "X-Request-ID"

// RequestIDFrom returns the request identifier stored by the middleware, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// middleware is a handler decorator.
type middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first one listed is the outermost.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// withRequestID accepts a caller supplied identifier (bounded in length) or
// generates one, and echoes it in the response.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the status and size of a response for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withLogging writes one structured access log line per request. Health
// probes are logged at debug level to keep production logs readable.
func withLogging(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
				level = slog.LevelDebug
			}
			logger.Log(r.Context(), level, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
				"request_id", RequestIDFrom(r.Context()),
			)
		})
	}
}

// withRecover converts panics into a 500 problem response.
func withRecover(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic serving request",
						"panic", rec, "stack", string(debug.Stack()),
						"method", r.Method, "path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
					writeProblem(w, newProblem(r, http.StatusInternalServerError, "an unexpected error occurred"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// withTimeout bounds the work done for a request through its context, so
// slow database calls are cancelled and reported as 504 by the handler
// instead of hanging the connection.
func withTimeout(d time.Duration) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// swaggerCDN hosts the pinned Swagger UI assets used by /docs.
const swaggerCDN = "https://unpkg.com"

// contentSecurityPolicy allows the embedded dashboard (same origin, no inline
// scripts) and the Swagger UI assets from the pinned CDN.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' " + swaggerCDN + "; " +
	"style-src 'self' 'unsafe-inline' " + swaggerCDN + "; " +
	"img-src 'self' data: " + swaggerCDN + "; " +
	"font-src 'self' " + swaggerCDN + "; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// withSecurityHeaders sets defensive headers on every response.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
