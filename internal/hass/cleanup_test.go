// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// allEntries collects one of every kind of entry this package produces,
// so the invariants below are checked against the real payloads rather
// than hand-written ones that could drift from them.
func allEntries(t *testing.T) []Entry {
	t.Helper()
	d := newTestDiscovery(LangEN)
	opts := ControlOptions{
		DeviceRestart: true, PortPowerCycle: true, GuestAuthorize: true,
		DeviceLocate: true, ClientBlock: true, WLANEnable: true,
	}

	var out []Entry
	dev := testDevice()
	for _, fn := range []func() ([]Entry, error){
		func() ([]Entry, error) { return d.Device(dev) },
		func() ([]Entry, error) { return d.DeviceControls(dev, opts) },
		func() ([]Entry, error) { return d.Client(testClient(), ClientOptions{Signal: true}) },
		func() ([]Entry, error) { return d.ClientControls(testClient(), opts) },
		func() ([]Entry, error) { return d.Health("default") },
	} {
		entries, err := fn()
		if err != nil {
			t.Fatalf("building entries: %v", err)
		}
		out = append(out, entries...)
	}

	wlan, err := d.WLANControl(&model.WLAN{ID: "w1", Name: "HomeNet", Enabled: true})
	if err != nil {
		t.Fatalf("WLANControl: %v", err)
	}
	return append(out, wlan)
}

// The reconcile identifies its own configs by the bridge availability
// topic, so an entry without one would be invisible to the sweep and
// linger forever. Pinning it here is cheaper than discovering it as a
// stale entity in a live install.
func TestEveryEntryCarriesTheBridgeAvailabilityTopic(t *testing.T) {
	t.Parallel()

	d := newTestDiscovery(LangEN)
	want := stubTopics{}.AvailabilityTopic()

	for _, e := range allEntries(t) {
		if !d.IsOwnConfig(e.Payload) {
			t.Errorf("%s: not recognised as own config — availability must include %q",
				e.ConfigTopic, want)
		}
	}
}

// Every config topic has to match the filter the reconcile subscribes
// to; one that does not is never read back and can never be cleared.
func TestConfigFilterMatchesEveryConfigTopic(t *testing.T) {
	t.Parallel()

	filter := newTestDiscovery(LangEN).ConfigFilter()
	for _, e := range allEntries(t) {
		if !topicMatchesFilter(e.ConfigTopic, filter) {
			t.Errorf("%s does not match filter %s", e.ConfigTopic, filter)
		}
	}
}

// topicMatchesFilter implements the single-level MQTT wildcard, which is
// all ConfigFilter uses.
func topicMatchesFilter(topic, filter string) bool {
	tp := strings.Split(topic, "/")
	fp := strings.Split(filter, "/")
	if len(tp) != len(fp) {
		return false
	}
	for i := range fp {
		if fp[i] != "+" && fp[i] != tp[i] {
			return false
		}
	}
	return true
}

