// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// stubTopics mirrors the coordinator's layout closely enough to assert
// that discovery points at the topics it is given rather than at ones
// it rebuilt itself.
type stubTopics struct{}

func (stubTopics) DeviceTopic(mac model.MAC, key string) string {
	return "unifi/default/device/" + mac.String() + "/" + key
}

func (stubTopics) ClientTopic(key, valueKey string) string {
	return "unifi/default/client/" + key + "/" + valueKey
}
func (stubTopics) HealthTopic(key string) string { return "unifi/default/health/" + key }
func (stubTopics) WLANTopic(id, key string) string {
	return "unifi/default/wlan/" + id + "/" + key
}
func (stubTopics) AvailabilityTopic() string { return "unifi/bridge/status" }

func newTestDiscovery(lang string) *Discovery {
	return New(Config{BaseTopic: "homeassistant", Topics: stubTopics{}, Site: "default", Language: lang})
}

func testDevice() *model.Device {
	return &model.Device{
		MAC:       model.MustParseMAC("00:00:5e:00:53:02"),
		ID:        "id-sw",
		Name:      "Basement Switch",
		Model:     "USW Pro Max 16 PoE",
		Type:      model.DeviceSwitch,
		State:     model.DeviceOnline,
		Firmware:  "7.4.1",
		UplinkMAC: model.MustParseMAC("00:00:5e:00:53:01"),
		Ports: []model.Port{
			{Idx: 1, State: model.PortUp, SpeedMbps: 1000, PoE: &model.PoEState{Enabled: true}},
			{Idx: 2, State: model.PortDown},
		},
		Radios: []model.Radio{
			{FrequencyGHz: 5, Channel: 36},
		},
	}
}

// decode returns the entries keyed by config topic with their payloads
// unmarshalled.
func decode(t *testing.T, entries []Entry) map[string]map[string]any {
	t.Helper()
	out := make(map[string]map[string]any, len(entries))
	for _, e := range entries {
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("payload for %s is not JSON: %v", e.ConfigTopic, err)
		}
		out[e.ConfigTopic] = payload
	}
	return out
}

func TestDeviceEntities(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	byTopic := decode(t, entries)

	want := []string{
		"homeassistant/sensor/unifi_00005e005302/state/config",
		"homeassistant/binary_sensor/unifi_00005e005302/reachable/config",
		"homeassistant/sensor/unifi_00005e005302/uptime/config",
		"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config",
		"homeassistant/sensor/unifi_00005e005302/memory_utilization/config",
		"homeassistant/sensor/unifi_00005e005302/uplink_tx_bps/config",
		"homeassistant/sensor/unifi_00005e005302/uplink_rx_bps/config",
		"homeassistant/sensor/unifi_00005e005302/firmware/config",
		"homeassistant/binary_sensor/unifi_00005e005302/update_available/config",
		// Ports
		"homeassistant/binary_sensor/unifi_00005e005302/port_1_link/config",
		"homeassistant/sensor/unifi_00005e005302/port_1_speed/config",
		"homeassistant/binary_sensor/unifi_00005e005302/port_1_poe/config",
		"homeassistant/binary_sensor/unifi_00005e005302/port_2_link/config",
		// Radio
		"homeassistant/sensor/unifi_00005e005302/radio_5g_channel/config",
		"homeassistant/sensor/unifi_00005e005302/radio_5g_tx_retries/config",
	}
	for _, topic := range want {
		if _, ok := byTopic[topic]; !ok {
			t.Errorf("missing discovery topic %s", topic)
		}
	}

	// A port without PoE hardware must get no PoE entity: the
	// coordinator publishes no such topic, so the entity would sit
	// unavailable forever.
	if _, ok := byTopic["homeassistant/binary_sensor/unifi_00005e005302/port_2_poe/config"]; ok {
		t.Error("a non-PoE port got a PoE entity")
	}
}

// The state topic in a discovery config must be exactly what the
// coordinator publishes to. This is why the layout is injected rather
// than rebuilt here.
func TestStateTopicsComeFromTheInjectedLayout(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	byTopic := decode(t, entries)

	checks := map[string]string{
		"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config":   "unifi/default/device/00005e005302/cpu_utilization",
		"homeassistant/binary_sensor/unifi_00005e005302/port_1_poe/config": "unifi/default/device/00005e005302/port/1/poe",
		"homeassistant/sensor/unifi_00005e005302/radio_5g_channel/config":  "unifi/default/device/00005e005302/radio/5g/channel",
	}
	for configTopic, wantState := range checks {
		e, ok := byTopic[configTopic]
		if !ok {
			t.Errorf("missing %s", configTopic)
			continue
		}
		if got := e["state_topic"]; got != wantState {
			t.Errorf("%s state_topic = %v, want %v", configTopic, got, wantState)
		}
	}
}

