// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	hapub "github.com/SukramJ/go-hamqtt/publisher"
	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
)

// Invariants over the published surface (ADR 0070 phase 9, step 0).
//
// Everything here rebuilds the surface from the real builders and reads
// no testdata, so every one of these tests still fails immediately
// after a golden regeneration. That is the whole point: a golden
// produced by the code it guards cannot catch a builder that moved, and
// the builder-against-builder checks below are what actually pin the
// two independent topic vocabularies in this tree
// (internal/hass's spec.stateSuffix strings against
// internal/coordinator's key* constants).

// --- helpers ---------------------------------------------------------

// entityConfig is the slice of a discovery payload these tests read.
type entityConfig struct {
	Topic string

	Name            string              `json:"name"`
	UniqueID        string              `json:"unique_id"`
	DefaultEntityID string              `json:"default_entity_id"`
	StateTopic      string              `json:"state_topic"`
	CommandTopic    string              `json:"command_topic"`
	AttributesTopic string              `json:"json_attributes_topic"`
	Availability    []availabilitySrc   `json:"availability"`
	AvailMode       string              `json:"availability_mode"`
	AvailTopic      string              `json:"availability_topic"`
	Device          entityDevice        `json:"device"`
	Raw             map[string]any      `json:"-"`
	Extra           map[string]struct{} `json:"-"`
}

type availabilitySrc struct {
	Topic         string `json:"topic"`
	ValueTemplate string `json:"value_template"`
}

type entityDevice struct {
	Identifiers []string `json:"identifiers"`
	Name        string   `json:"name"`
	ViaDevice   string   `json:"via_device"`
}

// surface is one scenario's messages, split into the two planes.
type surface struct {
	name string
	msgs []recordedMsg
	// configs are the discovery payloads, in topic order.
	configs []entityConfig
	// published is every topic the daemon actually wrote to.
	published map[string]bool
}

func loadSurface(t *testing.T, sc surfaceScenario) surface {
	t.Helper()
	doc := buildSurface(t, sc)
	s := surface{name: sc.name, msgs: doc.Messages, published: map[string]bool{}}
	for _, m := range doc.Messages {
		s.published[m.Topic] = true
		if !isConfigTopic(m.Topic) || m.JSON == nil {
			continue
		}
		raw, err := json.Marshal(m.JSON)
		if err != nil {
			t.Fatalf("re-encode %s: %v", m.Topic, err)
		}
		var cfg entityConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("decode %s: %v", m.Topic, err)
		}
		cfg.Topic = m.Topic
		cfg.Raw = m.JSON
		s.configs = append(s.configs, cfg)
	}
	return s
}

// deviceIsOffline reports whether the entity's own device-level
// availability source currently reads as not-online. The value comes
// off the recorded surface, not off the fixture, so the rule is the one
// Home Assistant applies.
func (s surface) deviceIsOffline(cfg entityConfig) bool {
	if len(cfg.Availability) < 2 {
		return false
	}
	for _, m := range s.msgs {
		if m.Topic != cfg.Availability[1].Topic || m.Text == nil {
			continue
		}
		return *m.Text != "ONLINE" && *m.Text != "home"
	}
	return false
}

func isConfigTopic(topic string) bool {
	return strings.HasPrefix(topic, "homeassistant/") && strings.HasSuffix(topic, "/config")
}

func allSurfaces(t *testing.T) []surface {
	t.Helper()
	out := make([]surface, 0, len(surfaceScenarios()))
	for _, sc := range surfaceScenarios() {
		out = append(out, loadSurface(t, sc))
	}
	return out
}

// --- the topic form --------------------------------------------------

// TestConfigTopicForm pins the retained per-entity config topic shape.
//
// This is the single most consequential fact for step 6: a
// publisher.SupersededTopics that renders a form this fleet is not on
// retracts nothing, the device bundle goes out while every per-entity
// config is still retained, and Home Assistant refuses it with one
// WARNING line and no entities.
func TestConfigTopicForm(t *testing.T) {
	t.Parallel()

	var total, nodeIsIdentifier, uidIsNodePlusKey int
	var exceptions []string
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			total++
			parts := strings.Split(cfg.Topic, "/")
			if len(parts) != 5 {
				t.Fatalf("%s: %s is not the five-segment form", s.name, cfg.Topic)
			}
			if parts[0] != "homeassistant" || parts[4] != "config" {
				t.Fatalf("%s: unexpected config topic %s", s.name, cfg.Topic)
			}
			node, object := parts[2], parts[3]
			if len(cfg.Device.Identifiers) != 1 {
				t.Fatalf("%s: %s has %d device identifiers, want 1",
					s.name, cfg.Topic, len(cfg.Device.Identifiers))
			}
			if node == cfg.Device.Identifiers[0] {
				nodeIsIdentifier++
			} else {
				t.Errorf("%s: node id %q is not the device identifier %q",
					s.name, node, cfg.Device.Identifiers[0])
			}
			if cfg.UniqueID == node+"_"+object {
				uidIsNodePlusKey++
			} else {
				exceptions = append(exceptions, cfg.Topic+" -> "+cfg.UniqueID)
			}
		}
	}

	// Held as literals so a change is a declaration in review rather
	// than a silently regenerated number.
	const (
		wantTotal      = 315
		wantExceptions = 21
	)
	if total != wantTotal {
		t.Errorf("config count = %d, want %d", total, wantTotal)
	}
	if nodeIsIdentifier != total {
		t.Errorf("node id == device.identifiers[0] on %d of %d", nodeIsIdentifier, total)
	}
	if got := total - uidIsNodePlusKey; got != wantExceptions {
		sort.Strings(exceptions)
		t.Errorf("unique_id != <node_id>_<object_id> on %d configs, want %d:\n%s",
			got, wantExceptions, strings.Join(exceptions, "\n"))
	}
}

