// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package model

import (
	"net/netip"
	"strings"
)

// ResolveNetwork maps an IP address onto the site's network catalogue
// and reports whether a match was found.
//
// This is what makes VLAN and network filtering work on the official
// API alone: the Integration API never tells us a client's VLAN, but it
// does tell us the client's IP and the subnet of every network
// (CONCEPT.md §2.2).
//
// Overlapping subnets are resolved longest-prefix-first, the same rule
// a routing table uses, so a /24 carved out of a /16 wins for addresses
// inside it. An invalid or unset address never matches.
func ResolveNetwork(ip netip.Addr, networks []Network) (Network, bool) {
	if !ip.IsValid() {
		return Network{}, false
	}

	var (
		best     Network
		bestBits = -1
		found    bool
	)
	for _, n := range networks {
		for _, p := range n.Subnets {
			if !p.IsValid() || !p.Contains(ip) {
				continue
			}
			if p.Bits() > bestBits {
				best, bestBits, found = n, p.Bits(), true
			}
		}
	}
	return best, found
}

// ResolveUplinks fills in every device's UplinkMAC from its UplinkID by
// looking the UUID up in the same device slice, and returns the ID→MAC
// map so callers can resolve client uplinks with it too.
//
// The API reports uplinks as device UUIDs while the whole project is
// keyed on MACs, so this translation has to happen exactly once per
// poll — doing it per entity would rebuild the map N times
// (CONCEPT.md §3.4).
//
// Devices whose uplink is not in the slice (an unadopted or filtered
// device) keep a zero UplinkMAC rather than being dropped.
func ResolveUplinks(devices []Device) map[string]MAC {
	byID := make(map[string]MAC, len(devices))
	for i := range devices {
		if devices[i].ID != "" {
			byID[devices[i].ID] = devices[i].MAC
		}
	}
	for i := range devices {
		if id := devices[i].UplinkID; id != "" {
			devices[i].UplinkMAC = byID[id]
		}
	}
	return byID
}

// DeviceTypeFrom classifies a device from the feature keys the console
// reports. A gateway is recognised by model prefix because the API
// exposes no "routing" feature — gateways report switching and, on the
// all-in-one models, accessPoint as well, which would otherwise make
// them indistinguishable from a plain switch.
func DeviceTypeFrom(features []string, deviceModel string) DeviceType {
	if isGatewayModel(deviceModel) {
		return DeviceGateway
	}

	var switching, ap bool
	for _, f := range features {
		switch f {
		case "switching":
			switching = true
		case "accessPoint":
			ap = true
		}
	}

	switch {
	case ap:
		return DeviceAccessPoint
	case switching:
		return DeviceSwitch
	default:
		return DeviceOther
	}
}

// gatewayModels lists the product-line tokens UniFi uses for consoles
// and security gateways.
//
// Matching is on the model name's first token so every member of a line
// is covered without a code change. The token is taken with both space
// and hyphen as separators because the API returns display names, not
// product codes: a live console reports "UCG Fiber" and "USW Pro Max 16
// PoE", while the documentation writes "UCG-Fiber". Matching only the
// hyphenated form classified every gateway as a switch.
var gatewayModels = map[string]bool{
	"UDM": true, // Dream Machine / Pro / SE / Max
	"UDR": true, // Dream Router
	"UDW": true, // Dream Wall
	"UCG": true, // Cloud Gateway (Ultra / Max / Fiber)
	"UXG": true, // Next-Gen Gateway
	"USG": true, // legacy Security Gateway
	"UX":  true, // Express
	"EFG": true, // Enterprise Fortress Gateway
}

func isGatewayModel(m string) bool {
	return gatewayModels[modelToken(m)]
}

// modelToken returns the leading product-line token of a model name,
// upper-cased: "UCG Fiber" and "UCG-Fiber" both yield "UCG".
func modelToken(m string) string {
	u := strings.ToUpper(strings.TrimSpace(m))
	if i := strings.IndexAny(u, " -_"); i >= 0 {
		return u[:i]
	}
	return u
}
