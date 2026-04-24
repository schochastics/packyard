package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/schochastics/packyard/internal/auth"
)

const (
	ctxKeyIdentity ctxKey = iota + 100
)

// authMiddleware reads the Authorization header, resolves it to an
// auth.Identity via the DB, and attaches the identity to the request
// context. It does NOT reject unauthenticated requests: some
// endpoints (notably /health and, on servers that allow it, the
// default-channel CRAN reads) don't require a token. Handlers that
// DO require auth call requireScope; handlers that don't look at
// the identity stay simple.
//
// An invalid/revoked/missing token is treated the same: no identity
// is attached. Lookup errors unrelated to "token not found" are
// logged but do not fail the request here — handlers requiring
// auth will reject when they look for an identity they don't find.
func authMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("Authorization")
			plaintext, ok := auth.ParseBearer(raw)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			id, err := auth.Lookup(r.Context(), deps.DB.DB, plaintext)
			switch {
			case err == nil:
				ctx := context.WithValue(r.Context(), ctxKeyIdentity, id)
				next.ServeHTTP(w, r.WithContext(ctx))
			case errors.Is(err, auth.ErrTokenNotFound):
				// Silently drop: the handler's requireScope will
				// return 401 for any endpoint that needs auth.
				next.ServeHTTP(w, r)
			default:
				// A DB error during lookup is operator-observable but
				// shouldn't leak into an auth-failure response. Mark
				// the request unauthenticated and let handlers decide.
				next.ServeHTTP(w, r)
			}
		})
	}
}

// IdentityFromContext returns the identity attached by authMiddleware,
// or (zero, false) if the request was anonymous.
func IdentityFromContext(ctx context.Context) (auth.Identity, bool) {
	v, ok := ctx.Value(ctxKeyIdentity).(auth.Identity)
	return v, ok
}

// requireScope writes a 401 (unauthenticated) or 403 (insufficient
// scope) response and returns false if the request doesn't hold the
// given scope. Handlers use it as:
//
//	if !requireScope(w, r, "publish:"+channel) { return }
//
// The 401 vs 403 distinction is the standard one: 401 means "who are
// you?", 403 means "I know who you are but you can't do this."
func requireScope(w http.ResponseWriter, r *http.Request, required string) bool {
	id, ok := IdentityFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized,
			CodeUnauthorized, "authentication required",
			"supply a valid bearer token in the Authorization header")
		return false
	}
	if !id.Scopes.Has(required) {
		writeError(w, r, http.StatusForbidden,
			CodeInsufficientScope, "token does not grant "+required,
			"issue a new token with the required scope, or use a different token")
		return false
	}
	return true
}