// TestConfigTopicFormIsTheFiveSegmentNodeIDForm is the F5 decision,
// written so it cannot pass vacuously.
//
// go-hamqtt's publisher.SupersededTopics renders the retraction topics
// for the per-entity configs a device bundle replaces. Getting the form
// wrong is silent and total: the bundle is published while every
// per-entity config is still retained, Home Assistant answers with one
// "Received a conflicting MQTT discovery message" warning, and the
// result is no entities and nothing on the wire to say so.
//
// Both candidate forms are transcribed here and rendered over the real
// builder's output. The test requires that LegacyTopicWithNodeID — the
// library's *default* — reproduces every published topic, and that
// LegacyTopicByUniqueID reproduces none of them. Asserting only the
// first would pass just as happily if the two forms ever became
// indistinguishable, which is exactly when the check stops being worth
// anything.
//
// It also pins the two inputs the form is a function of, because
// neither is a property of the default: the bundle's NodeID has to be
// device.identifiers[0], and the component key has to be the object-id
// segment rather than anything derived from the unique_id.
func TestConfigTopicFormIsTheFiveSegmentNodeIDForm(t *testing.T) {
	t.Parallel()

	// publisher.LegacyTopicWithNodeID, the default form.
	withNodeID := func(prefix, platform, nodeID, componentKey, _ string) string {
		return prefix + "/" + platform + "/" + nodeID + "/" + componentKey + "/config"
	}
	// publisher.LegacyTopicByUniqueID, the four-segment alternative.
	byUniqueID := func(prefix, platform, _, _, uniqueID string) string {
		return prefix + "/" + platform + "/" + uniqueID + "/config"
	}

	var matched, unmatchedByUID int
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			parts := strings.Split(cfg.Topic, "/")
			prefix, platform := parts[0], parts[1]
			// The two inputs a step-4 model has to choose. The node id
			// is taken from the payload's own device block, not from
			// the topic, so this proves the bundle can be keyed on it.
			nodeID := cfg.Device.Identifiers[0]
			componentKey := parts[3]

			if got := withNodeID(prefix, platform, nodeID, componentKey, cfg.UniqueID); got != cfg.Topic {
				t.Errorf("%s: the five-segment node-id form renders %q, want %q",
					s.name, got, cfg.Topic)
				continue
			}
			matched++
			if byUniqueID(prefix, platform, nodeID, componentKey, cfg.UniqueID) != cfg.Topic {
				unmatchedByUID++
			}
		}
	}

	if matched == 0 {
		t.Fatal("no configs compared; the test asserts nothing")
	}
	// The whole point: the other candidate must reproduce *nothing*.
	// LegacyEntityTopics replaces the default rather than extending it,
	// so stating LegacyTopicByUniqueID at step 6 would turn a working
	// retraction into none.
	if unmatchedByUID != matched {
		t.Errorf("the by-unique_id form reproduces %d of %d published topics; "+
			"if the two forms are no longer distinguishable this test proves nothing",
			matched-unmatchedByUID, matched)
	}
	if hass.LegacyConfigTopicForm !=
		"<discovery_prefix>/<platform>/<node_id>/<object_id>/config" {
		t.Errorf("hass.LegacyConfigTopicForm = %q, which is no longer the form "+
			"this test proves", hass.LegacyConfigTopicForm)
	}
}

// TestConfigFilterMatchesEveryConfigTopic pins that the orphan
// reconcile's own subscription sees everything this daemon writes — and
// records that it is a five-segment filter, so a four-segment device
// bundle would be invisible to it in both directions.
func TestConfigFilterMatchesEveryConfigTopic(t *testing.T) {
	t.Parallel()

	filter := newSurfaceDiscovery(t).ConfigFilter()
	if filter != "homeassistant/+/+/+/config" {
		t.Fatalf("ConfigFilter = %q", filter)
	}
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			if !matchFilter(filter, cfg.Topic) {
				t.Errorf("%s: %s does not match %s", s.name, cfg.Topic, filter)
			}
		}
	}
	// A device bundle is four segments and therefore outside the filter.
	if matchFilter(filter, "homeassistant/device/unifi_00005e005301/config") {
		t.Error("the five-segment filter unexpectedly matches a device bundle")
	}
}

