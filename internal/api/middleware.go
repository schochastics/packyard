package api

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
)

// ctxKey is a private type for context keys so no external package can
// collide with (or read) our entries.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota + 1
)

// RequestIDFromContext returns the request ID attached by
// requestIDMiddleware, or "" if the request hasn't been through it
// (e.g. in unit tests that call a handler directly).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// requestIDMiddleware attaches a fresh UUIDv7 to the context and to the
// X-Request-Id response header. UUIDv7 (not v4) is used because the
// timestamp prefix makes request IDs roughly sortable — useful when
// cross-referencing logs across systems.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.NewV7()
		var rid string
		if err != nil {
			// NewV7 can only fail if the system random source is broken.
			// Fall back to v4 rather than dropping the request.
			rid = uuid.New().String()
		} else {
			rid = id.String()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder wraps http.ResponseWriter so the access-log middleware
// can learn what status was written, since net/http doesn't expose that
// by default. We intentionally don't implement http.Flusher / Hijacker /
// Pusher here — none of our handlers need them.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// accessLogMiddleware logs one structured line per request at INFO. The
// log line intentionally does NOT include query strings or request
// bodies — those are noisy and sometimes sensitive. Add specific fields
// in the handler if you need them.
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		slog.Default().InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
			"request_id", RequestIDFromContext(r.Context()),
		)
	})
}

// recoveryMiddleware converts a panic inside any handler into a 500 with
// the standard error envelope, and logs a stack trace. Ordering
// matters: recovery must sit INSIDE accessLog so the recovered response
// is still observed by the access log. It must sit OUTSIDE application
// middleware so panics in those still get caught.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			slog.Default().ErrorContext(r.Context(), "handler panic",
				"panic", rec,
				"stack", string(debug.Stack()),
				"path", r.URL.Path,
				"request_id", RequestIDFromContext(r.Context()),
			)
			writeError(w, r, http.StatusInternalServerError,
				CodeInternal, "internal server error",
				"check server logs; reference the request_id in bug reports")
		}()
		next.ServeHTTP(w, r)
	})
}

// chain composes middleware so that the first listed wraps the outermost.
// chain(h, A, B) returns A(B(h)).
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
