// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"strings"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

func allControls() ControlOptions {
	return ControlOptions{
		DeviceRestart: true, PortPowerCycle: true, DeviceLocate: true,
		ClientBlock: true, GuestAuthorize: true, WLANEnable: true,
	}
}

func TestDeviceControls(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).DeviceControls(testDevice(), allControls())
	if err != nil {
		t.Fatalf("DeviceControls: %v", err)
	}
	byTopic := decode(t, entries)

	restart := byTopic["homeassistant/button/unifi_00005e005302/restart/config"]
	if restart == nil {
		t.Fatal("no restart button")
	}
	if got := restart["command_topic"]; got != "unifi/default/device/00005e005302/cmd/restart" {
		t.Errorf("command_topic = %v", got)
	}
	if got := restart["payload_press"]; got != "PRESS" {
		t.Errorf("payload_press = %v", got)
	}

	locate := byTopic["homeassistant/switch/unifi_00005e005302/locate/config"]
	if locate == nil {
		t.Fatal("no locate switch")
	}
	if got := locate["state_topic"]; got != "unifi/default/device/00005e005302/locate" {
		t.Errorf("state_topic = %v", got)
	}
	if got := locate["command_topic"]; got != "unifi/default/device/00005e005302/cmd/locate/set" {
		t.Errorf("command_topic = %v", got)
	}

	// Port 1 has PoE, port 2 does not — and a port with no PoE has
	// nothing to power-cycle.
	if _, ok := byTopic["homeassistant/button/unifi_00005e005302/port_1_power_cycle/config"]; !ok {
		t.Error("no power-cycle button for the PoE port")
	}
	if _, ok := byTopic["homeassistant/button/unifi_00005e005302/port_2_power_cycle/config"]; ok {
		t.Error("a non-PoE port got a power-cycle button")
	}
}

// Nothing may be optimistic: the state comes back from the console
// after the follow-up poll, so a failed command snaps the entity back
// rather than leaving it lying about what happened.
//
// Absent is how that is expressed. Home Assistant's MQTT switch turns
// optimistic mode on only when no state_topic is given, and every
// switch here gives one, so an omitted key and an explicit false are
// the same entity — while an explicit false on a button is a key that
// platform does not declare at all. What this test refuses is the
// value that would change behaviour.
func TestControlsAreNeverOptimistic(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).DeviceControls(testDevice(), allControls())
	if err != nil {
		t.Fatalf("DeviceControls: %v", err)
	}
	controls := 0
	for topic, payload := range decode(t, entries) {
		controls++
		// Absent, not false: the key is a pointer nothing sets, so an
		// optimistic control cannot be published by accident, and a
		// deliberate one would have to state the value — at which point
		// this test says so.
		if got, ok := payload["optimistic"]; ok {
			t.Errorf("%s states optimistic = %v; the platform default is "+
				"already what this bridge wants", topic, got)
		}
	}
	if controls == 0 {
		t.Fatal("no control configs rendered; the test asserts nothing")
	}
}

// TestButtonsCarryOnlyKeysTheButtonSchemaDeclares is the regression for
// F2 of notes/adr0070-phase9-measurement.md.
//
// Every one of this bridge's button payloads used to carry
// "state_topic": "" and "optimistic": false, neither of which the MQTT
// button schema declares. Home Assistant drops both silently — its
// discovery schemas are extra=REMOVE_EXTRA — so the buttons worked and
// nothing said otherwise. A validator reading the payload as a document
// refuses it, and once a device's entities are published as one bundle
// a refusal costs that device every entity it has, not the button.
func TestButtonsCarryOnlyKeysTheButtonSchemaDeclares(t *testing.T) {
	t.Parallel()

	// Keys the MQTT button schema does not declare, so Home Assistant
	// drops them on arrival and a document validator refuses them.
	notOnButton := []string{"state_topic", "optimistic"}

	entries, err := newTestDiscovery(LangEN).DeviceControls(testDevice(), allControls())
	if err != nil {
		t.Fatalf("DeviceControls: %v", err)
	}
	buttons := 0
	for topic, payload := range decode(t, entries) {
		if !strings.HasPrefix(topic, "homeassistant/button/") {
			continue
		}
		buttons++
		for _, key := range notOnButton {
			if v, ok := payload[key]; ok {
				t.Errorf("%s carries %q = %#v, which platform button does not declare",
					topic, key, v)
			}
		}
		// The keys it does need must still be there, so the fix cannot
		// pass by emptying the payload.
		for _, key := range []string{"command_topic", "payload_press", "unique_id"} {
			if _, ok := payload[key]; !ok {
				t.Errorf("%s is missing %q", topic, key)
			}
		}
	}
	if buttons == 0 {
		t.Fatal("no button configs rendered; the test asserts nothing")
	}
}

// Each control is individually switchable, so an operator can allow
// restarts without allowing anything else.
func TestControlsAreIndividuallyGated(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).DeviceControls(
		testDevice(), ControlOptions{DeviceRestart: true},
	)
	if err != nil {
		t.Fatalf("DeviceControls: %v", err)
	}
	byTopic := decode(t, entries)

	if len(byTopic) != 1 {
		t.Fatalf("got %d entities, want only the restart button: %v", len(byTopic), byTopic)
	}
	if _, ok := byTopic["homeassistant/button/unifi_00005e005302/restart/config"]; !ok {
		t.Error("the enabled control is missing")
	}
}

