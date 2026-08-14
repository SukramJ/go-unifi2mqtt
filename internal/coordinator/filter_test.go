// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"log/slog"
	"net/netip"
	"strconv"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

func filterNetworks() []model.Network {
	return []model.Network{
		{ID: "n1", Name: "LAN", VLAN: 1, Subnets: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
		{ID: "n2", Name: "IoT", VLAN: 20, Subnets: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}},
	}
}

func sampleClients() []model.Client {
	return []model.Client{
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:10"), ID: "c1", Name: "Phone",
			Type: model.ClientWireless, IP: netip.MustParseAddr("192.0.2.42"),
		},
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:11"), ID: "c2", Name: "NAS",
			Type: model.ClientWired, IP: netip.MustParseAddr("192.0.2.50"),
		},
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:12"), ID: "c3", Name: "Thermostat",
			Type: model.ClientWireless, IP: netip.MustParseAddr("198.51.100.20"),
		},
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:13"), ID: "c4", Name: "Guest Laptop",
			Type: model.ClientWireless, IP: netip.MustParseAddr("198.51.100.21"), IsGuest: true,
		},
		{
			// No IP: on the reference site 14% of clients look like this.
			MAC: model.MustParseMAC("00:00:5e:00:53:14"), ID: "c5", Name: "Printer",
			Type: model.ClientWired,
		},
		{
			// VPN clients have no MAC at all.
			ID: "c6-uuid", Name: "Remote Worker", Type: model.ClientVPN,
			IP: netip.MustParseAddr("203.0.113.5"),
		},
	}
}

func applyFilter(t *testing.T, cfg config.ClientsConfig) []model.Client {
	t.Helper()
	cfg.Enable = true
	if cfg.Max == 0 {
		cfg.Max = 100
	}
	f := newClientFilter(&cfg, filterNetworks(), slog.New(slog.DiscardHandler))
	return f.Apply(sampleClients())
}

func names(clients []model.Client) []string {
	out := make([]string, 0, len(clients))
	for i := range clients {
		out = append(out, clients[i].Name)
	}
	return out
}

func TestFilterDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  config.ClientsConfig
		want []string
	}{
		{
			name: "no restriction publishes everything",
			cfg:  config.ClientsConfig{},
			want: []string{"Phone", "NAS", "Thermostat", "Guest Laptop", "Printer", "Remote Worker"},
		},
		{
			name: "by type",
			cfg:  config.ClientsConfig{Types: []string{"WIRELESS"}},
			want: []string{"Phone", "Thermostat", "Guest Laptop"},
		},
		{
			name: "type matching is case-insensitive",
			cfg:  config.ClientsConfig{Types: []string{"wireless"}},
			want: []string{"Phone", "Thermostat", "Guest Laptop"},
		},
		{
			name: "by VLAN",
			cfg:  config.ClientsConfig{VLANs: []int{20}},
			want: []string{"Thermostat", "Guest Laptop"},
		},
		{
			name: "by network name",
			cfg:  config.ClientsConfig{Networks: []string{"LAN"}},
			want: []string{"Phone", "NAS"},
		},
		{
			name: "network name matching is case-insensitive",
			cfg:  config.ClientsConfig{Networks: []string{"lan"}},
			want: []string{"Phone", "NAS"},
		},
		{
			// AND across dimensions, OR within one.
			name: "type and VLAN combine",
			cfg:  config.ClientsConfig{Types: []string{"WIRELESS"}, VLANs: []int{20}},
			want: []string{"Thermostat", "Guest Laptop"},
		},
		{
			name: "guests excluded",
			cfg:  config.ClientsConfig{VLANs: []int{20}, ExcludeGuests: true},
			want: []string{"Thermostat"},
		},
		{
			name: "excluded MAC never appears",
			cfg:  config.ClientsConfig{Types: []string{"WIRELESS"}, ExcludeMACs: []string{"00:00:5e:00:53:12"}},
			want: []string{"Phone", "Guest Laptop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := names(applyFilter(t, tt.cfg))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			want := map[string]bool{}
			for _, n := range tt.want {
				want[n] = true
			}
			for _, n := range got {
				if !want[n] {
					t.Errorf("unexpected client %q in %v", n, got)
				}
			}
		})
	}
}

// INCLUDE_MACS is an either/or, not an additive: naming MACs means
// "exactly these", and every other dimension is skipped. Config
// validation warns when both are set.
func TestIncludeMACsBypassesOtherFilters(t *testing.T) {
	t.Parallel()

	got := names(applyFilter(t, config.ClientsConfig{
		// The NAS is wired and on the LAN; both filters would exclude it.
		Types:       []string{"WIRELESS"},
		VLANs:       []int{20},
		IncludeMACs: []string{"00:00:5e:00:53:11"},
	}))
	if len(got) != 1 || got[0] != "NAS" {
		t.Errorf("got %v, want only the allowlisted NAS", got)
	}
}