// surfaceBaseConfig is the default operator config with discovery on.
func surfaceBaseConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(baseYAML), config.MapEnv{})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// newSurfaceDiscovery builds the real discovery builder over the real
// coordinator topic layout, so ConfigFilter is asked of the same object
// production asks.
func newSurfaceDiscovery(t *testing.T) *hass.Discovery {
	t.Helper()
	c := newHarness(t, surfaceBaseConfig(t)).c
	return hass.New(c.DiscoveryConfig("homeassistant", "en"))
}

// matchFilter implements MQTT topic-filter matching for the two
// wildcards, which is all these filters use.
func matchFilter(filter, topic string) bool {
	f := strings.Split(filter, "/")
	p := strings.Split(topic, "/")
	for i, seg := range f {
		if seg == "#" {
			return true
		}
		if i >= len(p) {
			return false
		}
		if seg != "+" && seg != p[i] {
			return false
		}
	}
	return len(f) == len(p)
}

// --- identity --------------------------------------------------------

// TestNoDuplicateEntityRegistryKeys counts the keys Home Assistant has
// no migration path for.
func TestNoDuplicateEntityRegistryKeys(t *testing.T) {
	t.Parallel()

	for _, s := range allSurfaces(t) {
		uid := map[string][]string{}
		pair := map[string][]string{}
		eid := map[string][]string{}
		for _, cfg := range s.configs {
			platform := strings.Split(cfg.Topic, "/")[1]
			uid[cfg.UniqueID] = append(uid[cfg.UniqueID], cfg.Topic)
			pair[platform+"|"+cfg.UniqueID] = append(pair[platform+"|"+cfg.UniqueID], cfg.Topic)
			eid[cfg.DefaultEntityID] = append(eid[cfg.DefaultEntityID], cfg.Topic)
		}
		reportDuplicates(t, s.name, "unique_id", uid)
		reportDuplicates(t, s.name, "(platform, unique_id)", pair)
		reportDuplicates(t, s.name, "default_entity_id", eid)
	}
}

func reportDuplicates(t *testing.T, scenario, what string, index map[string][]string) {
	t.Helper()
	for key, topics := range index {
		if len(topics) > 1 {
			sort.Strings(topics)
			t.Errorf("%s: duplicate %s %q on:\n  %s",
				scenario, what, key, strings.Join(topics, "\n  "))
		}
	}
}

// TestDeviceBlocksArePinned holds the device-registry keys as literals.
// Home Assistant keys the device registry on `identifiers`, and there
// is no migration path.
func TestDeviceBlocksArePinned(t *testing.T) {
	t.Parallel()

	want := []string{
		"unifi_00005e005301",
		"unifi_00005e005302",
		"unifi_00005e005303",
		"unifi_00005e005304",
		"unifi_client_00005e005311",
		"unifi_client_00005e005312",
		"unifi_client_00005e005313",
		"unifi_site_default",
	}
	got := map[string]bool{}
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			got[cfg.Device.Identifiers[0]] = true
		}
	}
	have := slices.Sorted(keysOf(got))
	if !slices.Equal(have, want) {
		t.Errorf("device identifiers =\n%#v\nwant\n%#v", have, want)
	}
}

func keysOf(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// --- the payload prerequisites (F7, F14, F15) -------------------------

// TestEveryConfigCarriesTheOrigin is F7.
//
// A device bundle without `origin.name` is refused outright by
// go-hamqtt's discovery.Validate, and a refused bundle publishes no
// entities at all — so the block is not a nicety, it is what makes the
// step-6 bundle form publishable. It goes on every config rather than
// only the ones a bundle will carry: a key present on some configs and
// absent on others makes the migration diff unreadable.
//
// The exact shape is pinned too. `sw_version` is deliberately absent:
// this bridge's version is stamped at link time, so carrying it would
// make 315 retained payloads' bytes depend on how the binary was
// linked.
func TestEveryConfigCarriesTheOrigin(t *testing.T) {
	t.Parallel()

	const wantName = "go-unifi2mqtt"
	seen := 0
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			o, ok := cfg.Raw["origin"].(map[string]any)
			if !ok {
				t.Errorf("%s: %s carries no origin block", s.name, cfg.Topic)
				continue
			}
			seen++
			if o["name"] != wantName {
				t.Errorf("%s: %s origin.name = %v, want %q", s.name, cfg.Topic, o["name"], wantName)
			}
			if len(o) != 1 {
				t.Errorf("%s: %s origin carries %d keys (%v), want name only",
					s.name, cfg.Topic, len(o), o)
			}
		}
	}
	if seen != hamqttTotalConfigs {
		t.Errorf("%d configs carry an origin, want all %d", seen, hamqttTotalConfigs)
	}
}

