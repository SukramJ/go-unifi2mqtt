// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package version exposes build metadata injected at link time via
// -ldflags. The defaults (Version=dev / Commit=none / BuildDate=
// unknown) keep `go run` and unit tests working without a Makefile;
// release builds overwrite them.
package version

// Build-time information. Populated by the Makefile / Dockerfile with
// `-X github.com/SukramJ/go-unifi2mqtt/internal/version.Version=...` etc.
//
// The in-source defaults must match the latest release tag so a
// `go install` / `go run` build (which has no -ldflags) still reports
// a sensible version. Bump this whenever a new tag is cut.
var (
	// Version is the human-readable release tag (e.g. "1.0.0").
	Version = "1.1.0"
	// Commit is the short Git SHA the binary was built from.
	Commit = "none"
	// BuildDate is the UTC RFC3339 build timestamp.
	BuildDate = "unknown"
)

// String returns a compact one-line build banner.
func String() string {
	return "go-unifi2mqtt " + Version + " (commit " + Commit + ", built " + BuildDate + ")"
}
