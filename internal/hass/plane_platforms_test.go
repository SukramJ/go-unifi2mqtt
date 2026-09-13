// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// TestAllPlatformsCoversTheConstBlock ties [allPlatforms] — and through
// it [publishedPlatforms] and [PublishedPlatforms] — to the Platform
// const block it claims to enumerate.
//
// There were two production spellings of these five strings: the const
// block the renderers write into a topic, and a hand-typed
// map[string]bool in plane.go that [OwnsConfigTopic] judged by. PR #29
// tied the *fixture* spelling to PublishedPlatforms and missed this
// pair, which no test referenced at all. A platform added to one and
// not the other is invisible to the orphan sweep in both directions —
// never retracted, never reported — and nothing on the wire says so.
//
// publishedPlatforms is now derived from allPlatforms, so only one
// spelling is left to drift, and this reads the declaration rather than
// transcribing it a third time.
func TestAllPlatformsCoversTheConstBlock(t *testing.T) {
	t.Parallel()

	declared := platformConstValues(t)
	if len(declared) == 0 {
		t.Fatal("no Platform constants found; this test's premise has changed")
	}

	listed := make(map[string]bool, len(allPlatforms))
	for _, p := range allPlatforms {
		listed[string(p)] = true
	}
	for _, v := range declared {
		if !listed[v] {
			t.Errorf("discovery.go declares a Platform %q that allPlatforms does not "+
				"list; OwnsConfigTopic would refuse every retained config on it, so "+
				"the sweep could neither retract nor report those entities", v)
		}
	}
	if len(declared) != len(allPlatforms) {
		t.Errorf("discovery.go declares %d Platform constants, allPlatforms has %d",
			len(declared), len(allPlatforms))
	}

	// The derived set and the exported view must agree with both.
	if len(publishedPlatforms) != len(allPlatforms) {
		t.Errorf("publishedPlatforms has %d entries, allPlatforms %d",
			len(publishedPlatforms), len(allPlatforms))
	}
	for _, v := range declared {
		if !publishedPlatforms[v] {
			t.Errorf("publishedPlatforms does not contain declared platform %q", v)
		}
	}
	want := slices.Clone(declared)
	slices.Sort(want)
	if got := PublishedPlatforms(); !slices.Equal(got, want) {
		t.Errorf("PublishedPlatforms() = %v, want the declared set %v", got, want)
	}
}

// platformConstValues parses discovery.go and returns the literal value
// of every constant declared with the explicit type Platform.
func platformConstValues(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "discovery.go", nil, 0)
	if err != nil {
		t.Fatalf("parse discovery.go: %v", err)
	}

	var out []string
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
			if !ok || ident.Name != "Platform" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s is a Platform constant without a string literal", name.Name)
					continue
				}
				out = append(out, lit.Value[1:len(lit.Value)-1])
			}
		}
	}
	return out
}