// The device block is what groups entities and draws the topology.
func TestDeviceBlock(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	e := decode(t, entries)["homeassistant/sensor/unifi_00005e005302/state/config"]

	dev, ok := e["device"].(map[string]any)
	if !ok {
		t.Fatal("no device block")
	}
	if got := dev["identifiers"].([]any)[0]; got != "unifi_00005e005302" {
		t.Errorf("identifiers = %v, want unifi_00005e005302", got)
	}
	if got := dev["manufacturer"]; got != Manufacturer {
		t.Errorf("manufacturer = %v, want %v", got, Manufacturer)
	}
	if got := dev["sw_version"]; got != "7.4.1" {
		t.Errorf("sw_version = %v", got)
	}
	// via_device is what makes Home Assistant draw client → AP → switch
	// → gateway instead of a flat list.
	if got := dev["via_device"]; got != "unifi_00005e005301" {
		t.Errorf("via_device = %v, want unifi_00005e005301", got)
	}
	// The MAC connection lets Home Assistant merge this device with one
	// discovered by another integration.
	conn := dev["connections"].([]any)[0].([]any)
	if conn[0] != "mac" || conn[1] != "00:00:5e:00:53:02" {
		t.Errorf("connections = %v, want [mac 00:00:5e:00:53:02]", conn)
	}
}

// A gateway has no uplink; advertising an empty via_device would make
// Home Assistant complain about a missing parent device.
func TestGatewayHasNoViaDevice(t *testing.T) {
	t.Parallel()

	dev := testDevice()
	dev.UplinkMAC = ""

	entries, err := newTestDiscovery(LangEN).Device(dev)
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	e := decode(t, entries)["homeassistant/sensor/unifi_00005e005302/state/config"]
	block := e["device"].(map[string]any)
	if _, ok := block["via_device"]; ok {
		t.Errorf("via_device is present for a device with no uplink: %v", block["via_device"])
	}
}

// The core rule of CONCEPT.md §6.2: switching language renames what the
// user sees and nothing else. Anything that changes an identifier would
// re-create the entity and lose its history.
func TestLanguageChangesOnlyTheDisplayName(t *testing.T) {
	t.Parallel()

	en, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device(en): %v", err)
	}
	de, err := newTestDiscovery(LangDE).Device(testDevice())
	if err != nil {
		t.Fatalf("Device(de): %v", err)
	}
	if len(en) != len(de) {
		t.Fatalf("entity count differs by language: %d vs %d", len(en), len(de))
	}

	enByTopic, deByTopic := decode(t, en), decode(t, de)
	var sawTranslation bool

	for topic, enPayload := range enByTopic {
		dePayload, ok := deByTopic[topic]
		if !ok {
			t.Errorf("topic %s exists only in English — the config topic must not be localised", topic)
			continue
		}
		for _, field := range []string{"unique_id", "object_id", "state_topic", "json_attributes_topic"} {
			if enPayload[field] != dePayload[field] {
				t.Errorf("%s: %s differs by language (%v vs %v) — this would orphan the entity",
					topic, field, enPayload[field], dePayload[field])
			}
		}
		if enPayload["name"] != dePayload["name"] {
			sawTranslation = true
		}
	}

	if !sawTranslation {
		t.Error("no entity name was translated — the German table is not being applied")
	}
}

