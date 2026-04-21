// Package version exposes build-time metadata for the pakman binary.
//
// Version is set at build time via ldflags:
//
//	go build -ldflags "-X github.com/schochastics/pakman/internal/version.Version=v1.0.0"
package version

// Version is the build version. Overridden at release time via -ldflags.
var Version = "dev"
