// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"context"
	"errors"
	"testing"
)

// A signal landing during startup surfaces as context.Canceled. Exiting
// 1 for that would make systemd record a clean `systemctl stop` as a
// unit failure.
func TestFatalErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not fatal", nil, false},
		{"cancellation is not fatal", context.Canceled, false},
		{"wrapped cancellation is not fatal", errors.Join(errors.New("shutting down"), context.Canceled), false},
		{"a real failure is fatal", errors.New("boom"), true},
		{"a deadline is fatal", context.DeadlineExceeded, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := fatalErr(tt.err); got != tt.want {
				t.Errorf("fatalErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestParseMajorMinor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in          string
		major       int
		minor       int
		ok          bool
		wantUntestd bool
	}{
		{in: "10.5.67", major: 10, minor: 5, ok: true},
		{in: "10.6.90", major: 10, minor: 6, ok: true},
		{in: "11.0.0", major: 11, minor: 0, ok: true},
		{in: "10.4.99", major: 10, minor: 4, ok: true, wantUntestd: true},
		{in: "9.3.45", major: 9, minor: 3, ok: true, wantUntestd: true},
		{in: "10.5", major: 10, minor: 5, ok: true},
		// A version string the console might grow later must degrade to
		// "unknown", not to "too old".
		{in: "10", ok: false},
		{in: "", ok: false},
		{in: "not.a.version", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			major, minor, ok := parseMajorMinor(tt.in)
			if ok != tt.ok {
				t.Fatalf("parseMajorMinor(%q) ok = %v, want %v", tt.in, ok, tt.ok)
			}
			if !ok {
				return
			}
			if major != tt.major || minor != tt.minor {
				t.Errorf("parseMajorMinor(%q) = %d.%d, want %d.%d", tt.in, major, minor, tt.major, tt.minor)
			}

			// Mirrors warnOldVersion's comparison, which is the decision
			// the parse exists to feed.
			untested := major < minAppMajor || (major == minAppMajor && minor < minAppMinor)
			if untested != tt.wantUntestd {
				t.Errorf("version %q untested = %v, want %v", tt.in, untested, tt.wantUntestd)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"much too long a name", 10, "much too …"},
		{"abc", 1, "a"},
		{"", 5, ""},
	}

	for _, tt := range tests {
		if got := truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}