// Identifiers are an interface with recorded history behind them. This
// pins the exact strings so a refactor cannot quietly change them.
func TestIdentifiersAreStable(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangDE).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	byTopic := decode(t, entries)

	// unique_id is the identity Home Assistant tracks an entity by:
	// changing one orphans the entity and drops its history. These
	// strings are an interface with a year of recorded data behind them.
	wantUnique := map[string]string{
		"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config":     "unifi_00005e005302_cpu_utilization",
		"homeassistant/binary_sensor/unifi_00005e005302/port_1_poe/config":   "unifi_00005e005302_port_1_poe",
		"homeassistant/sensor/unifi_00005e005302/radio_5g_tx_retries/config": "unifi_00005e005302_radio_5g_tx_retries",
	}
	for topic, uid := range wantUnique {
		e, ok := byTopic[topic]
		if !ok {
			t.Errorf("missing %s", topic)
			continue
		}
		if got := e["unique_id"]; got != uid {
			t.Errorf("%s unique_id = %v, want %v", topic, got, uid)
		}
	}

	// The entity_id seed is derived from the device name so automations
	// read sensibly. It is MAC-independent on purpose — renaming a
	// device changes the seed but never an existing entity.
	wantSeed := map[string]string{
		"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config":   "basement_switch_cpu_utilization",
		"homeassistant/binary_sensor/unifi_00005e005302/port_1_poe/config": "basement_switch_port_1_poe",
	}
	for topic, seed := range wantSeed {
		e := byTopic[topic]
		if got := e["object_id"]; got != seed {
			t.Errorf("%s object_id = %v, want %v", topic, got, seed)
		}
	}
}

// Both seeds are published because which one works depends on the Home
// Assistant version: object_id was removed in HA Core 2026.4, and
// default_entity_id is not honoured consistently on older releases —
// the release where a localised name leaks into the entity_id instead.
func TestBothEntityIDSeedsArePublished(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangDE).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}

	for topic, payload := range decode(t, entries) {
		obj, hasObj := payload["object_id"].(string)
		def, hasDef := payload["default_entity_id"].(string)
		if !hasObj || obj == "" {
			t.Errorf("%s has no object_id", topic)
			continue
		}
		if !hasDef || def == "" {
			t.Errorf("%s has no default_entity_id — on HA 2026.4+ the localised name would seed the entity_id", topic)
			continue
		}
		// default_entity_id is "<platform>.<seed>" and must agree with
		// object_id, or the two HA versions would disagree about the id.
		platform := strings.Split(topic, "/")[1]
		if want := platform + "." + obj; def != want {
			t.Errorf("%s default_entity_id = %q, want %q", topic, def, want)
		}
	}
}

// The seed must survive a language switch: that is the whole reason for
// setting it. A German display name may not reach the entity_id.
func TestEntityIDSeedIsLanguageIndependent(t *testing.T) {
	t.Parallel()

	en, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device(en): %v", err)
	}
	de, err := newTestDiscovery(LangDE).Device(testDevice())
	if err != nil {
		t.Fatalf("Device(de): %v", err)
	}

	enByTopic, deByTopic := decode(t, en), decode(t, de)
	for topic, e := range enByTopic {
		d := deByTopic[topic]
		for _, field := range []string{"object_id", "default_entity_id"} {
			if e[field] != d[field] {
				t.Errorf("%s: %s differs by language (%v vs %v)", topic, field, e[field], d[field])
			}
		}
		// And nothing German may appear in the seed at all.
		seed, _ := d["object_id"].(string)
		for _, german := range []string{"auslastung", "betriebszeit", "erreichbar", "verfuegbar"} {
			if strings.Contains(seed, german) {
				t.Errorf("%s object_id = %q contains a localised word", topic, seed)
			}
		}
	}
}

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"UniFi-SW-HAR", "unifi_sw_har"},
		{"Basement Switch", "basement_switch"},
		{"USW Pro Max 16 PoE", "usw_pro_max_16_poe"},
		// Umlauts lose the diaeresis rather than expanding it, because
		// that is what Home Assistant's own slugify does: it unidecodes
		// before slugging, so "Süd" is "sud" there and must be here too.
		{"Küche", "kuche"},
		{"Süd", "sud"},
		{"Größe", "grosse"},
		{"Kläranlage", "klaranlage"},
		{"Groß & Klein", "gross_klein"},
		{"  padded  ", "padded"},
		{"a///b", "a_b"},
		{"", ""},
		{"---", ""},
	}
	for _, tt := range tests {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A device whose name already ends in the word the key starts with must
// not stutter it back.
func TestEntityIDSeedCollapsesRepeatedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		device, key, want string
	}{
		{"Switch Garage", "state", "switch_garage_state"},
		{"Garage Switch", "switch_state", "garage_switch_state"},
		{"UniFi-SW-HAR", "cpu_utilization", "unifi_sw_har_cpu_utilization"},
	}
	for _, tt := range tests {
		if got := entityIDSeed(tt.device, tt.key); got != tt.want {
			t.Errorf("entityIDSeed(%q, %q) = %q, want %q", tt.device, tt.key, got, tt.want)
		}
	}
}

