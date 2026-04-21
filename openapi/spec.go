// Package openapi exposes the pakman OpenAPI 3 spec as an embedded
// byte slice so the server can serve it at runtime without depending
// on the filesystem.
//
// openapi.yaml is the source of truth. Keep it alongside this file so
// //go:embed can see it. The internal/api package converts the YAML to
// JSON on startup for the /api/v1/openapi.json route.
package openapi

import _ "embed"

// YAML is the pakman OpenAPI 3.0.3 spec in YAML form. Do NOT mutate.
//
//go:embed openapi.yaml
var YAML []byte
