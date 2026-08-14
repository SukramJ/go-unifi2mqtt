// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"cmp"
	"log/slog"
	"slices"
	"strings"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Client filtering.
//
// This is mandatory machinery, not a convenience: the reference
// installation has 121 clients, and publishing all of them would create
// several hundred Home Assistant entities on first start
// (CONCEPT.md §6.3).
//
// The evaluation order is fixed and each rule exists for a reason:
//
//  1. EXCLUDE_MACS      always wins — the "never this device" escape
//  2. INCLUDE_MACS      when set, ONLY these, every other filter skipped
//  3. EXCLUDE_GUESTS    guests are transient by nature
//  4. TYPES/NETWORKS/   AND across dimensions, OR within one;
//     VLANS/SSIDS       an empty list means "no restriction here"
//  5. MAX               hard cap, applied after sorting by key

// clientFilter decides which clients are published.
type clientFilter struct {
	cfg      *config.ClientsConfig
	log      *slog.Logger
	networks []model.Network

	// Normalised lookups, built once per poll rather than per client:
	// a 121-client site would otherwise re-lowercase the same config
	// lists 121 times.
	include  map[model.MAC]bool
	exclude  map[model.MAC]bool
	types    map[model.ClientType]bool
	networkN map[string]bool
	vlans    map[int]bool
	ssids    map[string]bool
}

func newClientFilter(cfg *config.ClientsConfig, networks []model.Network, log *slog.Logger) *clientFilter {
	f := &clientFilter{cfg: cfg, log: log, networks: networks}

	f.include = macSet(cfg.IncludeMACs)
	f.exclude = macSet(cfg.ExcludeMACs)

	f.types = make(map[model.ClientType]bool, len(cfg.Types))
	for _, t := range cfg.Types {
		f.types[model.ClientType(strings.ToUpper(strings.TrimSpace(t)))] = true
	}
	f.networkN = lowerSet(cfg.Networks)
	f.ssids = lowerSet(cfg.SSIDs)
	f.vlans = make(map[int]bool, len(cfg.VLANs))
	for _, v := range cfg.VLANs {
		f.vlans[v] = true
	}
	return f
}

func macSet(list []string) map[model.MAC]bool {
	out := make(map[model.MAC]bool, len(list))
	for _, raw := range list {
		// Validation already rejected malformed entries at startup, so a
		// parse failure here can only mean the config changed underneath
		// us; skipping is the safe reading.
		if m, err := model.ParseMAC(raw); err == nil && !m.IsZero() {
			out[m] = true
		}
	}
	return out
}

func lowerSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

// Apply returns the clients to publish, with Network and VLAN resolved.
//
// Resolution happens here rather than in the client package because it
// needs the network catalogue, and because a filter on VLAN has to see
// the same value the published attributes will show.
func (f *clientFilter) Apply(clients []model.Client) []model.Client {
	if !f.cfg.Enable {
		return nil
	}

	kept := make([]model.Client, 0, min(len(clients), f.cfg.Max))
	var unmapped int

	for i := range clients {
		c := clients[i]
		f.resolveNetwork(&c)

		keep, reason := f.keep(&c)
		if !keep {
			if reason == reasonUnmapped {
				unmapped++
			}
			continue
		}
		kept = append(kept, c)
	}

	// Sorting before the cap is what keeps the published set stable.
	// Without it the API's ordering would decide which 50 of 80 clients
	// get entities, and that ordering changes between polls — Home
	// Assistant would see entities appear and disappear continuously.
	slices.SortFunc(kept, func(a, b model.Client) int {
		return cmp.Compare(a.Key(), b.Key())
	})

	if unmapped > 0 {
		f.log.Debug("coordinator.clients_unmapped",
			slog.Int("count", unmapped),
			slog.String("note", "no IP or no matching network; dropped because a network filter is set"))
	}

	if len(kept) > f.cfg.Max {
		f.log.Warn("coordinator.clients_capped",
			slog.Int("matched", len(kept)),
			slog.Int("published", f.cfg.Max),
			slog.String("note", "raise CLIENTS.MAX or narrow the filters"))
		kept = kept[:f.cfg.Max]
	}
	return kept
}

// resolveNetwork fills in Network and VLAN from the catalogue. This is
// what makes VLAN filtering work on the official API alone, since the
// client payload carries no VLAN of its own (CONCEPT.md §2.2).
func (f *clientFilter) resolveNetwork(c *model.Client) {
	if n, ok := model.ResolveNetwork(c.IP, f.networks); ok {
		c.Network = n.Name
		c.VLAN = n.VLAN
	}
}

// dropReason distinguishes the one rejection worth counting from the
// rest, which are the filter working as configured.
type dropReason int

const (
	reasonFiltered dropReason = iota
	reasonUnmapped
)

func (f *clientFilter) keep(c *model.Client) (bool, dropReason) {
	// 1. The explicit "never" list wins over everything, including the
	//    allowlist — it is the escape hatch for a device you always want
	//    invisible.
	if !c.MAC.IsZero() && f.exclude[c.MAC] {
		return false, reasonFiltered
	}

	// 2. An allowlist is an either/or, not an additive: naming MACs
	//    means "exactly these", and the other dimensions are skipped.
	//    Config validation warns when both are set.
	if len(f.include) > 0 {
		return !c.MAC.IsZero() && f.include[c.MAC], reasonFiltered
	}

	if f.cfg.ExcludeGuests && c.IsGuest {
		return false, reasonFiltered
	}

	if len(f.types) > 0 && !f.types[c.Type] {
		return false, reasonFiltered
	}

	// A client that could not be mapped to a network cannot satisfy a
	// network filter. On the reference site 14% of clients have no IP at
	// all, so this is a common case rather than an edge one — it is
	// counted and reported once per cycle instead of logged per client.
	wantsNetwork := len(f.networkN) > 0 || len(f.vlans) > 0
	if wantsNetwork && c.Network == "" {
		return false, reasonUnmapped
	}
	if len(f.networkN) > 0 && !f.networkN[strings.ToLower(c.Network)] {
		return false, reasonFiltered
	}
	if len(f.vlans) > 0 && !f.vlans[c.VLAN] {
		return false, reasonFiltered
	}

	// SSID needs the classic layer; config validation rejects this
	// filter without it, so an empty SSID here means the client is
	// wired rather than that the data is missing.
	if len(f.ssids) > 0 && !f.ssids[strings.ToLower(c.SSID)] {
		return false, reasonFiltered
	}

	return true, reasonFiltered
}
