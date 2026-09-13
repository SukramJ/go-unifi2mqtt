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
		func() ([]Entry, error) { return d.Health() },
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

// Every config topic this daemon writes has to match [ConfigFilter] —
// the five-segment form, ending in `config`. The sweep's window is the
// wider `<prefix>/#`, so a topic failing this is still *seen*; what it
// falls out of is the shape ConfigTopicFor can rebuild, which is how
// the claim list is keyed. One that does not match is never matched
// back to a claim and can never be cleared.
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
		// A config with every mark of ours that this process never
		// published. It is either an earlier run's leftover or a second
		// UniFi console's live entity, and no string on the wire says
		// which — so it is reported and never cleared.
		unpublished = "homeassistant/switch/unifi_site_default/wlan_other-console/config"
		// A second instance under its own MQTT root. Caught one step
		// earlier, by the shape test, so it is not even reported.
		otherRoot = "homeassistant/sensor/unifi_00005e0053aa/state/config"
	)
	retained := map[string][]byte{
		live:        own("unifi_00005e005302_state"),
		stale:       own("unifi_00005e005399_state"),
		client:      own("unifi_client_aabbccddeeff_presence"),
		foreign:     []byte(`{"unique_id":"zigbee_lamp_state","availability":[{"topic":"` + ours + `"}]}`),
		cleared:     nil,
		unpublished: own("unifi_wlan_other-console_enabled"),
		otherRoot: []byte(`{"unique_id":"unifi_00005e0053aa_state",` +
			`"availability":[{"topic":"unifi-garage/bridge/status"}]}`),
	}
	// This process announced `live`, and published `stale` and `client`
	// earlier in this same run before giving them up — a retraction that
	// did not stick is exactly what the sweep is left to retry.
	claims := Claims{
		Published: map[string]bool{live: true, stale: true, client: true},
		Announced: map[string]bool{live: true},
	}

	check := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("everything ready", func(t *testing.T) {
		t.Parallel()

		orphans, unclaimed, unready := d.OrphanConfigs(retained, claims, allReady)
		check(t, orphans, client, stale)
		check(t, unclaimed, unpublished)
		check(t, unready)
	})

	t.Run("client source not ready", func(t *testing.T) {
		t.Parallel()

		ready := map[Class]bool{ClassDevice: true, ClassSite: true, ClassWLAN: true}
		orphans, _, _ := d.OrphanConfigs(retained, claims, ready)
		check(t, orphans, stale)
	})

	t.Run("nothing ready", func(t *testing.T) {
		t.Parallel()

		orphans, unclaimed, unready := d.OrphanConfigs(retained, claims, nil)
		check(t, orphans)
		// Readiness gates the retraction, not the report — but it does
		// decide *which* report. With its class silent, the config may be
		// this daemon's own live entity that nothing has announced yet,
		// so it is named as unready rather than as safely clearable.
		check(t, unclaimed)
		check(t, unready, unpublished)
	})

	t.Run("a process that published nothing clears nothing", func(t *testing.T) {
		t.Parallel()

		orphans, unclaimed, unready := d.OrphanConfigs(retained, Claims{}, allReady)
		check(t, orphans)
		check(t, unclaimed, client, live, stale, unpublished)
		check(t, unready)
	})
}

// The claim list is the whole of the ownership evidence, so a config
// that carries every mark of ours and was not published by this process
// must survive a sweep in which the class is ready, the shape matches
// and the entity is not announced — the exact four conditions the old
// rule cleared on.
func TestAnUnpublishedConfigIsNeverAnOrphan(t *testing.T) {
	t.Parallel()

	d := newTestDiscovery(LangEN)
	const topic = "homeassistant/switch/unifi_site_default/wlan_other-console/config"
	payload := []byte(`{"unique_id":"unifi_wlan_other-console_enabled",` +
		`"availability":[{"topic":"` + stubTopics{}.AvailabilityTopic() + `"}]}`)

	if !d.IsOwnConfig(payload) {
		t.Fatal("setup: the payload no longer has this daemon's shape, " +
			"so this test no longer measures the hard case")
	}
	orphans, unclaimed, _ := d.OrphanConfigs(
		map[string][]byte{topic: payload},
		Claims{Published: map[string]bool{"homeassistant/sensor/unifi_00005e005302/state/config": true}},
		map[Class]bool{ClassDevice: true, ClassClient: true, ClassSite: true, ClassWLAN: true},
	)
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none — an unpublished config is not ours to clear", orphans)
	}
	if len(unclaimed) != 1 || unclaimed[0] != topic {
		t.Errorf("unclaimed = %v, want [%s] — it must at least be reported", unclaimed, topic)
	}
}

