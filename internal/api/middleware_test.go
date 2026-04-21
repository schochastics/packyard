package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestRequestIDMiddlewareSetsHeaderAndContext(t *testing.T) {
	t.Parallel()

	var gotCtxID string
	h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCtxID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	hdr := rec.Header().Get("X-Request-Id")
	if hdr == "" {
		t.Fatal("X-Request-Id header missing")
	}
	if gotCtxID != hdr {
		t.Errorf("context ID %q != header %q", gotCtxID, hdr)
	}
	// UUIDv7 is a standard UUID string: 8-4-4-4-12 hex.
	uuidRE := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !uuidRE.MatchString(hdr) {
		t.Errorf("X-Request-Id %q does not look like a UUID", hdr)
	}
}

func TestRecoveryMiddlewareConvertsPanicTo500(t *testing.T) {
	t.Parallel()

	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}), requestIDMiddleware, recoveryMiddleware)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ErrorCode != CodeInternal {
		t.Errorf("error_code = %q, want %q", body.ErrorCode, CodeInternal)
	}
	if body.RequestID == "" {
		t.Error("request_id absent from error body")
	}
}

func TestRequestIDFromEmptyContext(t *testing.T) {
	t.Parallel()
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext(empty) = %q, want \"\"", got)
	}
}

func TestAccessLogMiddlewareDoesNotAlterResponse(t *testing.T) {
	t.Parallel()

	const payload = "hello"
	h := accessLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, payload)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), payload) {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), payload)
	}
}