// TestSiteDeviceIsAnnouncedUnderOneName is F14, over the published
// surface rather than over the rendering experiment.
//
// The synthetic site device used to be "UniFi Site <Site.Name>" on its
// health entities and "UniFi Site <Site.Internal>" on every SSID
// switch. Per entity that is last-write-wins in Home Assistant's device
// registry and invisible; a device bundle carries exactly one device
// block, so one of the two would have won by collection order. Both
// paths now read Site.Name, because Site.Internal is the API's
// addressing token and was never a display string.
//
// Identity is untouched: `identifiers` keys on Site.Internal on both
// paths and did before, which is why this rename re-registers nothing.
func TestSiteDeviceIsAnnouncedUnderOneName(t *testing.T) {
	t.Parallel()

	const siteID = "unifi_site_default"
	names := map[string]int{}
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			if len(cfg.Device.Identifiers) == 0 || cfg.Device.Identifiers[0] != siteID {
				continue
			}
			names[cfg.Device.Name]++
		}
	}
	// Both planes have to be in the sample, or "one name" is trivially
	// true because only one of them was looked at.
	if len(names) != 1 {
		t.Fatalf("%s is announced under %d names: %v", siteID, len(names), names)
	}
	const want = "UniFi Site Default"
	if n := names[want]; n == 0 {
		t.Fatalf("%s is announced as %v, want %q", siteID, names, want)
	} else if n < 20 {
		t.Errorf("only %d site configs sampled; the health plane and the SSID switches "+
			"must both be represented or this test proves nothing", n)
	}
}

// TestNoConfigPublishesAnEmptyManufacturer is F15.
//
// clientDeviceInfo has no manufacturer to report for a network client.
// It used to say so with `"manufacturer": ""`; deviceInfo now omits the
// key, which is what Home Assistant reads the empty string as anyway
// and what go-hamqtt's own omitempty field renders. Step 4 carved this
// one key out of its byte comparison; that carve-out is gone.
func TestNoConfigPublishesAnEmptyManufacturer(t *testing.T) {
	t.Parallel()

	absent, present := 0, 0
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			dev, _ := cfg.Raw["device"].(map[string]any)
			m, ok := dev["manufacturer"]
			switch {
			case !ok:
				absent++
			case m == "":
				t.Errorf("%s: %s publishes an empty manufacturer", s.name, cfg.Topic)
			default:
				present++
			}
		}
	}
	// The 36 client configs of the three `full` scenarios are the ones
	// that lost the key; every other config still names Ubiquiti, so a
	// fix that dropped the key everywhere fails here.
	const wantAbsent = 36
	if absent != wantAbsent {
		t.Errorf("%d configs omit the manufacturer, want %d", absent, wantAbsent)
	}
	if present != hamqttTotalConfigs-wantAbsent {
		t.Errorf("%d configs name a manufacturer, want %d",
			present, hamqttTotalConfigs-wantAbsent)
	}
}

// TestIdentityIsLanguageIndependent compares the two languages
// directly. CONCEPT.md §6.2 states unique_id, default_entity_id and
// every topic segment are English; this is the check of it.
func TestIdentityIsLanguageIndependent(t *testing.T) {
	t.Parallel()

	pairs := [][2]string{{"minimal.en", "minimal.de"}, {"full.en", "full.de"}}
	byName := map[string]surface{}
	for _, s := range allSurfaces(t) {
		byName[s.name] = s
	}
	for _, p := range pairs {
		en, de := byName[p[0]], byName[p[1]]
		if len(en.configs) != len(de.configs) {
			t.Fatalf("%s/%s: %d vs %d configs", p[0], p[1], len(en.configs), len(de.configs))
		}
		for i := range en.configs {
			a, b := en.configs[i], de.configs[i]
			if a.Topic != b.Topic {
				t.Errorf("%s: config topic moves with language: %s vs %s", p[0], a.Topic, b.Topic)
			}
			if a.UniqueID != b.UniqueID {
				t.Errorf("%s: unique_id moves with language: %s vs %s", a.Topic, a.UniqueID, b.UniqueID)
			}
			if a.DefaultEntityID != b.DefaultEntityID {
				t.Errorf("%s: default_entity_id moves with language: %s vs %s",
					a.Topic, a.DefaultEntityID, b.DefaultEntityID)
			}
			if a.StateTopic != b.StateTopic || a.CommandTopic != b.CommandTopic {
				t.Errorf("%s: a topic moves with language", a.Topic)
			}
			if !slices.Equal(a.Device.Identifiers, b.Device.Identifiers) {
				t.Errorf("%s: device.identifiers moves with language", a.Topic)
			}
		}
	}
}

// --- availability ----------------------------------------------------

