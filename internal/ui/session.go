// Package ui serves the operator dashboard at /ui/ — a server-rendered
// HTML interface for inspecting channels, packages, events, cells, and
// storage. No JavaScript framework, no client build step; everything
// renders server-side from html/template.
//
// Auth is a signed session cookie that wraps the operator's bearer
// token. The cookie value is "<b64(token)>.<b64(hmac_sha256(token))>"
// so the server can verify it without looking up state.
package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// sessionCookieName is the cookie that carries the signed bearer token.
// The name is deliberately distinct from anything else so an existing
// "auth" or "token" cookie from another service on the same host can't
// collide.
const sessionCookieName = "pakman_ui"

// defaultSessionTTL is how long an operator stays logged in before the
// UI prompts for a token again. Tokens themselves can be rotated or
// revoked independently of this TTL.
const defaultSessionTTL = 24 * time.Hour

// errInvalidSession is returned by verifySessionCookie for any form of
// tampered, truncated, or wrong-key cookie. Callers convert this to a
// redirect to /ui/login.
var errInvalidSession = errors.New("ui: invalid session cookie")

// signSessionCookie produces the cookie value for plaintextToken using
// key. Format: "<b64url(token)>.<b64url(hmac_sha256(token, key))>".
// Both halves are base64url without padding so the cookie fits
// straightforwardly into a Set-Cookie header.
func signSessionCookie(plaintextToken string, key []byte) string {
	enc := base64.RawURLEncoding
	tokB := enc.EncodeToString([]byte(plaintextToken))

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(plaintextToken))
	sig := enc.EncodeToString(mac.Sum(nil))

	return tokB + "." + sig
}

// verifySessionCookie extracts the plaintext token from a signed cookie
// value. Returns errInvalidSession on any form of tampering.
// Constant-time compare on the HMAC half so this isn't a timing side
// channel.
func verifySessionCookie(value string, key []byte) (string, error) {
	i := strings.IndexByte(value, '.')
	if i < 0 {
		return "", errInvalidSession
	}
	enc := base64.RawURLEncoding

	tokenBytes, err := enc.DecodeString(value[:i])
	if err != nil {
		return "", errInvalidSession
	}
	got, err := enc.DecodeString(value[i+1:])
	if err != nil {
		return "", errInvalidSession
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(tokenBytes)
	want := mac.Sum(nil)

	if !hmac.Equal(got, want) {
		return "", errInvalidSession
	}
	return string(tokenBytes), nil
}