// EXCLUDE_MACS outranks even the allowlist — it is the escape hatch for
// a device that must always stay invisible.
func TestExcludeBeatsInclude(t *testing.T) {
	t.Parallel()

	got := applyFilter(t, config.ClientsConfig{
		IncludeMACs: []string{"00:00:5e:00:53:11", "00:00:5e:00:53:10"},
		ExcludeMACs: []string{"00:00:5e:00:53:11"},
	})
	if len(got) != 1 || got[0].Name != "Phone" {
		t.Errorf("got %v, want the excluded MAC dropped despite being allowlisted", names(got))
	}
}

// A client that cannot be mapped to a network cannot satisfy a network
// filter. Without a network filter it must still be published.
func TestUnmappableClientsDropOnlyUnderNetworkFilters(t *testing.T) {
	t.Parallel()

	// No network filter: the printer (no IP) survives.
	got := names(applyFilter(t, config.ClientsConfig{Types: []string{"WIRED"}}))
	if len(got) != 2 {
		t.Errorf("got %v, want both wired clients including the one with no IP", got)
	}

	// With a VLAN filter it cannot match and is dropped.
	got = names(applyFilter(t, config.ClientsConfig{Types: []string{"WIRED"}, VLANs: []int{1}}))
	if len(got) != 1 || got[0] != "NAS" {
		t.Errorf("got %v, want only the NAS", got)
	}
}

// Sorting before the cap is what keeps the published set stable: the
// API's ordering changes between polls, so an unsorted cap would make
// Home Assistant see entities appear and disappear continuously.
func TestMaxCapIsStableAcrossReorderedInput(t *testing.T) {
	t.Parallel()

	cfg := config.ClientsConfig{Enable: true, Max: 3}
	f := newClientFilter(&cfg, filterNetworks(), slog.New(slog.DiscardHandler))

	forward := f.Apply(sampleClients())

	reversed := sampleClients()
	slicesReverse(reversed)
	backward := f.Apply(reversed)

	if len(forward) != 3 || len(backward) != 3 {
		t.Fatalf("cap not applied: %d / %d", len(forward), len(backward))
	}
	for i := range forward {
		if forward[i].Key() != backward[i].Key() {
			t.Fatalf("the capped set depends on input order:\n  %v\n  %v",
				names(forward), names(backward))
		}
	}
}

func slicesReverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func TestDisabledPublishesNothing(t *testing.T) {
	t.Parallel()

	cfg := config.ClientsConfig{Enable: false, Max: 100}
	f := newClientFilter(&cfg, filterNetworks(), slog.New(slog.DiscardHandler))
	if got := f.Apply(sampleClients()); len(got) != 0 {
		t.Errorf("got %d clients with CLIENTS.ENABLE off, want 0", len(got))
	}
}

// The filter resolves the network so a VLAN filter and the published
// attributes agree on the same value.
func TestFilterResolvesNetwork(t *testing.T) {
	t.Parallel()

	got := applyFilter(t, config.ClientsConfig{Types: []string{"WIRELESS"}})
	for i := range got {
		c := &got[i]
		switch c.Name {
		case "Phone":
			if c.Network != "LAN" || c.VLAN != 1 {
				t.Errorf("Phone resolved to %q/vlan %d, want LAN/1", c.Network, c.VLAN)
			}
		case "Thermostat":
			if c.Network != "IoT" || c.VLAN != 20 {
				t.Errorf("Thermostat resolved to %q/vlan %d, want IoT/20", c.Network, c.VLAN)
			}
		}
	}
}

// A VPN client has no MAC, so it is keyed by UUID. It must not be
// dropped by a MAC-based filter it cannot possibly match.
func TestClientsWithoutMACAreKeyedByID(t *testing.T) {
	t.Parallel()

	got := applyFilter(t, config.ClientsConfig{Types: []string{"VPN"}})
	if len(got) != 1 {
		t.Fatalf("got %v, want the VPN client", names(got))
	}
	if got[0].Key() != "c6-uuid" {
		t.Errorf("Key() = %q, want the UUID", got[0].Key())
	}

	// An exclude list of real MACs must not accidentally match it.
	got = applyFilter(t, config.ClientsConfig{
		Types: []string{"VPN"}, ExcludeMACs: []string{"00:00:5e:00:53:10"},
	})
	if len(got) != 1 {
		t.Errorf("the VPN client was dropped by an unrelated MAC exclusion")
	}
}

func TestCapWarnsRatherThanTruncatingSilently(t *testing.T) {
	t.Parallel()

	var buf logCapture
	cfg := config.ClientsConfig{Enable: true, Max: 2}
	f := newClientFilter(&cfg, filterNetworks(), slog.New(&buf))

	if got := f.Apply(sampleClients()); len(got) != 2 {
		t.Fatalf("got %d clients, want the cap of 2", len(got))
	}
	if !buf.contains("clients_capped") {
		t.Error("truncation was silent; an operator would never know entities are missing")
	}
	if !buf.contains(strconv.Itoa(len(sampleClients()))) {
		t.Error("the warning does not say how many clients matched")
	}
}