// TestAvailabilityModelIsTwoLevel pins the model the migration must not
// change by accident: a list of one or two sources with mode "all",
// where the second level is the *device's own state topic* rather than
// a per-device availability topic.
//
// This matters because go-hamqtt's zero model.Availability resolves to
// {LevelBridge, LevelDevice} with mode "all", and its LevelDevice names
// a dedicated `<root>/<uid>/availability` topic that this bridge never
// writes. Taking the default would leave every entity that currently
// has a second level permanently unavailable.
func TestAvailabilityModelIsTwoLevel(t *testing.T) {
	t.Parallel()

	var oneLevel, twoLevel int
	for _, s := range allSurfaces(t) {
		bridge := ""
		for topic := range s.published {
			if strings.HasSuffix(topic, "/bridge/status") {
				bridge = topic
			}
		}
		if bridge == "" {
			t.Fatalf("%s: no bridge status topic published", s.name)
		}
		for _, cfg := range s.configs {
			if cfg.AvailTopic != "" {
				t.Errorf("%s: uses the singular availability_topic form", cfg.Topic)
			}
			if cfg.AvailMode != "all" {
				t.Errorf("%s: availability_mode = %q, want \"all\"", cfg.Topic, cfg.AvailMode)
			}
			if len(cfg.Availability) == 0 || cfg.Availability[0].Topic != bridge {
				t.Fatalf("%s: first availability source is not the bridge topic", cfg.Topic)
			}
			if cfg.Availability[0].ValueTemplate != "" {
				t.Errorf("%s: bridge availability carries a value_template", cfg.Topic)
			}
			switch len(cfg.Availability) {
			case 1:
				oneLevel++
			case 2:
				twoLevel++
				second := cfg.Availability[1]
				if !s.published[second.Topic] {
					t.Errorf("%s: second availability source %q is published by nobody",
						cfg.Topic, second.Topic)
				}
				if second.ValueTemplate == "" {
					t.Errorf("%s: second availability source has no value_template", cfg.Topic)
				}
			default:
				t.Errorf("%s: %d availability sources", cfg.Topic, len(cfg.Availability))
			}
		}
	}

	const (
		wantOneLevel = 128
		wantTwoLevel = 187
	)
	if oneLevel != wantOneLevel || twoLevel != wantTwoLevel {
		t.Errorf("availability levels: one=%d two=%d, want one=%d two=%d",
			oneLevel, twoLevel, wantOneLevel, wantTwoLevel)
	}
}

// --- delivery --------------------------------------------------------

// TestPublishQoSAndRetain reads the delivery guarantee off the
// transport call, not off a constant.
//
// go-hamqtt's publisher.QoS zero value is QoSUnset and resolves to
// QoS 1, so a Config left unset silently moves this bridge's state
// plane from QoS 0 to QoS 1. Deliberate QoS 0 is the sentinel
// publisher.QoSAtMostOnce (0x80).
func TestPublishQoSAndRetain(t *testing.T) {
	t.Parallel()

	var configQoS1, stateQoS0, availQoS1, other int
	for _, s := range allSurfaces(t) {
		for _, m := range s.msgs {
			switch {
			case !m.Retain:
				other++
				t.Errorf("%s: %s published unretained", s.name, m.Topic)
			case isConfigTopic(m.Topic):
				if m.QoS != 1 {
					t.Errorf("%s: config %s at QoS %d, want 1", s.name, m.Topic, m.QoS)
				}
				configQoS1++
			case strings.HasSuffix(m.Topic, "/bridge/status"):
				if m.QoS != 1 {
					t.Errorf("%s: availability %s at QoS %d, want 1", s.name, m.Topic, m.QoS)
				}
				availQoS1++
			default:
				if m.QoS != 0 {
					t.Errorf("%s: state %s at QoS %d, want 0", s.name, m.Topic, m.QoS)
				}
				stateQoS0++
			}
		}
	}

	const (
		wantConfig = 315
		wantState  = 317
		wantAvail  = 5
	)
	if configQoS1 != wantConfig || stateQoS0 != wantState || availQoS1 != wantAvail || other != 0 {
		t.Errorf("delivery census: config=%d state=%d availability=%d unretained=%d, "+
			"want %d/%d/%d/0", configQoS1, stateQoS0, availQoS1, other,
			wantConfig, wantState, wantAvail)
	}
}

// --- builder against builder -----------------------------------------

// knownAdvertisedButUnpublished lists the topics a discovery config
// names that nothing in this daemon ever writes to.
//
// Every entry is a finding in notes/adr0070-phase9-measurement.md, not
// an accepted shape: an entity pointing at a topic nobody writes sits
// at "unknown" forever with nothing in any log. They are listed here so
// the test can pin the *exact* set — a new one fails, and fixing one
// fails until it is removed from this list.
//
// It is empty. The four entries it held were the locate switches of
// F11, and the locate LED is now read back from the classic API and
// published. The map stays because its emptiness is an assertion: a
// topic that becomes advertised-but-unwritten has to be declared here,
// in review, rather than quietly tolerated.
var knownAdvertisedButUnpublished = map[string]string{}

// TestAdvertisedStateTopicsArePublished is the builder-against-builder
// check: every state and attributes topic a discovery config names is
// compared against the topics the publish path actually writes.
//
// The two sides are composed by two independent vocabularies —
// internal/hass's spec.stateSuffix string literals and
// internal/coordinator's key* constants — and nothing before this
// compared them. A change to either alone leaves entities pointing at
// topics nobody writes: permanently "unknown", nothing in the log.
func TestAdvertisedStateTopicsArePublished(t *testing.T) {
	t.Parallel()

	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			if s.deviceIsOffline(cfg) {
				// A device the console reports as offline has no
				// statistics sample by design (refreshDeviceStats skips
				// it), so its statistics topics legitimately receive
				// nothing — which is exactly what the entity's second
				// availability level covers.
				continue
			}
			for _, topic := range []string{cfg.StateTopic, cfg.AttributesTopic} {
				if topic == "" || s.published[topic] {
					continue
				}
				if _, known := knownAdvertisedButUnpublished[topic]; known {
					continue
				}
				t.Errorf("%s: %s names %q, which nothing publishes",
					s.name, cfg.Topic, topic)
			}
		}
	}
}