func TestNoControlsProducesNothing(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).DeviceControls(testDevice(), ControlOptions{})
	if err != nil {
		t.Fatalf("DeviceControls: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entities with every control off", len(entries))
	}
}

func TestClientControls(t *testing.T) {
	t.Parallel()

	cl := testClient()
	entries, err := newTestDiscovery(LangEN).ClientControls(cl, allControls())
	if err != nil {
		t.Fatalf("ClientControls: %v", err)
	}
	byTopic := decode(t, entries)

	blocked := byTopic["homeassistant/switch/unifi_client_00005e005310/blocked/config"]
	if blocked == nil {
		t.Fatal("no blocked switch")
	}
	if got := blocked["command_topic"]; got != "unifi/default/client/00005e005310/blocked/set" {
		t.Errorf("command_topic = %v", got)
	}

	// The authorize button only makes sense for a guest.
	if _, ok := byTopic["homeassistant/button/unifi_client_00005e005310/authorize/config"]; ok {
		t.Error("a non-guest client got an authorize button")
	}

	cl.IsGuest = true
	entries, err = newTestDiscovery(LangEN).ClientControls(cl, allControls())
	if err != nil {
		t.Fatalf("ClientControls: %v", err)
	}
	if _, ok := decode(t, entries)["homeassistant/button/unifi_client_00005e005310/authorize/config"]; !ok {
		t.Error("a guest client got no authorize button")
	}
}

// Blocking is keyed by MAC on the wire, so a client without one cannot
// be blocked — offering the switch would produce an entity that errors
// on click.
func TestClientWithoutMACGetsNoBlockSwitch(t *testing.T) {
	t.Parallel()

	cl := &model.Client{ID: "vpn-uuid", Name: "Remote", Type: model.ClientVPN}
	entries, err := newTestDiscovery(LangEN).ClientControls(cl, allControls())
	if err != nil {
		t.Fatalf("ClientControls: %v", err)
	}
	for topic := range decode(t, entries) {
		if strings.Contains(topic, "blocked") {
			t.Errorf("a client with no MAC got a block switch: %s", topic)
		}
	}
}

func TestWLANControl(t *testing.T) {
	t.Parallel()

	w := &model.WLAN{ID: "w-1", Name: "HomeNet", Enabled: true}
	entry, err := newTestDiscovery(LangEN).WLANControl(w)
	if err != nil {
		t.Fatalf("WLANControl: %v", err)
	}
	payload := decode(t, []Entry{entry})[entry.ConfigTopic]

	// The SSID is the useful label; a page of entities all called
	// "Enabled" would be useless.
	if got := payload["name"]; got != "HomeNet" {
		t.Errorf("name = %v, want the SSID", got)
	}
	if got := payload["command_topic"]; got != "unifi/default/wlan/w-1/enabled/set" {
		t.Errorf("command_topic = %v", got)
	}
	// WLANs belong to the site, not to any single access point.
	dev := payload["device"].(map[string]any)
	if got := dev["identifiers"].([]any)[0]; got != "unifi_site_default" {
		t.Errorf("identifiers = %v, want the site device", got)
	}
}

// Same rule as everywhere: language changes the display name only.
func TestControlLanguageChangesOnlyTheDisplayName(t *testing.T) {
	t.Parallel()

	en, err := newTestDiscovery(LangEN).DeviceControls(testDevice(), allControls())
	if err != nil {
		t.Fatalf("DeviceControls(en): %v", err)
	}
	de, err := newTestDiscovery(LangDE).DeviceControls(testDevice(), allControls())
	if err != nil {
		t.Fatalf("DeviceControls(de): %v", err)
	}

	enByTopic, deByTopic := decode(t, en), decode(t, de)
	var translated bool
	for topic, enPayload := range enByTopic {
		dePayload, ok := deByTopic[topic]
		if !ok {
			t.Errorf("topic %s exists only in English", topic)
			continue
		}
		for _, field := range []string{"unique_id", "default_entity_id", "command_topic"} {
			if enPayload[field] != dePayload[field] {
				t.Errorf("%s: %s differs by language — this would orphan the entity", topic, field)
			}
		}
		if enPayload["name"] != dePayload["name"] {
			translated = true
		}
	}
	if !translated {
		t.Error("no control name was translated")
	}
}

// The port number belongs in the display name, not only in the key —
// otherwise a 24-port switch shows 24 identical entity names.
func TestPortButtonNamesCarryTheirNumber(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangDE).DeviceControls(testDevice(), allControls())
	if err != nil {
		t.Fatalf("DeviceControls: %v", err)
	}
	payload := decode(t, entries)["homeassistant/button/unifi_00005e005302/port_1_power_cycle/config"]
	if payload == nil {
		t.Fatal("no port button")
	}
	if got := payload["name"].(string); !strings.Contains(got, "1") {
		t.Errorf("name = %q, want it to name the port", got)
	}
}
