// Package api contains pakman's HTTP handlers, middleware, and router.
//
// Every error response flows through writeError so clients can parse a
// consistent JSON envelope. error_code is the documented machine-readable
// identifier — see design.md §7 — and the hint field is the optional
// "what the operator should try next" string.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error codes. Keep this list in sync with openapi/openapi.yaml (once
// that file lands in Phase B). A change here that doesn't mirror the
// spec will break SDK generators downstream.
const (
	CodeBadRequest        = "bad_request"
	CodeUnauthorized      = "unauthorized"
	CodeInsufficientScope = "insufficient_scope"
	CodeNotFound          = "not_found"
	CodeConflict          = "conflict"
	CodeVersionImmutable  = "version_immutable"
	CodeChannelImmutable  = "channel_immutable"
	CodeInternal          = "internal_error"
	CodeUnavailable       = "unavailable"
	CodePayloadTooLarge   = "payload_too_large"
)

// ErrorBody is the JSON body returned for every non-2xx response. Kept
// flat (no nested "error" object) so R callers can parse it with a
// single jsonlite::fromJSON() into a named list without post-processing.
type ErrorBody struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// writeError sends an ErrorBody with the given HTTP status. It pulls the
// request ID off the request context so clients can quote a single
// identifier when filing a bug. Logs at warn or error level depending
// on severity so ops tooling can split routine 4xx chatter from real
// server issues.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message, hint string) {
	body := ErrorBody{
		ErrorCode: code,
		Message:   message,
		Hint:      hint,
		RequestID: RequestIDFromContext(r.Context()),
	}
	logAt := slog.LevelWarn
	if status >= 500 {
		logAt = slog.LevelError
	}
	slog.Default().Log(r.Context(), logAt, "http error response",
		"status", status,
		"error_code", code,
		"message", message,
		"path", r.URL.Path,
		"method", r.Method,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Nothing useful to do — the body is already written out.
		slog.Default().Error("encode error body", "err", err)
	}
}

// writeJSON is the success-path counterpart to writeError. Sets the
// content type, writes the status, and JSON-encodes body. Errors from
// encoding are logged but not propagated — the status line is already
// on the wire.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().ErrorContext(r.Context(), "encode response body",
			"err", err, "path", r.URL.Path)
	}
}