// TestKnownUnpublishedTopicsAreStillAdvertised is the other half: an
// entry in knownAdvertisedButUnpublished that stops being advertised —
// because the finding was fixed, or because the entity was dropped —
// must be deleted from the list rather than left behind to mask a
// future one.
func TestKnownUnpublishedTopicsAreStillAdvertised(t *testing.T) {
	t.Parallel()

	advertised := map[string]bool{}
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			advertised[cfg.StateTopic] = true
			advertised[cfg.AttributesTopic] = true
		}
	}
	for topic, why := range knownAdvertisedButUnpublished {
		if !advertised[topic] {
			t.Errorf("%q is no longer advertised; drop it from "+
				"knownAdvertisedButUnpublished (%s)", topic, why)
		}
	}
}

// TestLocateSwitchStateReflectsTheReadBack is the regression for F11 of
// notes/adr0070-phase9-measurement.md.
//
// The locate switch advertised `…/device/<mac>/locate` as its state
// topic and nothing in this daemon ever wrote to it. The switch sat at
// "unknown" forever: a press was executed — the command path works —
// and the entity never reflected it, with nothing in any log.
//
// Asserting only that the topic is written would pass on a publisher
// that hard-codes OFF, so this checks the *value* against the fixture's
// read-back, which is ON for exactly one device of the four.
func TestLocateSwitchStateReflectsTheReadBack(t *testing.T) {
	t.Parallel()

	// The one device whose locate LED the fixture reports as lit.
	const litDevice = "unifi/default/device/00005e005302/locate"

	checked := 0
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			if !strings.HasSuffix(cfg.Topic, "/locate/config") {
				continue
			}
			checked++
			want := "OFF"
			if cfg.StateTopic == litDevice {
				want = "ON"
			}
			got, ok := s.textOn(cfg.StateTopic)
			if !ok {
				t.Errorf("%s: %s names %q, which nothing publishes",
					s.name, cfg.Topic, cfg.StateTopic)
				continue
			}
			if got != want {
				t.Errorf("%s: %s = %q, want %q", s.name, cfg.StateTopic, got, want)
			}
		}
	}
	// Three scenarios have the control on, four devices each.
	const wantChecked = 12
	if checked != wantChecked {
		t.Errorf("checked %d locate switches, want %d", checked, wantChecked)
	}
}

// textOn returns the scalar payload published on a topic.
func (s surface) textOn(topic string) (string, bool) {
	for _, m := range s.msgs {
		if m.Topic == topic && m.Text != nil {
			return *m.Text, true
		}
	}
	return "", false
}

// TestCommandTopicsAreSubscribed pins the third vocabulary: the command
// suffixes internal/hass writes into a config as string literals
// against the cmd* constants internal/coordinator subscribes and parses
// on. They are two copies of the same six strings.
func TestCommandTopicsAreSubscribed(t *testing.T) {
	t.Parallel()

	filters := []string{
		"unifi/default/device/+/" + cmdRestart,
		"unifi/default/device/+/" + cmdLocateSet,
		"unifi/default/device/+/port/+/" + cmdPowerCycle,
		"unifi/default/client/+/" + cmdBlockedSet,
		"unifi/default/client/+/" + cmdAuthorize,
		"unifi/default/wlan/+/" + cmdWLANEnabled,
	}

	var commands int
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			if cfg.CommandTopic == "" {
				continue
			}
			commands++
			matched := false
			for _, f := range filters {
				if matchFilter(f, cfg.CommandTopic) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("%s: command topic %q matches no subscription",
					cfg.Topic, cfg.CommandTopic)
			}
		}
	}
	const wantCommands = 45
	if commands != wantCommands {
		t.Errorf("command topics = %d, want %d", commands, wantCommands)
	}
}

// --- the slug --------------------------------------------------------

// librarySlug is go-hamqtt's topic.Slug.
//
// It was a verbatim transcription while the measurement could not take
// the dependency; step 4 takes it, so this now calls the real function
// and the count in §3.3 is measured against the library rather than
// against a copy of it that could drift.
func librarySlug(s string) string { return hatopic.Slug(s) }

