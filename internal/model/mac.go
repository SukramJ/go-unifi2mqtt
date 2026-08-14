// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package model

import (
	"errors"
	"strings"
)

// ErrInvalidMAC is returned by ParseMAC for input that is not a 48-bit
// MAC address in any of the accepted notations.
var ErrInvalidMAC = errors.New("model: invalid MAC address")

// MAC is a normalised 48-bit hardware address: lowercase hexadecimal,
// no separators (e.g. "aabbccddeeff").
//
// It is a distinct type rather than a string so a raw address straight
// out of an API response cannot be written into an MQTT topic or a Home
// Assistant unique_id by accident. [ParseMAC] is the only constructor —
// normalisation happens there and nowhere else, which is what makes the
// MAC usable as the project's canonical identity (see CONCEPT.md §3.4).
//
// The zero value is the empty MAC, which VPN and Teleport clients
// legitimately have; check it with [MAC.IsZero].
type MAC string

// ParseMAC normalises a hardware address from any notation UniFi's two
// API flavours use: colon-separated ("aa:bb:cc:dd:ee:ff"),
// hyphen-separated, dot-separated Cisco style ("aabb.ccdd.eeff") or
// already bare. Case is irrelevant.
//
// An empty input yields the zero MAC and no error: the API omits the
// field for client types that have no hardware address, and that is not
// a parse failure.
func ParseMAC(s string) (MAC, error) {
	if s == "" {
		return "", nil
	}

	var b strings.Builder
	b.Grow(12)
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			b.WriteByte(c)
		case c >= 'A' && c <= 'F':
			b.WriteByte(c + ('a' - 'A'))
		case c == ':' || c == '-' || c == '.':
			// separator, skip
		default:
			return "", ErrInvalidMAC
		}
	}

	if b.Len() != 12 {
		return "", ErrInvalidMAC
	}
	return MAC(b.String()), nil
}

// MustParseMAC is ParseMAC for compile-time-known input, panicking on
// failure. Only for tests and package-level variables.
func MustParseMAC(s string) MAC {
	m, err := ParseMAC(s)
	if err != nil {
		panic("model: MustParseMAC(" + s + "): " + err.Error())
	}
	return m
}

// String returns the topic form: bare lowercase hex, no separators.
// This is what goes into MQTT topics and unique_ids.
func (m MAC) String() string { return string(m) }

// Colon returns the display form "aa:bb:cc:dd:ee:ff", used for Home
// Assistant's device `connections` field and for human-facing output.
// The zero MAC returns an empty string.
func (m MAC) Colon() string {
	if len(m) != 12 {
		return ""
	}
	var b strings.Builder
	b.Grow(17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(string(m[i : i+2]))
	}
	return b.String()
}

// IsZero reports whether the MAC is unset.
func (m MAC) IsZero() bool { return m == "" }
