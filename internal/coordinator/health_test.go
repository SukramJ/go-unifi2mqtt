// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

func healthConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(`
HOST: 192.0.2.1
API_KEY: k
MQTT_SERVER: broker
MQTT_TOPIC: unifi
HASS_ENABLE: true
CLASSIC_ENABLE: true
CLASSIC_USERNAME: admin
CLASSIC_PASSWORD: pw
`), config.MapEnv{})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func sampleHealth() model.Health {
	latency, uptime := 12, int64(864000)
	return model.Health{
		WAN:        model.SubsystemHealth{Status: "ok"},
		LAN:        model.SubsystemHealth{Status: "ok"},
		WLAN:       model.SubsystemHealth{Status: "warning"},
		VPN:        model.SubsystemHealth{Status: "unknown"},
		WANIP:      netip.MustParseAddr("203.0.113.7"),
		LatencyMs:  &latency,
		UptimeSec:  &uptime,
		RxBps:      18400000,
		TxBps:      2100000,
		NumUser:    40,
		NumGuest:   3,
		NumIoT:     17,
		NumAP:      3,
		NumSwitch:  8,
		NumGateway: 1,
	}
}

func TestHealthPublished(t *testing.T) {
	t.Parallel()

	h := newHarness(t, healthConfig(t))
	h.src.health = sampleHealth()

	if err := h.c.refreshHealth(t.Context()); err != nil {
		t.Fatalf("refreshHealth: %v", err)
	}

	want := map[string]string{
		"unifi/default/health/wan/state":      "ok",
		"unifi/default/health/wan/ip":         "203.0.113.7",
		"unifi/default/health/wan/latency_ms": "12",
		"unifi/default/health/wan/rx_bps":     "18400000",
		"unifi/default/health/wlan/state":     "warning",
		"unifi/default/health/vpn/state":      "unknown",
		// user + guest + iot
		"unifi/default/health/clients/total": "60",
		"unifi/default/health/clients/guest": "3",
	}
	for topic, wantPayload := range want {
		got, ok := h.broker.latest(topic)
		if !ok {
			t.Errorf("topic %s was never published", topic)
			continue
		}
		if got != wantPayload {
			t.Errorf("%s = %q, want %q", topic, got, wantPayload)
		}
	}

	// A controller that reports no latency at all must publish an empty
	// value, not a measured 0 — the reference console does exactly this.
	noLatency := sampleHealth()
	noLatency.LatencyMs = nil
	h2 := newHarness(t, healthConfig(t))
	h2.src.health = noLatency
	if err := h2.c.refreshHealth(t.Context()); err != nil {
		t.Fatalf("refreshHealth: %v", err)
	}
	if got, _ := h2.broker.latest("unifi/default/health/wan/latency_ms"); got != "" {
		t.Errorf("latency = %q with no data, want empty so HA shows unknown", got)
	}

	raw, ok := h.broker.latest("unifi/default/health/attributes")
	if !ok {
		t.Fatal("no health attributes")
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if got := attrs["access_points"]; got != float64(3) {
		t.Errorf("access_points = %v, want 3", got)
	}
}

// Without the classic layer this endpoint does not exist. That is a
// configuration choice, not a failure — the loop must stay quiet rather
// than warning on every tick.
func TestHealthWithoutClassicLayerIsQuiet(t *testing.T) {
	t.Parallel()

	h := newHarness(t, healthConfig(t))
	h.src.healthErr = unifi.ErrCapabilityUnavailable

	if err := h.c.refreshHealth(t.Context()); err != nil {
		t.Errorf("refreshHealth returned %v, want nil for an unavailable capability", err)
	}
	if got := h.broker.topicsWithPrefix("unifi/default/health/"); len(got) != 0 {
		t.Errorf("published %d health topics without the classic layer", len(got))
	}
	// No entities either: they would point at topics that never receive
	// a value and sit unavailable forever.
	if got := h.broker.topicsWithPrefix("homeassistant/sensor/unifi_site_"); len(got) != 0 {
		t.Errorf("announced %d health entities without the classic layer", len(got))
	}
}

// Health discovery waits until the classic layer has actually answered.
func TestHealthDiscoveryAnnouncedOnFirstSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t, healthConfig(t))
	h.src.health = sampleHealth()

	if err := h.c.refreshHealth(t.Context()); err != nil {
		t.Fatalf("refreshHealth: %v", err)
	}

	configs := h.broker.topicsWithPrefix("homeassistant/")
	wantTopics := []string{
		"homeassistant/binary_sensor/unifi_site_default/wan_connectivity/config",
		"homeassistant/sensor/unifi_site_default/wan_latency/config",
		"homeassistant/sensor/unifi_site_default/clients_total/config",
	}
	for _, topic := range wantTopics {
		if !configs[topic] {
			t.Errorf("missing health entity %s", topic)
		}
	}

	// Announced once, not on every poll.
	h.broker.reset()
	if err := h.c.refreshHealth(t.Context()); err != nil {
		t.Fatalf("refreshHealth: %v", err)
	}
	if got := h.broker.topicsWithPrefix("homeassistant/"); len(got) != 0 {
		t.Errorf("re-announced %d health entities on a second poll", len(got))
	}
}

// Health entities belong to a synthetic site device, not to the
// gateway: WAN latency is a property of the site, and hanging it off
// the gateway would make it vanish when the gateway is swapped.
func TestHealthEntitiesBelongToASiteDevice(t *testing.T) {
	t.Parallel()

	h := newHarness(t, healthConfig(t))
	h.src.health = sampleHealth()
	if err := h.c.refreshHealth(t.Context()); err != nil {
		t.Fatalf("refreshHealth: %v", err)
	}

	raw, ok := h.broker.latest("homeassistant/sensor/unifi_site_default/wan_latency/config")
	if !ok {
		t.Fatal("no WAN latency entity")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	dev := payload["device"].(map[string]any)
	if got := dev["identifiers"].([]any)[0]; got != "unifi_site_default" {
		t.Errorf("identifiers = %v, want the site device", got)
	}
	if _, ok := dev["via_device"]; ok {
		t.Error("the site device has a via_device; it is the root of the tree")
	}
}
