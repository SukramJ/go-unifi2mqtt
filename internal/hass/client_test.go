// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

func testClient() *model.Client {
	return &model.Client{
		MAC:       model.MustParseMAC("00:00:5e:00:53:10"),
		ID:        "c1",
		Name:      "Phone",
		Type:      model.ClientWireless,
		IP:        netip.MustParseAddr("192.0.2.42"),
		UplinkMAC: model.MustParseMAC("00:00:5e:00:53:03"),
		Network:   "LAN",
		VLAN:      1,
	}
}

func TestClientEntities(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Client(testClient(), ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	byTopic := decode(t, entries)

	tracker := "homeassistant/device_tracker/unifi_client_00005e005310/presence/config"
	e, ok := byTopic[tracker]
	if !ok {
		t.Fatalf("no device_tracker config; got %v", byTopic)
	}

	// source_type "router" is what makes this usable for the person
	// integration without a GPS location.
	if got := e["source_type"]; got != "router" {
		t.Errorf("source_type = %v, want router", got)
	}
	if got := e["payload_home"]; got != "home" {
		t.Errorf("payload_home = %v, want home", got)
	}
	if got := e["state_topic"]; got != "unifi/default/client/00005e005310/state" {
		t.Errorf("state_topic = %v", got)
	}

	if _, ok := byTopic["homeassistant/sensor/unifi_client_00005e005310/ip/config"]; !ok {
		t.Error("no IP sensor")
	}

	// Signal strength and the blocked switch need the classic layer;
	// announcing them without it would create entities that stay
	// unavailable forever.
	for topic := range byTopic {
		if strings.Contains(topic, "signal") || strings.Contains(topic, "blocked") {
			t.Errorf("announced a classic-only entity: %s", topic)
		}
	}
}

// With the classic layer the signal sensor appears — but only for
// wireless clients, where the value means something.
func TestSignalSensorOnlyForWirelessWithClassic(t *testing.T) {
	t.Parallel()

	signalTopic := "homeassistant/sensor/unifi_client_00005e005310/signal/config"

	entries, err := newTestDiscovery(LangEN).Client(testClient(), ClientOptions{Signal: true})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	e, ok := decode(t, entries)[signalTopic]
	if !ok {
		t.Fatal("no signal sensor for a wireless client with the capability on")
	}
	if got := e["device_class"]; got != "signal_strength" {
		t.Errorf("device_class = %v, want signal_strength", got)
	}
	if got := e["unit_of_measurement"]; got != "dBm" {
		t.Errorf("unit = %v, want dBm", got)
	}

	// A wired client has no meaningful signal strength.
	wired := testClient()
	wired.Type = model.ClientWired
	entries, err = newTestDiscovery(LangEN).Client(wired, ClientOptions{Signal: true})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if _, ok := decode(t, entries)[signalTopic]; ok {
		t.Error("a wired client got a signal sensor")
	}
}

// via_device continues the topology started by the infrastructure
// devices: gateway → switch → AP → phone.
func TestClientViaDevicePointsAtItsUplink(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Client(testClient(), ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	e := decode(t, entries)["homeassistant/device_tracker/unifi_client_00005e005310/presence/config"]
	dev := e["device"].(map[string]any)

	if got := dev["via_device"]; got != "unifi_00005e005303" {
		t.Errorf("via_device = %v, want the AP's device id", got)
	}
	// The client device id is namespaced separately, so a UniFi device
	// that also shows up as a client cannot collide with itself.
	if got := dev["identifiers"].([]any)[0]; got != "unifi_client_00005e005310" {
		t.Errorf("identifiers = %v, want the client-namespaced id", got)
	}
}

// A VPN client has no uplink and no MAC; it must still produce a
// tracker, keyed by its UUID and standing alone in the registry.
func TestClientWithoutMACOrUplink(t *testing.T) {
	t.Parallel()

	cl := &model.Client{ID: "vpn-uuid", Name: "Remote Worker", Type: model.ClientVPN}
	entries, err := newTestDiscovery(LangEN).Client(cl, ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}

	tracker := "homeassistant/device_tracker/unifi_client_vpn-uuid/presence/config"
	e, ok := decode(t, entries)[tracker]
	if !ok {
		t.Fatalf("no tracker for a VPN client; got %v", decode(t, entries))
	}
	dev := e["device"].(map[string]any)
	if _, ok := dev["via_device"]; ok {
		t.Error("a client with no uplink advertises via_device")
	}
	if _, ok := dev["connections"]; ok {
		t.Error("a client with no MAC advertises a MAC connection")
	}
}

func TestClientWithoutKeyProducesNothing(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Client(&model.Client{Name: "Nameless"}, ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries for a client with no identity, want 0", len(entries))
	}
}

// The tracker reports being away, so its availability must depend only
// on the bridge — not on the client being present, which would make it
// unavailable exactly when it should say not_home.
func TestTrackerStaysAvailableWhenTheClientIsAway(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Client(testClient(), ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	byTopic := decode(t, entries)

	tracker := byTopic["homeassistant/device_tracker/unifi_client_00005e005310/presence/config"]
	if got := len(tracker["availability"].([]any)); got != 1 {
		t.Errorf("tracker has %d availability sources, want 1 (bridge only)", got)
	}

	// The IP sensor, by contrast, is stale once the client is away.
	ip := byTopic["homeassistant/sensor/unifi_client_00005e005310/ip/config"]
	if got := len(ip["availability"].([]any)); got != 2 {
		t.Errorf("IP sensor has %d availability sources, want 2", got)
	}
}

// Same rule as for devices: language changes the display name only.
func TestClientLanguageChangesOnlyTheDisplayName(t *testing.T) {
	t.Parallel()

	en, err := newTestDiscovery(LangEN).Client(testClient(), ClientOptions{})
	if err != nil {
		t.Fatalf("Client(en): %v", err)
	}
	de, err := newTestDiscovery(LangDE).Client(testClient(), ClientOptions{})
	if err != nil {
		t.Fatalf("Client(de): %v", err)
	}

	enByTopic, deByTopic := decode(t, en), decode(t, de)
	var translated bool
	for topic, enPayload := range enByTopic {
		dePayload, ok := deByTopic[topic]
		if !ok {
			t.Errorf("topic %s exists only in English", topic)
			continue
		}
		for _, field := range []string{"unique_id", "object_id", "state_topic"} {
			if enPayload[field] != dePayload[field] {
				t.Errorf("%s: %s differs by language — this would orphan the entity", topic, field)
			}
		}
		if enPayload["name"] != dePayload["name"] {
			translated = true
		}
	}
	if !translated {
		t.Error("no client entity name was translated")
	}
}

func TestClientIdentifiersAreStable(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangDE).Client(testClient(), ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	byTopic := decode(t, entries)

	want := map[string]string{
		"homeassistant/device_tracker/unifi_client_00005e005310/presence/config": "unifi_client_00005e005310_presence",
		"homeassistant/sensor/unifi_client_00005e005310/ip/config":               "unifi_client_00005e005310_client_ip",
	}
	for topic, uid := range want {
		e, ok := byTopic[topic]
		if !ok {
			t.Errorf("missing %s", topic)
			continue
		}
		if got := e["unique_id"]; got != uid {
			t.Errorf("%s unique_id = %v, want %v", topic, got, uid)
		}
	}
}

func TestUnnamedClientGetsAFallbackName(t *testing.T) {
	t.Parallel()

	cl := testClient()
	cl.Name = ""

	entries, err := newTestDiscovery(LangEN).Client(cl, ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	e := decode(t, entries)["homeassistant/device_tracker/unifi_client_00005e005310/presence/config"]
	if got := e["device"].(map[string]any)["name"].(string); !strings.Contains(got, "00005e005310") {
		t.Errorf("fallback name = %q, want it to carry the key", got)
	}
}