// TestSlugAgreementOverTheRealCatalogue counts, over this bridge's own
// catalogue and over the device names its users actually have, how
// often go-hamqtt's topic.Slug would produce a different string than
// internal/hass.slugify.
//
// The divergence reaches default_entity_id and nothing else here —
// unique_id and device.identifiers are MAC-derived and ASCII — but
// Home Assistant never renames a registered entity, so swapping the
// function strands every entity on a non-ASCII-named device.
func TestSlugAgreementOverTheRealCatalogue(t *testing.T) {
	t.Parallel()

	// The entity half of every id: the keys internal/hass turns into
	// the second half of an entity-id seed.
	keys := entityKeyCatalogue(t)
	var keyDiverge int
	for _, k := range keys {
		if hassSlugProbe(k) != librarySlug(k) {
			keyDiverge++
			t.Logf("entity key %q: hass=%q library=%q", k, hassSlugProbe(k), librarySlug(k))
		}
	}

	names := []string{
		"Gateway", "Switch Garage", "AP", "UniFi Site Default",
		"Büro-Gateway", "Switch Küche", "Außengerät", "Groß-NAS",
		"Märkus' Händy", "Süd-WLAN", "Gäste", "EG-Wohnzimmer", "",
	}
	var nameDiverge int
	for _, n := range names {
		if hassSlugProbe(n) != librarySlug(n) {
			nameDiverge++
			t.Logf("device name %q: hass=%q library=%q", n, hassSlugProbe(n), librarySlug(n))
		}
	}

	const (
		wantKeys        = 35
		wantKeyDiverge  = 2
		wantNames       = 13
		wantNameDiverge = 9
	)
	if len(keys) != wantKeys || keyDiverge != wantKeyDiverge {
		t.Errorf("entity keys: %d probed, %d diverge; want %d, %d",
			len(keys), keyDiverge, wantKeys, wantKeyDiverge)
	}
	if len(names) != wantNames || nameDiverge != wantNameDiverge {
		t.Errorf("device names: %d probed, %d diverge; want %d, %d",
			len(names), nameDiverge, wantNames, wantNameDiverge)
	}
}