// Ownership needs both signals. A config from another integration that
// happens to use our id prefix, and one of ours pointed at a different
// MQTT root, must both read as foreign — clearing either would delete
// somebody else's entity.
func TestIsOwnConfigRequiresBothSignals(t *testing.T) {
	t.Parallel()

	d := newTestDiscovery(LangEN)
	ours := stubTopics{}.AvailabilityTopic()

	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "ours",
			payload: `{"unique_id":"unifi_00005e005302_state","availability":[{"topic":"` + ours + `"}]}`,
			want:    true,
		},
		{
			name:    "our prefix, another bridge's root",
			payload: `{"unique_id":"unifi_00005e005302_state","availability":[{"topic":"other/bridge/status"}]}`,
			want:    false,
		},
		{
			name:    "our availability topic, another integration's id",
			payload: `{"unique_id":"zigbee_00005e005302_state","availability":[{"topic":"` + ours + `"}]}`,
			want:    false,
		},
		{
			name:    "no availability at all",
			payload: `{"unique_id":"unifi_00005e005302_state"}`,
			want:    false,
		},
		{
			name:    "not JSON",
			payload: `online`,
			want:    false,
		},
		{
			name:    "empty",
			payload: ``,
			want:    false,
		},
	}
	for _, tt := range tests {
		if got := d.IsOwnConfig([]byte(tt.payload)); got != tt.want {
			t.Errorf("%s: IsOwnConfig = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The class decides which readiness gate applies, so a misclassified id
// is swept on the wrong signal.
func TestClassOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		uid  string
		want Class
	}{
		{"unifi_00005e005302_state", ClassDevice},
		{"unifi_00005e005302_port_3_poe", ClassDevice},
		{"unifi_client_00005e005399_presence", ClassClient},
		{"unifi_site_default_wan_state", ClassSite},
		{"unifi_wlan_w1_enabled", ClassWLAN},
		// Not ours, or not something this version knows about. Both must
		// be left alone rather than guessed at.
		{"zigbee_light_1", ClassUnknown},
		{"unifi_", ClassUnknown},
		{"unifi_something", ClassUnknown},
		{"unifi_notamac_state", ClassUnknown},
		{"", ClassUnknown},
	}
	for _, tt := range tests {
		if got := ClassOf(tt.uid); got != tt.want {
			t.Errorf("ClassOf(%q) = %v, want %v", tt.uid, got, tt.want)
		}
	}
}

// The classes really are those of the ids this package emits — a test
// that only checks hand-written strings would pass while the real
// scheme drifted.
func TestClassOfCoversEveryEmittedID(t *testing.T) {
	t.Parallel()

	for _, e := range allEntries(t) {
		var cfg struct {
			UniqueID string `json:"unique_id"`
		}
		if err := json.Unmarshal(e.Payload, &cfg); err != nil {
			t.Fatalf("%s: %v", e.ConfigTopic, err)
		}
		if ClassOf(cfg.UniqueID) == ClassUnknown {
			t.Errorf("%s: unique_id %q classifies as unknown, so it would never be swept",
				e.ConfigTopic, cfg.UniqueID)
		}
	}
}

func TestOrphanConfigs(t *testing.T) {
	t.Parallel()

	d := newTestDiscovery(LangEN)
	ours := stubTopics{}.AvailabilityTopic()
	own := func(uid string) []byte {
		return []byte(`{"unique_id":"` + uid + `","availability":[{"topic":"` + ours + `"}]}`)
	}
	allReady := map[Class]bool{
		ClassDevice: true, ClassClient: true, ClassSite: true, ClassWLAN: true,
	}

	const (
		live    = "homeassistant/sensor/unifi_00005e005302/state/config"
		stale   = "homeassistant/sensor/unifi_00005e005399/state/config"
		client  = "homeassistant/device_tracker/unifi_client_aabbccddeeff/presence/config"
		foreign = "homeassistant/sensor/zigbee_lamp/state/config"
		cleared = "homeassistant/sensor/unifi_00005e005400/state/config"
		// A second instance of this daemon, bridging another console to
		// the same broker under its own MQTT root. Its ids sit in the
		// same namespace and classify as ours, so the class gate cannot
		// catch it — only the availability topic tells the two apart. Two
		// instances that got this wrong would delete each other's
		// entities on every start.
		sibling = "homeassistant/sensor/unifi_00005e0053aa/state/config"
	)
	retained := map[string][]byte{
		live:    own("unifi_00005e005302_state"),
		stale:   own("unifi_00005e005399_state"),
		client:  own("unifi_client_aabbccddeeff_presence"),
		foreign: []byte(`{"unique_id":"zigbee_lamp_state","availability":[{"topic":"` + ours + `"}]}`),
		cleared: nil,
		sibling: []byte(`{"unique_id":"unifi_00005e0053aa_state",` +
			`"availability":[{"topic":"unifi-garage/bridge/status"}]}`),
	}
	published := map[string]bool{live: true}

	t.Run("everything ready", func(t *testing.T) {
		t.Parallel()

		got := d.OrphanConfigs(retained, published, allReady)
		want := map[string]bool{stale: true, client: true}
		if len(got) != len(want) {
			t.Fatalf("orphans = %v, want %v", got, want)
		}
		for _, topic := range got {
			if !want[topic] {
				t.Errorf("unexpected orphan %s", topic)
			}
		}
	})

	t.Run("client source not ready", func(t *testing.T) {
		t.Parallel()

		ready := map[Class]bool{ClassDevice: true, ClassSite: true, ClassWLAN: true}
		got := d.OrphanConfigs(retained, published, ready)
		if len(got) != 1 || got[0] != stale {
			t.Fatalf("orphans = %v, want only the stale device config", got)
		}
	})

	t.Run("nothing ready", func(t *testing.T) {
		t.Parallel()

		if got := d.OrphanConfigs(retained, published, nil); len(got) != 0 {
			t.Fatalf("orphans = %v, want none", got)
		}
	})
}
