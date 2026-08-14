// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package model

import (
	"net/netip"
	"testing"
)

func nets() []Network {
	return []Network{
		{
			ID: "n-lan", Name: "LAN", VLAN: 1, Default: true,
			Management: NetworkGateway,
			Subnets:    []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")},
		},
		{
			ID: "n-iot", Name: "IoT", VLAN: 20,
			Management: NetworkGateway,
			Subnets:    []netip.Prefix{netip.MustParsePrefix("192.168.20.0/24")},
		},
		{
			// Deliberately overlaps n-guest-sub below to exercise the
			// longest-prefix rule.
			ID: "n-wide", Name: "Wide", VLAN: 30,
			Management: NetworkGateway,
			Subnets:    []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		},
		{
			ID: "n-guest-sub", Name: "Guest", VLAN: 40,
			Management: NetworkGateway,
			Subnets:    []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")},
		},
		{
			// UNMANAGED networks carry no subnet and must never match.
			ID: "n-unmanaged", Name: "Unmanaged", VLAN: 50,
			Management: NetworkUnmanaged,
		},
	}
}

func TestResolveNetwork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ip       string
		wantName string
		wantVLAN int
		wantOK   bool
	}{
		{"plain match", "192.168.1.42", "LAN", 1, true},
		{"other vlan", "192.168.20.7", "IoT", 20, true},
		{"network address itself", "192.168.20.0", "IoT", 20, true},
		{"broadcast address", "192.168.1.255", "LAN", 1, true},

		// The /16 is more specific than the /8 and must win, exactly
		// like a routing table would decide it.
		{"longest prefix wins", "10.42.0.9", "Guest", 40, true},
		{"falls back to the wider net", "10.9.0.9", "Wide", 30, true},

		{"no matching network", "172.16.0.1", "", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ResolveNetwork(netip.MustParseAddr(tt.ip), nets())
			if ok != tt.wantOK {
				t.Fatalf("ResolveNetwork(%s) ok = %v, want %v", tt.ip, ok, tt.wantOK)
			}
			if got.Name != tt.wantName || got.VLAN != tt.wantVLAN {
				t.Errorf("ResolveNetwork(%s) = %q/vlan %d, want %q/vlan %d",
					tt.ip, got.Name, got.VLAN, tt.wantName, tt.wantVLAN)
			}
		})
	}
}

// A client with no usable address (the API omits ipAddress for clients
// it has not seen an address for) must not silently land in whichever
// network happens to be listed first.
func TestResolveNetworkInvalidAddr(t *testing.T) {
	t.Parallel()

	if _, ok := ResolveNetwork(netip.Addr{}, nets()); ok {
		t.Error("ResolveNetwork matched the zero address")
	}
}

func TestResolveNetworkEmptyCatalogue(t *testing.T) {
	t.Parallel()

	if _, ok := ResolveNetwork(netip.MustParseAddr("192.168.1.1"), nil); ok {
		t.Error("ResolveNetwork matched against an empty catalogue")
	}
}

func TestResolveUplinks(t *testing.T) {
	t.Parallel()

	gw := MustParseMAC("aa:aa:aa:aa:aa:aa")
	sw := MustParseMAC("bb:bb:bb:bb:bb:bb")
	ap := MustParseMAC("cc:cc:cc:cc:cc:cc")

	devices := []Device{
		{ID: "id-gw", MAC: gw},                    // no uplink: the gateway
		{ID: "id-sw", MAC: sw, UplinkID: "id-gw"}, // switch -> gateway
		{ID: "id-ap", MAC: ap, UplinkID: "id-sw"}, // AP -> switch
		{ID: "id-x", MAC: MustParseMAC("dd:dd:dd:dd:dd:dd"), UplinkID: "id-absent"},
	}

	byID := ResolveUplinks(devices)

	if got := devices[0].UplinkMAC; !got.IsZero() {
		t.Errorf("gateway UplinkMAC = %q, want zero", got)
	}
	if got := devices[1].UplinkMAC; got != gw {
		t.Errorf("switch UplinkMAC = %q, want %q", got, gw)
	}
	if got := devices[2].UplinkMAC; got != sw {
		t.Errorf("AP UplinkMAC = %q, want %q", got, sw)
	}
	// An uplink pointing outside the polled set must leave the MAC zero
	// rather than dropping the device or panicking.
	if got := devices[3].UplinkMAC; !got.IsZero() {
		t.Errorf("dangling uplink resolved to %q, want zero", got)
	}

	if got, want := len(byID), 4; got != want {
		t.Errorf("id map has %d entries, want %d", got, want)
	}
	if got := byID["id-ap"]; got != ap {
		t.Errorf("byID[id-ap] = %q, want %q", got, ap)
	}
}

func TestDeviceTypeFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		features []string
		model    string
		want     DeviceType
	}{
		// Gateways report switching (and often accessPoint too), so the
		// model name is what tells them apart from a plain switch.
		{"dream machine", []string{"switching", "accessPoint"}, "UDM-Pro", DeviceGateway},

		// A live console returns display names with spaces, not the
		// hyphenated product codes the docs use. Matching only the
		// hyphenated form classified every real gateway as a switch.
		{"space-separated gateway", []string{"switching"}, "UCG Fiber", DeviceGateway},
		{"space-separated dream machine", []string{"switching"}, "UDM Pro Max", DeviceGateway},
		{"space-separated switch", []string{"switching"}, "USW Pro Max 16 PoE", DeviceSwitch},
		{"space-separated flex", []string{"switching"}, "USW Flex 2.5G 5", DeviceSwitch},
		{"space-separated AP", []string{"accessPoint"}, "Nano HD", DeviceAccessPoint},
		{"space-separated mesh AP", []string{"accessPoint"}, "AC Mesh", DeviceAccessPoint},
		{"dream machine bare", nil, "UDM", DeviceGateway},
		{"cloud gateway ultra", []string{"switching"}, "UCG-Ultra", DeviceGateway},
		{"express", []string{"switching", "accessPoint"}, "UX", DeviceGateway},
		{"lowercase model", []string{"switching"}, "udr", DeviceGateway},

		{"switch", []string{"switching"}, "USW-Pro-24-PoE", DeviceSwitch},
		{"access point", []string{"accessPoint"}, "U6-Pro", DeviceAccessPoint},
		{"ap that also switches", []string{"switching", "accessPoint"}, "U6-Enterprise-IW", DeviceAccessPoint},
		{"no features", nil, "USP-PDU-Pro", DeviceOther},

		// A model that merely starts with the same letters as a gateway
		// line must not be misread: USW is not USG.
		{"usw is not usg", []string{"switching"}, "USW-24", DeviceSwitch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := DeviceTypeFrom(tt.features, tt.model); got != tt.want {
				t.Errorf("DeviceTypeFrom(%v, %q) = %q, want %q",
					tt.features, tt.model, got, tt.want)
			}
		})
	}
}

func TestClientKey(t *testing.T) {
	t.Parallel()

	mac := MustParseMAC("aa:bb:cc:dd:ee:ff")

	// A wired/wireless client is keyed by MAC so a controller restore
	// (which hands out fresh UUIDs) does not orphan its HA entity.
	if got := (Client{MAC: mac, ID: "uuid-1"}).Key(); got != mac.String() {
		t.Errorf("Key() = %q, want the MAC %q", got, mac)
	}
	// VPN and Teleport clients have no MAC; the UUID is all there is.
	if got := (Client{ID: "uuid-2", Type: ClientVPN}).Key(); got != "uuid-2" {
		t.Errorf("Key() = %q, want the UUID", got)
	}
}