// hassSlugProbe reproduces internal/hass.slugify, which is unexported.
// Kept beside librarySlug so the two are read together.
func hassSlugProbe(s string) string {
	r := strings.NewReplacer("ä", "a", "ö", "o", "ü", "u", "ß", "ss")
	var b strings.Builder
	last := true
	for _, c := range r.Replace(strings.ToLower(s)) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
			last = false
			continue
		}
		if !last {
			b.WriteRune('_')
			last = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// entityKeyCatalogue is every distinct entity key this daemon
// publishes, derived from the config topics rather than transcribed.
func entityKeyCatalogue(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, s := range allSurfaces(t) {
		for i := range s.configs {
			seen[strings.Split(s.configs[i].Topic, "/")[3]] = true
		}
	}
	return slices.Sorted(keysOf(seen))
}

// --- two instances ---------------------------------------------------

// TestTwoDefaultInstancesCollideOnEveryString measures what two
// default-configured daemons on one broker do to each other.
func TestTwoDefaultInstancesCollideOnEveryString(t *testing.T) {
	t.Parallel()

	a := loadSurface(t, surfaceScenarios()[2]) // full.en
	b := loadSurface(t, surfaceScenarios()[2])

	shared := 0
	for i := range a.configs {
		if a.configs[i].Topic == b.configs[i].Topic &&
			a.configs[i].UniqueID == b.configs[i].UniqueID {
			shared++
		}
	}
	if shared != len(a.configs) {
		t.Errorf("two default instances share %d of %d config topics and unique_ids",
			shared, len(a.configs))
	}

	// The second signal IsOwnConfig requires is the availability topic,
	// which embeds MQTT_TOPIC — so an instance on a different root sees
	// these configs as somebody else's and leaves them alone, while
	// still overwriting every one of them.
	other := loadSurfaceWithRoot(t, "unifi2")
	for i := range a.configs {
		if a.configs[i].Topic != other.configs[i].Topic {
			t.Fatalf("changing MQTT_TOPIC moved a config topic: %s vs %s",
				a.configs[i].Topic, other.configs[i].Topic)
		}
		if a.configs[i].UniqueID != other.configs[i].UniqueID {
			t.Fatalf("changing MQTT_TOPIC moved a unique_id")
		}
		if a.configs[i].Availability[0].Topic == other.configs[i].Availability[0].Topic {
			t.Fatalf("changing MQTT_TOPIC did not move the availability topic")
		}
	}
}

// TestClientIDSeparatesSessionsAndNothingElse is the other half of F6,
// and the fact step 6 has to plan around.
//
// MQTT_CLIENT_ID ends the eviction loop two default daemons are in. It
// does not separate their published surface by one byte: the config
// topics, the unique_ids and the device identifiers carry no
// instance-scoped string at all, and unlike MQTT_TOPIC the client id
// does not even move the availability topic.
//
// Today that means two instances on one console overwrite each other
// entity by entity — noisy, and converging. At step 6 a device bundle
// is *one* retained topic carrying that device's whole component set,
// so two instances with any divergence — a different LANGUAGE, a
// different control set, one with the classic layer and one without —
// replace each other's entire entity set on every publish. A staggered
// upgrade is worse still: A retracts the per-entity configs, B on the
// old build republishes them, A retracts again, and upgrading B second
// makes B's bundle replace A's whole fleet. go-mtec2mqtt's reviewer
// demonstrated that this actually happens.
func TestClientIDSeparatesSessionsAndNothingElse(t *testing.T) {
	t.Parallel()

	sc := surfaceScenarios()[2] // full.en
	a := loadSurface(t, sc)

	other := sc
	other.name = "full.en.client-id"
	other.yaml = sc.yaml + "MQTT_CLIENT_ID: unifi2mqtt-garage\n"
	b := loadSurface(t, other)

	if len(a.msgs) != len(b.msgs) {
		t.Fatalf("MQTT_CLIENT_ID changed the message count: %d vs %d",
			len(a.msgs), len(b.msgs))
	}
	for i := range a.msgs {
		x, err := json.Marshal(a.msgs[i])
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		y, err := json.Marshal(b.msgs[i])
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !bytes.Equal(x, y) {
			t.Errorf("MQTT_CLIENT_ID moved a published message: %s",
				a.msgs[i].Topic)
		}
	}
	// And the configs specifically, so a future availability change
	// cannot make this pass for the wrong reason.
	for i := range a.configs {
		if a.configs[i].Topic != b.configs[i].Topic ||
			a.configs[i].UniqueID != b.configs[i].UniqueID ||
			len(a.configs[i].Availability) != len(b.configs[i].Availability) ||
			a.configs[i].Availability[0].Topic != b.configs[i].Availability[0].Topic {
			t.Errorf("MQTT_CLIENT_ID moved %s", a.configs[i].Topic)
		}
	}
}

func loadSurfaceWithRoot(t *testing.T, root string) surface {
	t.Helper()
	sc := surfaceScenarios()[2]
	sc.name = "full.en.root-" + root
	sc.yaml = strings.Replace(sc.yaml, "MQTT_TOPIC: unifi\n", "MQTT_TOPIC: "+root+"\n", 1)
	return loadSurface(t, sc)
}

// --- the census ------------------------------------------------------

// TestSurfaceCensus holds the measured surface as Go literals, so a
// change to it is a declaration in review rather than a regenerated
// blob.
func TestSurfaceCensus(t *testing.T) {
	t.Parallel()

	type census struct {
		messages, configs int
		platforms         string
	}
	want := map[string]census{
		"minimal.en":  {92, 45, "binary_sensor=11 sensor=34"},
		"minimal.de":  {92, 45, "binary_sensor=11 sensor=34"},
		"full.en":     {151, 75, "binary_sensor=12 button=6 device_tracker=3 sensor=45 switch=9"},
		"full.de":     {151, 75, "binary_sensor=12 button=6 device_tracker=3 sensor=45 switch=9"},
		"nonascii.de": {151, 75, "binary_sensor=12 button=6 device_tracker=3 sensor=45 switch=9"},
	}
	for _, s := range allSurfaces(t) {
		counts := map[string]int{}
		for _, cfg := range s.configs {
			counts[strings.Split(cfg.Topic, "/")[1]]++
		}
		parts := make([]string, 0, len(counts))
		for _, p := range slices.Sorted(keysOf(boolSet(counts))) {
			parts = append(parts, fmt.Sprintf("%s=%d", p, counts[p]))
		}
		got := census{len(s.msgs), len(s.configs), strings.Join(parts, " ")}
		if got != want[s.name] {
			t.Errorf("%s census = %+v, want %+v", s.name, got, want[s.name])
		}
	}
}

func boolSet(m map[string]int) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// The sweep's platform set and the catalogue's platforms are the same
// five, compared rather than spelled twice.
//
// [hass.OwnsConfigTopic] judges a retained config by a closed map of
// platforms; TestSurfaceCensus states the same five again as a fixture
// histogram. Nothing compared them, so dropping one from the map
// survived the suite. A platform that falls out of it becomes invisible
// to the sweep in *both* directions — its configs are never retracted
// and never even reported — and the only evidence would be entities
// that quietly accumulate on the broker.
//
// The two sibling tests cannot see this: TestOwnsConfigTopicIsNarrow
// uses synthetic topics and TestReportOnlySweepOverTheRealFleet builds
// its own ten-topic tree. This runs the real predicate over the real
// fleet.
func TestTheSweepPredicateOwnsEveryConfigThisDaemonPublishes(t *testing.T) {
	t.Parallel()

	const prefix = "homeassistant"
	seen := map[string]bool{}
	for _, s := range allSurfaces(t) {
		for _, cfg := range s.configs {
			parsed, ok := hapub.ParseConfigTopic(prefix, cfg.Topic)
			if !ok {
				t.Errorf("%s: %s does not parse as a discovery config topic", s.name, cfg.Topic)
				continue
			}
			seen[parsed.Platform] = true
			if !hass.OwnsConfigTopic(parsed) {
				t.Errorf("%s: the sweep does not recognise %s as this daemon's own. "+
					"It would neither be retracted nor reported — an entity that "+
					"accumulates silently on the broker.", s.name, cfg.Topic)
			}
		}
	}

	// And the other direction: a platform in the closed set that the
	// catalogue no longer produces widens the window over configs that
	// are not ours to judge.
	got := slices.Sorted(keysOf(seen))
	want := hass.PublishedPlatforms()
	if !slices.Equal(got, want) {
		t.Errorf("the rendered surface covers platforms %v, while hass.PublishedPlatforms "+
			"declares %v; the sweep and the catalogue disagree about what this daemon emits",
			got, want)
	}
}