// nestedTopics is a second instance of this daemon whose MQTT_TOPIC is
// nested *under* the first one's: root "unifi/kitchen" against
// stubTopics' "unifi".
//
// This is not a hypothetical shape. A review of go-homeconnect2mqtt
// measured exactly it: its ownership predicate asked whether a payload's
// topic sat under the instance's own MQTT root, and an instance rooted
// at `homeconnect` therefore claimed every component of one rooted at
// `homeconnect/kitchen`, because "homeconnect/kitchen/…" starts with
// "homeconnect/". Driven over the real catalogue, 687 of 687 configs
// were accepted and 510 live components tombstoned.
type nestedTopics struct{}

func (nestedTopics) DeviceTopic(mac model.MAC, key string) string {
	return "unifi/kitchen/default/device/" + mac.String() + "/" + key
}

func (nestedTopics) ClientTopic(key, valueKey string) string {
	return "unifi/kitchen/default/client/" + key + "/" + valueKey
}
func (nestedTopics) HealthTopic(key string) string { return "unifi/kitchen/default/health/" + key }
func (nestedTopics) WLANTopic(id, key string) string {
	return "unifi/kitchen/default/wlan/" + id + "/" + key
}
func (nestedTopics) AvailabilityTopic() string { return "unifi/kitchen/bridge/status" }

// TestIsOwnConfigComparesTheAvailabilityTopicExactlyNotByPrefix pins
// that this bridge is immune to the nested-sibling-root defeat above.
//
// Two things protect it and they are not the same thing. The load-
// bearing one is that ownership here is *recorded*: the sweep may only
// retract a topic in Claims.Published, so a nested sibling's configs are
// never candidates whatever they look like. But IsOwnConfig survives as
// the shape half, and a shape half written as "does this payload's
// availability topic sit under my root" would answer *true* for every
// config of the nested instance — the outer root is a prefix of the
// inner one. Exact equality is what keeps the shape half from being the
// homeconnect predicate in a different package.
//
// The second assertion is the point of the test: it demonstrates that
// the prefix form would have accepted these very payloads, so a future
// reader cannot conclude the exactness is incidental.
func TestIsOwnConfigComparesTheAvailabilityTopicExactlyNotByPrefix(t *testing.T) {
	t.Parallel()

	outer := newTestDiscovery(LangEN)
	inner := New(Config{
		BaseTopic: "homeassistant", Topics: nestedTopics{},
		Site: "default", SiteName: "Default", Language: LangEN,
	})

	innerEntries, err := inner.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	wlan, err := inner.WLANControl(&model.WLAN{ID: "w1", Name: "HomeNet", Enabled: true})
	if err != nil {
		t.Fatalf("WLANControl: %v", err)
	}
	innerEntries = append(innerEntries, wlan)
	if len(innerEntries) == 0 {
		t.Fatal("the nested instance rendered nothing")
	}

	outerRootPrefix := stubTopics{}.AvailabilityTopic()
	outerRootPrefix = outerRootPrefix[:strings.Index(outerRootPrefix, "/")+1] // "unifi/"

	prefixWouldAccept := 0
	for _, e := range innerEntries {
		if outer.IsOwnConfig(e.Payload) {
			t.Errorf("%s: the outer instance claims a config published by the "+
				"instance rooted at unifi/kitchen; the availability-topic check "+
				"has stopped being an exact comparison", e.ConfigTopic)
		}
		var cfg ownership
		if err := json.Unmarshal(e.Payload, &cfg); err != nil {
			t.Fatalf("%s: %v", e.ConfigTopic, err)
		}
		if len(cfg.Availability) == 0 {
			t.Fatalf("%s: no availability entry to compare", e.ConfigTopic)
		}
		for _, a := range cfg.Availability {
			if strings.HasPrefix(a.Topic, outerRootPrefix) {
				prefixWouldAccept++
				break
			}
		}
		// The id namespace is shared, so it cannot separate them either.
		if !strings.HasPrefix(cfg.UniqueID, idPrefix+"_") {
			t.Errorf("%s: unique_id %q left the shared namespace; this test no "+
				"longer probes the case it was written for", e.ConfigTopic, cfg.UniqueID)
		}
	}

	if prefixWouldAccept != len(innerEntries) {
		t.Errorf("a root-prefix predicate would have accepted %d of %d nested configs; "+
			"this test only proves exactness matters if the prefix form accepts them all",
			prefixWouldAccept, len(innerEntries))
	}

	// The outer instance's own configs must still read as its own.
	for _, e := range allEntries(t) {
		if !outer.IsOwnConfig(e.Payload) {
			t.Errorf("%s: the outer instance no longer recognises its own config", e.ConfigTopic)
		}
	}
}
