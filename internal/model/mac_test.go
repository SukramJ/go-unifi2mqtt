// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package model

import (
	"errors"
	"testing"
)

func TestParseMAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    MAC
		wantErr error
	}{
		{"colon separated", "aa:bb:cc:dd:ee:ff", "aabbccddeeff", nil},
		{"uppercase colon", "AA:BB:CC:DD:EE:FF", "aabbccddeeff", nil},
		{"hyphen separated", "aa-bb-cc-dd-ee-ff", "aabbccddeeff", nil},
		{"cisco dotted", "aabb.ccdd.eeff", "aabbccddeeff", nil},
		{"already bare", "aabbccddeeff", "aabbccddeeff", nil},
		{"mixed case bare", "AaBbCcDdEeFf", "aabbccddeeff", nil},
		{"digits", "00:11:22:33:44:55", "001122334455", nil},

		// An absent MAC is legitimate: VPN and Teleport clients have none.
		{"empty is the zero MAC", "", "", nil},

		{"too short", "aa:bb:cc:dd:ee", "", ErrInvalidMAC},
		{"too long", "aa:bb:cc:dd:ee:ff:00", "", ErrInvalidMAC},
		{"non-hex letter", "gg:bb:cc:dd:ee:ff", "", ErrInvalidMAC},
		{"embedded space", "aa bb cc dd ee ff", "", ErrInvalidMAC},
		{"separators only", "::::::", "", ErrInvalidMAC},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseMAC(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseMAC(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseMAC(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// Every notation of the same address must collapse to one value —
// that is the whole point of normalising at the parse boundary, since
// the two API flavours format MACs differently.
func TestParseMACNormalisesToOneValue(t *testing.T) {
	t.Parallel()

	forms := []string{
		"aa:bb:cc:dd:ee:ff",
		"AA:BB:CC:DD:EE:FF",
		"aa-bb-cc-dd-ee-ff",
		"aabb.ccdd.eeff",
		"AABBCCDDEEFF",
	}

	first := MustParseMAC(forms[0])
	for _, f := range forms[1:] {
		if got := MustParseMAC(f); got != first {
			t.Errorf("MustParseMAC(%q) = %q, want %q", f, got, first)
		}
	}
}

func TestMACFormatting(t *testing.T) {
	t.Parallel()

	m := MustParseMAC("AA:BB:CC:DD:EE:FF")

	if got, want := m.String(), "aabbccddeeff"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := m.Colon(), "aa:bb:cc:dd:ee:ff"; got != want {
		t.Errorf("Colon() = %q, want %q", got, want)
	}
	if m.IsZero() {
		t.Error("IsZero() = true for a populated MAC")
	}
}

func TestZeroMAC(t *testing.T) {
	t.Parallel()

	var m MAC
	if !m.IsZero() {
		t.Error("IsZero() = false for the zero value")
	}
	if got := m.Colon(); got != "" {
		t.Errorf("Colon() = %q for the zero MAC, want empty", got)
	}
	if got := m.String(); got != "" {
		t.Errorf("String() = %q for the zero MAC, want empty", got)
	}
}

func TestMustParseMACPanicsOnInvalid(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("MustParseMAC did not panic on invalid input")
		}
	}()
	_ = MustParseMAC("nope")
}
