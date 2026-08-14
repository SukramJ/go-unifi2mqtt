// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package version

import (
	"strings"
	"testing"
)

// The build banner is asserted by the CI `build` job (`unifi2mqtt
// --version`) and by the add-on log line, so keep the shape stable:
// program name first, then the three ldflags-injected fields.
func TestStringContainsBuildMetadata(t *testing.T) {
	t.Parallel()

	got := String()
	for _, want := range []string{"go-unifi2mqtt", Version, Commit, BuildDate} {
		if want == "" {
			t.Fatal("build metadata must not be empty")
		}
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}