// A device with no name still needs a usable seed rather than one
// starting with an underscore.
func TestEntityIDSeedWithoutDeviceName(t *testing.T) {
	t.Parallel()

	if got := entityIDSeed("", "cpu_utilization"); got != "cpu_utilization" {
		t.Errorf("= %q, want the bare key", got)
	}
}

// Two-stage availability: the bridge topic covers the daemon being
// gone, the device state topic covers the device being gone.
func TestAvailability(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	byTopic := decode(t, entries)

	// A statistics sensor should go unavailable when the device does —
	// otherwise it sits there showing a stale CPU reading as current.
	cpu := byTopic["homeassistant/sensor/unifi_00005e005302/cpu_utilization/config"]
	avail := cpu["availability"].([]any)
	if len(avail) != 2 {
		t.Fatalf("cpu availability has %d sources, want 2", len(avail))
	}
	if got := cpu["availability_mode"]; got != "all" {
		t.Errorf("availability_mode = %v, want all", got)
	}
	if got := avail[0].(map[string]any)["topic"]; got != "unifi/bridge/status" {
		t.Errorf("first availability source = %v, want the bridge topic", got)
	}

	// The state sensor is the entity that reports being offline, so
	// making it unavailable when the device is offline would hide
	// exactly the information the user needs.
	state := byTopic["homeassistant/sensor/unifi_00005e005302/state/config"]
	if got := len(state["availability"].([]any)); got != 1 {
		t.Errorf("state sensor has %d availability sources, want 1 (bridge only)", got)
	}
	update := byTopic["homeassistant/binary_sensor/unifi_00005e005302/update_available/config"]
	if got := len(update["availability"].([]any)); got != 1 {
		t.Errorf("update sensor has %d availability sources, want 1 (bridge only)", got)
	}
}

func TestSensorMetadata(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Device(testDevice())
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	byTopic := decode(t, entries)

	tests := []struct {
		topic string
		field string
		want  any
	}{
		{"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config", "unit_of_measurement", "%"},
		{"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config", "state_class", "measurement"},
		{"homeassistant/sensor/unifi_00005e005302/uptime/config", "device_class", "duration"},
		{"homeassistant/sensor/unifi_00005e005302/uptime/config", "unit_of_measurement", "s"},
		{"homeassistant/sensor/unifi_00005e005302/uplink_tx_bps/config", "device_class", "data_rate"},
		{"homeassistant/binary_sensor/unifi_00005e005302/reachable/config", "device_class", "connectivity"},
		{"homeassistant/binary_sensor/unifi_00005e005302/reachable/config", "payload_on", "ONLINE"},
		{"homeassistant/binary_sensor/unifi_00005e005302/update_available/config", "device_class", "update"},
		{"homeassistant/binary_sensor/unifi_00005e005302/port_1_link/config", "payload_on", "UP"},
	}
	for _, tt := range tests {
		e, ok := byTopic[tt.topic]
		if !ok {
			t.Errorf("missing %s", tt.topic)
			continue
		}
		if got := e[tt.field]; got != tt.want {
			t.Errorf("%s %s = %v, want %v", tt.topic, tt.field, got, tt.want)
		}
	}

	// Diagnostic entities should not clutter the main device page.
	for _, topic := range []string{
		"homeassistant/sensor/unifi_00005e005302/cpu_utilization/config",
		"homeassistant/sensor/unifi_00005e005302/uptime/config",
	} {
		if got := byTopic[topic]["entity_category"]; got != "diagnostic" {
			t.Errorf("%s entity_category = %v, want diagnostic", topic, got)
		}
	}
}

// A device with no MAC has no stable identity, so it must produce no
// entities rather than entities keyed on an empty string.
func TestDeviceWithoutMACProducesNothing(t *testing.T) {
	t.Parallel()

	entries, err := newTestDiscovery(LangEN).Device(&model.Device{Name: "Broken"})
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries for a device with no MAC, want 0", len(entries))
	}
}

// An unnamed device still needs a label; without one Home Assistant
// shows a blank row in the device registry.
func TestUnnamedDeviceGetsAFallbackName(t *testing.T) {
	t.Parallel()

	dev := testDevice()
	dev.Name = ""

	entries, err := newTestDiscovery(LangEN).Device(dev)
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	block := decode(t, entries)["homeassistant/sensor/unifi_00005e005302/state/config"]["device"].(map[string]any)
	if got := block["name"].(string); !strings.Contains(got, "00:00:5e:00:53:02") {
		t.Errorf("fallback device name = %q, want it to carry the MAC", got)
	}
}
