// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Unit pins for the go-hamqtt rendering path — ADR 0070 phase 9, step 4.
//
// The byte-equality proof lives in internal/coordinator, against the
// five pinned scenario goldens. This file pins the pieces that proof
// rests on, one at a time, and — more importantly — pins each of them
// *against the library's own default*, so an override that has quietly
// become inert fails here rather than surviving as coverage it is not.

func hamqttTestDiscovery(lang string) *Discovery { return newTestDiscovery(lang) }

// renderOne renders a single entity of a group, by key.
func renderOne(t *testing.T, d *Discovery, g *hamqttGroup, key string) discovery.Component {
	t.Helper()
	for _, e := range g.entities {
		if e.Key() != key {
			continue
		}
		comp, err := discovery.RenderComponent(d.newHamqttContext(), g.dev, e, discovery.Origin{})
		if err != nil {
			t.Fatalf("render %q: %v", key, err)
		}
		return comp
	}
	t.Fatalf("no entity with key %q", key)
	return discovery.Component{}
}

func findEntity(t *testing.T, g *hamqttGroup, key string) *hamqttEntity {
	t.Helper()
	for _, e := range g.entities {
		if e.Key() == key {
			ent, ok := e.(*hamqttEntity)
			if !ok {
				t.Fatalf("entity %q is not a *hamqttEntity", key)
			}
			return ent
		}
	}
	t.Fatalf("no entity with key %q", key)
	return nil
}

// --- the layout -------------------------------------------------------

