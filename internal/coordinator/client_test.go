// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// clientConfig enables clients with an explicit away timeout.
func clientConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(`
HOST: 192.0.2.1
API_KEY: k
MQTT_SERVER: broker
MQTT_TOPIC: unifi
HASS_ENABLE: true
REFRESH_CLIENTS: 30
CLIENTS:
  ENABLE: true
  TYPES: []
  AWAY_TIMEOUT: 300
`+extra), config.MapEnv{})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func withClients(t *testing.T, h *harness) {
	t.Helper()
	h.src.mu.Lock()
	h.src.clients = []model.Client{
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:10"), ID: "c1", Name: "Phone",
			Type: model.ClientWireless, IP: netip.MustParseAddr("192.0.2.42"),
			UplinkID: "id-ap", ConnectedAt: time.Date(2026, 8, 14, 7, 0, 0, 0, time.UTC),
		},
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:11"), ID: "c2", Name: "NAS",
			Type: model.ClientWired, IP: netip.MustParseAddr("192.0.2.50"),
			UplinkID: "id-sw",
		},
	}
	h.src.mu.Unlock()
}

func TestClientPresencePublished(t *testing.T) {
	t.Parallel()

	h := newHarness(t, clientConfig(t, ""))
	withClients(t, h)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	if got, _ := h.broker.latest("unifi/default/client/00005e005310/state"); got != "home" {
		t.Errorf("presence = %q, want home", got)
	}
	if got, _ := h.broker.latest("unifi/default/client/00005e005310/ip"); got != "192.0.2.42" {
		t.Errorf("ip = %q, want 192.0.2.42", got)
	}

	raw, ok := h.broker.latest("unifi/default/client/00005e005310/attributes")
	if !ok {
		t.Fatal("no attributes topic")
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if got := attrs["network"]; got != "LAN" {
		t.Errorf("network = %v, want LAN — the VLAN mapping did not run", got)
	}
	// The uplink UUID must be resolved to a MAC, exactly like devices,
	// so via_device can point at the access point.
	if got := attrs["uplink_mac"]; got != "00:00:5e:00:53:03" {
		t.Errorf("uplink_mac = %v, want the AP's MAC", got)
	}
}

// Wireless clients vanish for a cycle while roaming between access
// points. Flipping to not_home on the first missed poll makes every
// presence automation flap.
func TestPresenceSurvivesAMissedPoll(t *testing.T) {
	t.Parallel()

	h := newHarness(t, clientConfig(t, ""))
	withClients(t, h)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	// The phone drops out while roaming.
	h.src.mu.Lock()
	h.src.clients = h.src.clients[1:]
	h.src.mu.Unlock()

	h.broker.reset()
	h.clock.advance(30 * time.Second)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	if got, ok := h.broker.latest("unifi/default/client/00005e005310/state"); ok {
		t.Errorf("presence flipped to %q after one missed poll, want it held at home", got)
	}

	// Still inside the grace period.
	h.clock.advance(4 * time.Minute)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	if got, ok := h.broker.latest("unifi/default/client/00005e005310/state"); ok {
		t.Errorf("presence flipped to %q before AWAY_TIMEOUT, want it held", got)
	}

	// Past it: now the client is genuinely away.
	h.clock.advance(2 * time.Minute)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	if got, _ := h.broker.latest("unifi/default/client/00005e005310/state"); got != "not_home" {
		t.Errorf("presence = %q after the grace period, want not_home", got)
	}
}

// A client that returns during the grace period must never have gone
// away at all — no not_home, no entity churn.
func TestReturningClientNeverGoesAway(t *testing.T) {
	t.Parallel()

	h := newHarness(t, clientConfig(t, ""))
	withClients(t, h)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	full := h.src.clients
	h.src.mu.Lock()
	h.src.clients = full[1:]
	h.src.mu.Unlock()

	h.clock.advance(2 * time.Minute)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	h.src.mu.Lock()
	h.src.clients = full
	h.src.mu.Unlock()

	h.broker.reset()
	h.clock.advance(30 * time.Second)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()
	for _, m := range h.broker.msgs {
		if strings.HasSuffix(m.topic, "client/00005e005310/state") && m.payload == "not_home" {
			t.Error("a client that returned within the grace period was still marked away")
		}
	}
}

// An away client's IP is stale by definition; publishing the last known
// one suggests it is still reachable there.
func TestAwayClientDropsItsIP(t *testing.T) {
	t.Parallel()

	h := newHarness(t, clientConfig(t, ""))
	withClients(t, h)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	h.src.mu.Lock()
	h.src.clients = nil
	h.src.mu.Unlock()

	h.clock.advance(10 * time.Minute)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	if got, _ := h.broker.latest("unifi/default/client/00005e005310/ip"); got != "" {
		t.Errorf("ip = %q for an away client, want it cleared", got)
	}
	// The tracker itself must stay: a device_tracker that disappears
	// makes automations referencing it error out, rather than simply
	// seeing "away".
	if got, _ := h.broker.latest("unifi/default/client/00005e005310/state"); got != "not_home" {
		t.Errorf("state = %q, want not_home", got)
	}
}

func TestClientDiscoveryAnnouncedOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t, clientConfig(t, ""))
	withClients(t, h)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	tracker := "homeassistant/device_tracker/unifi_client_00005e005310/presence/config"
	if got := h.broker.count(tracker); got != 1 {
		t.Fatalf("tracker config published %d times, want 1", got)
	}

	raw, _ := h.broker.latest(tracker)
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("tracker config: %v", err)
	}
	if got := payload["source_type"]; got != "router" {
		t.Errorf("source_type = %v, want router", got)
	}
	if got := payload["payload_home"]; got != "home" {
		t.Errorf("payload_home = %v, want home", got)
	}
	// via_device continues the topology: gateway → switch → AP → phone.
	dev := payload["device"].(map[string]any)
	if got := dev["via_device"]; got != "unifi_00005e005303" {
		t.Errorf("via_device = %v, want the AP", got)
	}

	// A second poll must not re-announce.
	h.broker.reset()
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	if got := h.broker.count(tracker); got != 0 {
		t.Errorf("tracker config republished %d times on an unchanged poll", got)
	}
}

// Clients are off by default, and the loop must publish nothing at all
// in that case — not even an empty topic tree.
func TestClientsDisabledPublishesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil) // default config has CLIENTS.ENABLE off
	withClients(t, h)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	if got := h.broker.topicsWithPrefix("unifi/default/client/"); len(got) != 0 {
		t.Errorf("published %d client topics with CLIENTS.ENABLE off", len(got))
	}
	if got := h.src.callCount("Clients"); got != 0 {
		t.Errorf("polled clients %d times with the feature off", got)
	}
}

// A filtered-out client must never reach the broker, including its
// discovery config — that is the whole point of filtering before
// publishing rather than after.
func TestFilteredClientIsNeverAnnounced(t *testing.T) {
	t.Parallel()

	h := newHarness(t, clientConfig(t, "  EXCLUDE_MACS: [\"00:00:5e:00:53:11\"]\n"))
	withClients(t, h)
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}

	for topic := range h.broker.topicsWithPrefix("") {
		if strings.Contains(topic, "00005e005311") {
			t.Errorf("an excluded client reached the broker: %s", topic)
		}
	}
}
