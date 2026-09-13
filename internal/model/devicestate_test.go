// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// TestAllDeviceStatesCoversTheConstBlock ties [allDeviceStates] to the
// const block it claims to enumerate, by reading the declaration rather
// than a second transcription of it.
//
// The transcription this replaces is the reason it exists.
// discovery_test.go's TestEveryBinarySensorCanReportBothStates carried
// the comment "The complete vocabulary … Transcribed from the
// publishing side: model.DeviceState" over *ten* values while this
// block declared eleven: DeviceGettingReady was missing, and
// integration.toDeviceState puts it on the wire. The test existed to
// prove that the `reachable` value_template maps the whole domain onto
// exactly two payloads, and it was proving that over an incomplete
// domain — safe only because the template's else branch happens to
// absorb the eleventh. A totality proof that does not know the totality
// is the worst kind of green.
//
// So the vocabulary is now *derived*: consumers call
// [AllDeviceStates], and this test fails if a twelfth constant is
// declared without being added to it. There is no hand-maintained
// second copy left to forget.
func TestAllDeviceStatesCoversTheConstBlock(t *testing.T) {
	t.Parallel()

	declared := deviceStateConstNames(t)
	if len(declared) == 0 {
		t.Fatal("no DeviceState constants found; this test's premise has changed")
	}

	listed := make(map[string]bool, len(allDeviceStates))
	for _, s := range allDeviceStates {
		listed[string(s)] = true
	}

	for _, name := range declared {
		if !listed[name.value] {
			t.Errorf("model.go declares %s = %q but allDeviceStates does not list it; "+
				"every consumer that must handle the whole vocabulary reads "+
				"AllDeviceStates, so a value missing here is a value silently "+
				"excluded from their totality proofs", name.ident, name.value)
		}
	}
	if len(declared) != len(allDeviceStates) {
		t.Errorf("model.go declares %d DeviceState constants but allDeviceStates has %d entries",
			len(declared), len(allDeviceStates))
	}

	// Duplicates would let the count agree while a value is missing.
	if len(listed) != len(allDeviceStates) {
		t.Errorf("allDeviceStates has %d entries but only %d distinct values",
			len(allDeviceStates), len(listed))
	}
}

// constDecl is one typed string constant as the source declares it.
type constDecl struct {
	ident string // Go identifier, e.g. "DeviceGettingReady"
	value string // literal wire value, e.g. "GETTING_READY"
}

// deviceStateConstNames parses model.go and returns every constant
// declared with the explicit type DeviceState.
func deviceStateConstNames(t *testing.T) []constDecl {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "model.go", nil, 0)
	if err != nil {
		t.Fatalf("parse model.go: %v", err)
	}

	var out []constDecl
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "DeviceState" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s is a DeviceState constant without a string literal; "+
						"this test can no longer read the vocabulary", name.Name)
					continue
				}
				out = append(out, constDecl{
					ident: name.Name,
					value: lit.Value[1 : len(lit.Value)-1],
				})
			}
		}
	}
	slices.SortFunc(out, func(a, b constDecl) int {
		switch {
		case a.value < b.value:
			return -1
		case a.value > b.value:
			return 1
		default:
			return 0
		}
	})
	return out
}
