// Package version exposes build-time metadata for the packyard binary.
//
// Version is set at build time via ldflags:
//
//	go build -ldflags "-X github.com/schochastics/packyard/internal/version.Version=v1.0.0"
package version

// Version is the build version. Overridden at release time via -ldflags.
var Version = "dev"