// TestHamqttLayoutDelegatesToTheTopicsContract pins that the layout
// composes nothing of its own: every topic it renders comes back out of
// the [Topics] implementation it was given.
//
// That is the whole reason it exists in this shape. F10 counted 22
// places in this tree that compose an MQTT topic string, 18 of which
// remain; a Layout that re-formatted "<root>/<site>/device/<mac>/<key>"
// would be the 23rd, agreeing today and free to drift tomorrow, with
// the symptom being an entity that stays "unknown" forever and nothing
// in any log.
func TestHamqttLayoutDelegatesToTheTopicsContract(t *testing.T) {
	t.Parallel()

	l := hamqttLayout{topics: stubTopics{}}
	mac := model.MustParseMAC("00:00:5e:00:53:02")

	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			"device value", l.State(deviceSlot(mac.String(), "cpu_utilization")),
			stubTopics{}.DeviceTopic(mac, "cpu_utilization"),
		},
		{
			"port value", l.State(deviceSlot(mac.String(), "port/1/poe")),
			stubTopics{}.DeviceTopic(mac, "port/1/poe"),
		},
		{
			"radio value", l.State(deviceSlot(mac.String(), "radio/5g/channel")),
			stubTopics{}.DeviceTopic(mac, "radio/5g/channel"),
		},
		{
			"device command", l.Command(deviceSlot(mac.String(), "cmd/locate/set")),
			stubTopics{}.DeviceTopic(mac, "cmd/locate/set"),
		},
		{
			"port command", l.Command(deviceSlot(mac.String(), "port/1/cmd/power_cycle")),
			stubTopics{}.DeviceTopic(mac, "port/1/cmd/power_cycle"),
		},
		{
			"client value", l.State(clientSlot("00005e005311", "ip")),
			stubTopics{}.ClientTopic("00005e005311", "ip"),
		},
		{
			"client command", l.Command(clientSlot("00005e005311", "blocked/set")),
			stubTopics{}.ClientTopic("00005e005311", "blocked/set"),
		},
		{
			"health value", l.State(healthSlot("wan/latency_ms")),
			stubTopics{}.HealthTopic("wan/latency_ms"),
		},
		{
			"wlan value", l.State(wlanSlot("wlan-1", "enabled")),
			stubTopics{}.WLANTopic("wlan-1", "enabled"),
		},
		{
			"wlan command", l.Command(wlanSlot("wlan-1", "enabled/set")),
			stubTopics{}.WLANTopic("wlan-1", "enabled/set"),
		},
		{"bridge", l.Bridge(), stubTopics{}.AvailabilityTopic()},
		// The device-availability slot is the object's own state topic,
		// which is where this bridge's reachability signal actually is.
		{
			"device availability", l.Availability(deviceSlot(mac.String(), "cpu_utilization")),
			stubTopics{}.DeviceTopic(mac, "state"),
		},
		{
			"client availability", l.Availability(clientSlot("00005e005311", "ip")),
			stubTopics{}.ClientTopic("00005e005311", "state"),
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: layout renders %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestHamqttLayoutIsNotTopicDefault records why go-hamqtt's own
// topic.Default cannot be used here, rather than leaving the custom
// layout looking like an oversight.
//
// Three independent reasons, each asserted: Default has a bucket level
// this tree does not have, it keys devices on the device UID where this
// tree uses the bare MAC, and its Command is always State+"/set" where
// four of this bridge's six command suffixes are spelled differently.
func TestHamqttLayoutIsNotTopicDefault(t *testing.T) {
	t.Parallel()

	def := hatopic.Default{Root: "unifi"}
	mac := model.MustParseMAC("00:00:5e:00:53:02")
	ours := hamqttLayout{topics: stubTopics{}}

	slot := deviceSlot(mac.String(), "cpu_utilization")
	if def.State(slot) == ours.State(slot) {
		t.Errorf("topic.Default renders the same state topic as this bridge's layout (%q); "+
			"the custom layout would be unnecessary", def.State(slot))
	}
	cmd := deviceSlot(mac.String(), "cmd/restart")
	if def.Command(cmd) == ours.Command(cmd) {
		t.Errorf("topic.Default renders the same command topic as this bridge's layout (%q)",
			def.Command(cmd))
	}
	// The command form is the trap: appending "/set" to the state topic
	// would silently produce topics nothing subscribes to.
	if got := ours.Command(cmd); strings.HasSuffix(got, "/set") {
		t.Errorf("the restart command renders %q; this bridge spells it under cmd/", got)
	}
}

// TestHamqttLayoutIgnoresBucketAndChannel names a blind spot rather
// than claiming it as coverage.
//
// The layout reads a slot's Scope, Address and Path and nothing else.
// [hamodel.Slot] also carries a Bucket and a Channel, which this tree
// has no level for. Both directions are asserted, so the test cannot
// pass by the layout ignoring everything.
func TestHamqttLayoutIgnoresBucketAndChannel(t *testing.T) {
	t.Parallel()

	l := hamqttLayout{topics: stubTopics{}}
	base := deviceSlot("00005e005302", "uptime")
	want := l.State(base)

	ignored := base
	ignored.Bucket = hamodel.BucketValues
	ignored.Channel = "3"
	if got := l.State(ignored); got != want {
		t.Errorf("Bucket and Channel changed the topic: %q, want %q", got, want)
	}

	for _, perturbed := range []hamodel.Slot{
		{Scope: []string{scopeClient}, Address: base.Address, Path: base.Path},
		{Scope: base.Scope, Address: "00005e005303", Path: base.Path},
		{Scope: base.Scope, Address: base.Address, Path: []string{"firmware"}},
	} {
		if got := l.State(perturbed); got == want {
			t.Errorf("a changed scope, address or path left the topic at %q; "+
				"the layout ignores the inputs it must read", got)
		}
	}

	// A scope the layout does not know renders nothing rather than a
	// plausible-looking topic nobody publishes.
	if got := l.State(hamodel.Slot{Scope: []string{"nonsense"}, Address: "x", Path: []string{"y"}}); got != "" {
		t.Errorf("an unknown scope rendered %q, want the empty string", got)
	}
}

// --- the context overrides --------------------------------------------

// TestHamqttObjectIDReproducesTheComposition is measurement F4, as an
// assertion.
//
// The entity-id seed is collapseTokens(slugify(name)+"_"+slugify(key)),
// a composition rather than a normaliser applied to one string. The
// library's own discovery.ObjectID is Slug(uid)+"_"+Slug(key), which
// differs on three independent classes — the umlaut, the hyphen and
// the token that repeats its predecessor — and the third differs for
// pure ASCII too. Each is asserted against the library's answer, so an
// override that became inert fails here.
func TestHamqttObjectIDReproducesTheComposition(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	ctx := d.newHamqttContext()

	cases := []struct {
		deviceName string
		key        string
		want       string
	}{
		{"Büro-Gateway", "cpu_utilization", "buro_gateway_cpu_utilization"},
		{"Switch Garage", "state", "switch_garage_state"},
		{"Groß-NAS", "presence", "gross_nas_presence"},
		{"Gäste", "blocked", "gaste_blocked"},
	}
	for _, tc := range cases {
		dev := hamqttDevice(deviceInfo{Identifiers: []string{"unifi_x"}, Name: tc.deviceName})
		e := &hamqttEntity{
			Basic:    hamodel.Basic{EntityKey: tc.key, EntityPlatform: hacatalog.PlatformSensor},
			seedName: tc.deviceName,
			seedKey:  tc.key,
		}
		if got := ctx.ObjectID(dev, e); got != tc.want {
			t.Errorf("ObjectID(%q, %q) = %q, want %q", tc.deviceName, tc.key, got, tc.want)
		}
		if got := discovery.ObjectID(dev, e); got == tc.want {
			t.Errorf("the library's own ObjectID also yields %q for %q/%q; the override is inert",
				got, tc.deviceName, tc.key)
		}
	}

	// collapseTokens is the half no slug function has: it runs over the
	// JOINED string. Stated directly so the premise cannot rot.
	if slugify("Switch Garage")+"_"+slugify("switch_state") == "switch_garage_state" {
		t.Error("the composition no longer differs from a per-half slug; " +
			"entityIDSeed's collapseTokens is untested by this file")
	}
}

// TestHamqttUniqueIDIsThisBridgesFormula pins the one string Home
// Assistant's entity registry keys on and has no migration path for.
func TestHamqttUniqueIDIsThisBridgesFormula(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	ctx := d.newHamqttContext()
	dev := hamqttDevice(deviceInfo{Identifiers: []string{"unifi_client_00005e005311"}, Name: "Phone"})

	// The ip sensor is one of the 21 configs where the unique id is not
	// "<node_id>_<object_id>": the topic segment is "ip", the id says
	// "client_ip".
	e := &hamqttEntity{
		Basic:   hamodel.Basic{EntityKey: "ip", EntityPlatform: hacatalog.PlatformSensor},
		uidBase: "unifi_client_00005e005311",
		uidKey:  "client_ip",
	}
	const want = "unifi_client_00005e005311_client_ip"
	if got := ctx.UniqueID(dev, e); got != want {
		t.Errorf("UniqueID = %q, want %q", got, want)
	}
	if got := discovery.UniqueID(hamqttNamespace, dev, e); got == want {
		t.Errorf("the library's own UniqueID also yields %q; the override is inert", got)
	}
	// And the component key must stay the topic segment, not the id's
	// suffix — F5's third condition.
	if e.Key() == "client_ip" {
		t.Error("the component key is the unique id's suffix; the retraction at step 6 " +
			"would then render a topic this bridge never published")
	}
}

// TestHamqttNodeIDIsTheDeviceIdentifier pins F5's first condition — and
// names it as an equivalent override rather than claiming it as a
// correction.
//
// publisher.SupersededTopics keys the retraction on the bundle's node
// id, and this bridge publishes device.identifiers[0] in that segment
// on 315 of 315 configs. go-hamqtt's own discovery.NodeID is
// topic.Slug(dev.UID()), which happens to reproduce every identifier
// this bridge composes, because they are already lower-case ASCII with
// "_" and "-". So the override changes nothing today. Both halves are
// asserted: that the default agrees on the real identifier shapes, and
// that the two differ on an identifier that is not already slug-shaped
// — otherwise the premise could rot without a word.
func TestHamqttNodeIDIsTheDeviceIdentifier(t *testing.T) {
	t.Parallel()

	ctx := hamqttTestDiscovery("en").newHamqttContext()
	for _, id := range []string{
		"unifi_00005e005302",
		"unifi_client_00005e005311",
		"unifi_client_vpn-client-uuid",
		"unifi_site_default",
	} {
		dev := hamqttDevice(deviceInfo{Identifiers: []string{id}, Name: "x"})
		if got := ctx.NodeID(dev); got != id {
			t.Errorf("NodeID = %q, want the identifier %q", got, id)
		}
		if got := discovery.NodeID(dev); got != id {
			t.Errorf("the library's default NodeID renders %q for identifier %q; the override "+
				"has stopped being equivalent and step 6 now depends on it", got, id)
		}
	}
	// The premise: the two are not the same function, they merely agree
	// on the shapes this fleet produces.
	odd := hamqttDevice(deviceInfo{Identifiers: []string{"UniFi Site Büro"}, Name: "x"})
	if ctx.NodeID(odd) == discovery.NodeID(odd) {
		t.Error("the override and topic.Slug agree even on a non-slug identifier; " +
			"this test proves nothing about the override")
	}
}

// --- availability ------------------------------------------------------

// TestHamqttAvailabilityIsTwoLevelWithATemplate is measurement F8.
//
// The library's shape is already right — a list, mode "all", resolved
// per entity — so the 128/187 split costs nothing but a
// hamodel.BridgeOnly on the 128. What differs is the second level's
// spelling: discovery.StdContext names a dedicated availability topic
// with no template, this bridge names the object's own state topic and
// maps its vocabulary. Both are asserted, so the override cannot go
// inert unnoticed.
func TestHamqttAvailabilityIsTwoLevelWithATemplate(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	g := d.hamqttDeviceGroup(testDevice(), ControlOptions{})
	ctx := d.newHamqttContext()

	// A statistics sensor: bridge + the device's own state topic.
	cpu := renderOne(t, d, g, "cpu_utilization")
	if cpu.AvailabilityMode != "all" {
		t.Errorf("availability_mode = %q, want \"all\"", cpu.AvailabilityMode)
	}
	if len(cpu.Availability) != 2 {
		t.Fatalf("%d availability sources, want 2", len(cpu.Availability))
	}
	if cpu.Availability[0].Topic != (stubTopics{}).AvailabilityTopic() {
		t.Errorf("first source = %q, want the bridge topic", cpu.Availability[0].Topic)
	}
	if cpu.Availability[0].ValueTemplate != "" {
		t.Errorf("the bridge source carries a template %q", cpu.Availability[0].ValueTemplate)
	}
	wantSecond := stubTopics{}.DeviceTopic(testDevice().MAC, "state")
	if cpu.Availability[1].Topic != wantSecond {
		t.Errorf("second source = %q, want the device's own state topic %q",
			cpu.Availability[1].Topic, wantSecond)
	}
	if cpu.Availability[1].ValueTemplate != deviceAvailTemplate {
		t.Errorf("second source template = %q, want %q",
			cpu.Availability[1].ValueTemplate, deviceAvailTemplate)
	}

	// The entity that reports the offline condition opts out of the
	// second level: making it unavailable would hide what the user
	// needs. That is the 128 of the 128/187 split.
	state := renderOne(t, d, g, "state")
	if len(state.Availability) != 1 {
		t.Errorf("the state sensor has %d availability sources, want 1 (bridge only)",
			len(state.Availability))
	}

	// And the library's own spelling is different, which is why the
	// method is overridden at all.
	std := discovery.StdContext{Layout: ctx.layout, Namespace: hamqttNamespace, Enc: discovery.RawEncoding}
	e := findEntity(t, g, "cpu_utilization")
	got := std.Availability(g.dev, e)
	if len(got) == 2 && got[1] == cpu.Availability[1] {
		t.Error("discovery.StdContext already renders this bridge's second availability level; " +
			"the override is inert and F8 has been resolved elsewhere")
	}
}

// TestHamqttBridgeOnlyIsLoadBearing asserts the other direction: the
// 128 entities that opt out do so because of an explicit
// hamodel.BridgeOnly, not because the renderer happens to drop a level.
func TestHamqttBridgeOnlyIsLoadBearing(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	g := d.hamqttDeviceGroup(testDevice(), ControlOptions{})
	e := findEntity(t, g, "state")
	if !e.Description.Availability.Has(hamodel.LevelBridge) ||
		e.Description.Availability.Has(hamodel.LevelDevice) {
		t.Fatalf("the state sensor's availability is not BridgeOnly: %v", e.Description.Availability)
	}

	// Drop the setting and the entity gains the level it must not have.
	e.Description.Availability = hamodel.Availability{}
	comp, err := discovery.RenderComponent(d.newHamqttContext(), g.dev, e, discovery.Origin{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(comp.Availability) != 2 {
		t.Errorf("without BridgeOnly the state sensor still has %d sources; the setting is inert "+
			"and 128 entities would be free to go unavailable exactly when they are needed",
			len(comp.Availability))
	}
}

// --- the encoding ------------------------------------------------------

// TestHamqttRawEncodingIsLoadBearing pins the setting that would be the
// widest silent diff of all.
//
// discovery.Encoding's zero value is EnvelopeEncoding, which attaches
// `value_template: {{ value_json.value }}` to every entity that reads a
// topic. This bridge publishes bare scalars, so the envelope template
// would break 297 entities at once — and in review it reads like a
// formatting detail.
func TestHamqttRawEncodingIsLoadBearing(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	g := d.hamqttDeviceGroup(testDevice(), ControlOptions{})
	if got := renderOne(t, d, g, "cpu_utilization").ValueTemplate; got != "" {
		t.Errorf("value_template = %q on a sensor that publishes a bare scalar", got)
	}

	envelope := d.newHamqttContext()
	envelope.Enc = discovery.EnvelopeEncoding
	comp, err := discovery.RenderComponent(envelope, g.dev, findEntity(t, g, "cpu_utilization"),
		discovery.Origin{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if comp.ValueTemplate == "" {
		t.Error("the envelope encoding renders no value_template either; " +
			"discovery.RawEncoding is inert and this file proves nothing about it")
	}

	// A binary sensor that DOES carry a template keeps its own: the
	// description wins over the encoding.
	reachable := renderOne(t, d, g, "reachable")
	if !strings.Contains(reachable.ValueTemplate, "ONLINE") {
		t.Errorf("the reachable sensor's value_template = %q, want this bridge's own",
			reachable.ValueTemplate)
	}
}

// --- device_tracker -----------------------------------------------------

// TestHamqttRendersADeviceTracker answers as much of the measurement's
// open device_tracker question as a unit test can.
//
// Three of the 315 published configs are device_tracker, a platform no
// other bridge in the programme has, and §3.4 of the measurement names
// "can a bundle carry a device_tracker" as an open live-HA question.
// The rendering half is settled here and in
// TestHamqttBundlesValidate: the library has a typed field set for the
// platform, renders all three of its keys, and discovery.Validate
// accepts it both as a body and inside a bundle. Only the runtime half
// — whether Home Assistant itself accepts the component — is left, and
// no unit test can reach it.
func TestHamqttRendersADeviceTracker(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	cl := &model.Client{
		MAC: model.MustParseMAC("00:00:5e:00:53:11"), ID: "id-phone",
		Name: "Phone", Type: model.ClientWireless,
	}
	g := d.hamqttClientGroup(cl, ClientOptions{}, ControlOptions{})
	comp := renderOne(t, d, g, "presence")

	if comp.Platform != hacatalog.PlatformDeviceTracker {
		t.Fatalf("platform = %q, want device_tracker", comp.Platform)
	}
	body, err := comp.EntityJSON()
	if err != nil {
		t.Fatalf("EntityJSON: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for key, want := range map[string]string{
		"source_type":      "router",
		"payload_home":     "home",
		"payload_not_home": "not_home",
	} {
		if obj[key] != want {
			t.Errorf("%s = %v, want %q", key, obj[key], want)
		}
	}
	if err := discovery.ValidateBody(hacatalog.PlatformDeviceTracker, obj); err != nil {
		t.Errorf("the device_tracker body is refused: %v", err)
	}
}

// --- what the library cannot express --------------------------------------

// TestHamqttCannotRenderAnEmptyManufacturer pins the one key of the 315
// the library does not reproduce, and pins the reason rather than the
// symptom.
//
// clientDeviceInfo sets Manufacturer to "" because a network client has
// none, and this repository's own field carries no `omitempty`, so 36
// client configs publish `"manufacturer": ""`.
// discovery.DeviceInfo.Manufacturer IS `omitempty`, so the library
// renders the key absent — which is what Home Assistant reads the empty
// string as anyway. Reproducing it would mean replacing the whole
// `device` block through Component.Extra, i.e. replacing exactly the
// part of the payload the experiment is testing.
//
// The fix belongs to this bridge and to step 7: dropping the key moves
// 36 retained payloads, and a migration step whose golden diff is empty
// is provable while one with 36 rows is not.
func TestHamqttCannotRenderAnEmptyManufacturer(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	cl := &model.Client{
		MAC: model.MustParseMAC("00:00:5e:00:53:11"), ID: "id-phone",
		Name: "Phone", Type: model.ClientWireless,
	}

	// The shipped builder publishes the key, empty.
	entries, err := d.Client(cl, ClientOptions{})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	var shipped map[string]any
	if err := json.Unmarshal(entries[0].Payload, &shipped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dev, _ := shipped["device"].(map[string]any)
	m, present := dev["manufacturer"]
	if !present || m != "" {
		t.Fatalf("the shipped client config no longer carries an empty manufacturer (%v); "+
			"this divergence is gone and the carve-out in "+
			"TestHamqttReproducesThePublishedConfigs must go with it", m)
	}

	// The library omits it, and there is no typed route to the empty
	// string.
	g := d.hamqttClientGroup(cl, ClientOptions{}, ControlOptions{})
	body, err := renderOne(t, d, g, "presence").EntityJSON()
	if err != nil {
		t.Fatalf("EntityJSON: %v", err)
	}
	var rendered map[string]any
	if err := json.Unmarshal(body, &rendered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rdev, _ := rendered["device"].(map[string]any)
	if _, ok := rdev["manufacturer"]; ok {
		t.Error("the library now renders an empty manufacturer; the carve-out in " +
			"TestHamqttReproducesThePublishedConfigs is obsolete and 315 of 315 should be byte-equal")
	}
	// Everything else about the device block matches, so the carve-out
	// is one key wide and not a licence.
	delete(dev, "manufacturer")
	want, _ := json.Marshal(dev)
	got, _ := json.Marshal(rdev)
	if !bytes.Equal(want, got) {
		t.Errorf("the device block differs beyond the manufacturer key\n shipped %s\n library %s",
			want, got)
	}
}

// --- the component key ----------------------------------------------------

// TestHamqttComponentKeysAreTheObjectIDSegments pins F5's third
// condition on the entities where it is not the identity function.
//
// publisher.SupersededTopics renders the object-id segment from the
// component's key inside the bundle, so on these five the key must be
// the topic segment and NOT the unique id's suffix.
func TestHamqttComponentKeysAreTheObjectIDSegments(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	cl := &model.Client{
		MAC: model.MustParseMAC("00:00:5e:00:53:11"), ID: "id-phone",
		Name: "Phone", Type: model.ClientWireless,
	}
	g := d.hamqttClientGroup(cl, ClientOptions{Signal: true}, ControlOptions{})
	for key, wantUID := range map[string]string{
		"ip":     "unifi_client_00005e005311_client_ip",
		"signal": "unifi_client_00005e005311_client_signal",
	} {
		e := findEntity(t, g, key)
		if got := d.newHamqttContext().UniqueID(g.dev, e); got != wantUID {
			t.Errorf("%s: unique id = %q, want %q", key, got, wantUID)
		}
		nodeID := d.newHamqttContext().NodeID(g.dev)
		if wantUID == nodeID+"_"+key {
			t.Errorf("%s: the unique id is <node_id>_<component key> after all; this test's "+
				"premise has changed and these are no longer among F5's 21", key)
		}
	}

	w := &model.WLAN{ID: "wlan-1", Name: "HomeNet"}
	wg := d.hamqttWLANGroup(w)
	e := findEntity(t, wg, "wlan_wlan-1")
	if got := d.newHamqttContext().UniqueID(wg.dev, e); got != "unifi_wlan_wlan-1_enabled" {
		t.Errorf("SSID switch unique id = %q", got)
	}
	if got := d.newHamqttContext().NodeID(wg.dev); got != "unifi_site_default" {
		t.Errorf("SSID switch node id = %q, want the site device", got)
	}
	// The seed of the one entity whose entity id is not derived from
	// its device's name.
	if got := d.newHamqttContext().ObjectID(wg.dev, e); got != "unifi_wlan_homenet" {
		t.Errorf("SSID switch entity-id seed = %q, want %q", got, "unifi_wlan_homenet")
	}
}

// TestHamqttSitePlaneHasNoDeviceLevelSignal names the one place the
// availability override has a second, weaker switch — and says why it
// cannot be removed.
//
// [hamqttContext.Availability] skips LevelDevice when the entity
// carries no template. On the infrastructure and client planes that is
// unreachable, because every entity there has one and
// [hamodel.BridgeOnly] is the only switch. On the site plane it is the
// guard that matters: the site device has no state topic at all, so a
// LevelDevice there would name "unifi/default/health/state", which
// nothing publishes and which would grey out all nine site entities.
//
// Dropping BridgeOnly on a site entity is therefore an *equivalent*
// mutation — the guard catches it — so the guard is asserted directly
// rather than left as an untested branch.
func TestHamqttSitePlaneHasNoDeviceLevelSignal(t *testing.T) {
	t.Parallel()

	d := hamqttTestDiscovery("en")
	g := d.hamqttHealthGroup("Default")
	e := findEntity(t, g, "wan_latency")
	if e.availTemplate != "" {
		t.Fatalf("a site entity carries an availability template %q; "+
			"this test's premise has changed", e.availTemplate)
	}

	// Drop the explicit BridgeOnly and the level still does not render,
	// because the guard refuses to name a topic nobody writes.
	e.Description.Availability = hamodel.Availability{}
	comp, err := discovery.RenderComponent(d.newHamqttContext(), g.dev, e, discovery.Origin{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(comp.Availability) != 1 {
		t.Errorf("the site entity gained a device availability level (%d sources); it would "+
			"point at a topic nothing publishes", len(comp.Availability))
	}
	// And the topic it would have pointed at is indeed not one this
	// bridge writes — the site plane publishes wan/, clients/ and
	// attributes, never a bare "state".
	l := hamqttLayout{topics: stubTopics{}}
	if got := l.Availability(healthSlot("wan/latency_ms")); got != "unifi/default/health/state" {
		t.Errorf("the site availability slot renders %q; the premise above has changed", got)
	}
}

// TestHamqttSlotPathsAreJoinedNotInterpreted asserts an inert thing
// explicitly rather than leaving it as a blind spot.
//
// [hamodel.Slot.Path] is a list of segments, and [hamqttLayout] joins
// it with "/" and hands the result to [Topics] as one key — so the
// segmentation carries no meaning here at all: a path of one element
// "port/1/poe" and a path of three render the same topic, and a
// mutation replacing [splitSuffix] with a single-element slice changes
// nothing. That is a property of this layout, not of layouts in
// general: go-hamqtt's own topic.Default puts a bucket level between
// the address and the path, where the two forms differ.
//
// Stated here so the equivalence is a recorded decision rather than an
// untested branch — and asserted in both directions, so a layout that
// started reading the segmentation fails.
func TestHamqttSlotPathsAreJoinedNotInterpreted(t *testing.T) {
	t.Parallel()

	ours := hamqttLayout{topics: stubTopics{}}
	split := hamodel.Slot{Scope: []string{scopeDevice}, Address: "00005e005302", Path: []string{"port", "1", "poe"}}
	whole := hamodel.Slot{Scope: []string{scopeDevice}, Address: "00005e005302", Path: []string{"port/1/poe"}}
	if ours.State(split) != ours.State(whole) {
		t.Errorf("this layout reads the path segmentation: %q vs %q",
			ours.State(split), ours.State(whole))
	}
	if got := ours.State(split); got != (stubTopics{}).DeviceTopic("00005e005302", "port/1/poe") {
		t.Errorf("the joined path renders %q", got)
	}
	// The premise: segmentation is not meaningless everywhere.
	def := hatopic.Default{Root: "unifi"}
	if def.State(split) == def.State(whole) {
		t.Error("topic.Default does not read the segmentation either; the inertness asserted " +
			"here is a property of layouts in general and says nothing about this one")
	}
}
