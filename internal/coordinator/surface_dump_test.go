// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// Builder-output dump (ADR 0070 phase 9, step 2).
//
// The golden files are written by the very builders they guard, so
// diffing two checkouts' *fixtures* proves only that the regeneration
// ran. What has to be compared is the builders' live output at two
// refs.
//
// This harness writes exactly that: the canonical encoding of every
// scenario, to a directory outside the repository, without touching
// testdata/ and without consulting it. Copy this one file into a
// checkout of the other ref, run it there, and diff the two
// directories key by key.
//
//	go test ./internal/coordinator -run TestDumpSurface \
//	    -dump-surface-dir /tmp/before
//
// It is a no-op — and reports itself skipped — when the flag is unset,
// so it costs a normal run nothing. Step 4's byte-equality proof uses
// the same harness against the library-rendered output, which is why
// it is committed rather than thrown away.
var dumpSurfaceDir = flag.String("dump-surface-dir", "",
	"write each scenario's canonical builder output to this directory and skip every assertion")

func TestDumpSurface(t *testing.T) {
	t.Parallel()

	if *dumpSurfaceDir == "" {
		t.Skip("set -dump-surface-dir to write the builder output")
	}
	if err := os.MkdirAll(*dumpSurfaceDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, sc := range surfaceScenarios() {
		path := filepath.Join(*dumpSurfaceDir, sc.name+".json")
		if err := os.WriteFile(path, canonical(t, buildSurface(t, sc)), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
	}
}
