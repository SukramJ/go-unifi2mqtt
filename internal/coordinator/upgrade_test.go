// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"
	"github.com/SukramJ/go-mqtt/protocol"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// The 1.3.0 → 2.0.x upgrade, end to end, on a broker that retains.
//
// The broker starts out holding exactly what 1.3.0 left on it — the
// 1.3.0 surface golden of the full scenario, frozen as
// testdata/upgrade/v1.3.0_full.en.json — plus retained trees of
// instances this daemon must never touch. The console the upgraded
// daemon then polls has lost some of what 1.3.0 knew: two clients are
// away, a device and an SSID are gone. What is asserted is what Home
// Assistant would show, through haEvaluate, not which messages went out.

// retainedBroker is a broker in miniature: it retains, replays the
// retained matches of a filter right after a SUBSCRIBE (asynchronously,
// the way a broker's replay arrives through the client's read loop), and
// records every publish.
type retainedBroker struct {
	mu      sync.Mutex
	store   map[string][]byte
	history []published
	subs    []brokerSub
}

type brokerSub struct {
	filter string
	h      mqtt.MessageHandler
}

func newRetainedBroker(store map[string][]byte) *retainedBroker {
	return &retainedBroker{store: maps.Clone(store)}
}

func (b *retainedBroker) Publish(_ context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.history = append(b.history, published{topic: topic, payload: string(payload), qos: qos, retain: retain})
	if retain {
		if len(payload) == 0 {
			delete(b.store, topic)
		} else {
			b.store[topic] = append([]byte(nil), payload...)
		}
	}
	return nil
}

func (b *retainedBroker) Subscribe(_ context.Context, filter string, _ mqtt.QoS, h mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	b.mu.Lock()
	b.subs = append(b.subs, brokerSub{filter: filter, h: h})
	var replay []*mqtt.Message
	for topic, payload := range b.store {
		if protocol.MatchTopic(filter, topic) {
			replay = append(replay, &mqtt.Message{Topic: topic, Payload: payload, Retain: true})
		}
	}
	b.mu.Unlock()
	go func() {
		for _, m := range replay {
			h(m)
		}
	}()
	return mqtt.SubscribeResult{}, nil
}

func (b *retainedBroker) Unsubscribe(_ context.Context, filter string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = slices.DeleteFunc(b.subs, func(s brokerSub) bool { return s.filter == filter })
	return nil
}

func (b *retainedBroker) snapshot() map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.store)
}

func (b *retainedBroker) published() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.history)
}

// v130Store is the retained store 1.3.0 left behind in the full
// scenario.
func v130Store(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/upgrade/v1.3.0_full.en.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc surfaceDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	msgs := make([]published, 0, len(doc.Messages))
	for _, m := range doc.Messages {
		var payload string
		switch {
		case m.Empty:
		case m.JSON != nil:
			b, err := json.Marshal(m.JSON)
			if err != nil {
				t.Fatal(err)
			}
			payload = string(b)
		case m.Text != nil:
			payload = *m.Text
		}
		msgs = append(msgs, published{topic: m.Topic, payload: payload, retain: m.Retain})
	}
	store := retainedStore(msgs)
	if string(store["unifi/bridge/status"]) != "online" {
		t.Fatalf("the 1.3.0 store has no bridge/status online: %q", store["unifi/bridge/status"])
	}
	return store
}

// siblingTrees are retained trees the upgraded daemon must leave
// byte-identical: the same bridge under another root and under another
// site of the same root, the hobbyquaker Node.js `unifi2mqtt` under its
// own root, and another integration's config.
func siblingTrees(t *testing.T, v130 map[string][]byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	// A 1.x instance of this bridge under root "unifi2", and one under
	// root "unifi" for site "garage": their configs have this bridge's
	// unique_id and topic shape exactly, and differ only in the root or
	// the site their topics live under.
	for topic, payload := range v130 {
		if !strings.Contains(topic, "00005e005311") {
			continue
		}
		for _, sib := range []struct{ mac, from, to string }{
			{"00005e0053a1", "unifi/", "unifi2/"},
			{"00005e0053a2", "unifi/default/", "unifi/garage/"},
		} {
			tp := strings.ReplaceAll(topic, "00005e005311", sib.mac)
			p := strings.ReplaceAll(string(payload), "00005e005311", sib.mac)
			p = strings.ReplaceAll(p, "00:00:5e:00:53:11", "00:00:5e:00:53:"+sib.mac[10:])
			if !strings.HasPrefix(tp, "homeassistant/") {
				tp = strings.Replace(tp, sib.from, sib.to, 1)
			}
			p = strings.ReplaceAll(p, `"`+sib.from, `"`+sib.to)
			if sib.from == "unifi/" {
				p = strings.ReplaceAll(p, `"unifi/bridge/status"`, `"unifi2/bridge/status"`)
			}
			out[tp] = []byte(p)
		}
	}
	out["unifi2/bridge/status"] = []byte("online")
	// hobbyquaker's adapter: mqtt-smarthome 1.x topics under its root.
	out["hq/connected"] = []byte("2")
	out["hq/status/default/clients/00005e005311"] = []byte(`{"val":true}`)
	// Another integration entirely.
	const zigbee = `{"unique_id":"0x1234_temperature","state_topic":"zigbee2mqtt/0x1234",` +
		`"availability":[{"topic":"zigbee2mqtt/bridge/state"}],"device":{"identifiers":["zigbee2mqtt_0x1234"]}}`
	out["homeassistant/sensor/zigbee2mqtt_0x1234/temperature/config"] = []byte(zigbee)
	if len(out) < 10 {
		t.Fatalf("sibling fixture is too small: %d topics", len(out))
	}
	return out
}

// upgradeConsole is the console after the upgrade: the Old AP removed,
// SSID wlan-2 deleted, the phone and the guest tablet away.
func upgradeConsole(s *fakeSource) {
	surfaceDefaults(s)
	devices := surfaceDevices()[:3]
	s.devices = devices
	s.details = surfaceDetails(devices)
	s.wlans = s.wlans[:1]
	clients := surfaceClients()
	s.clients = []model.Client{clients[1]} // only the NAS is home
}

func upgradeConfig(t *testing.T) *config.Config {
	t.Helper()
	sc := surfaceScenarios()[2]
	if sc.name != "full.en" {
		t.Fatalf("scenario order moved: %s", sc.name)
	}
	cfg, err := config.Load(strings.NewReader(sc.yaml), config.MapEnv{})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startProcess is one daemon process against broker: constructed, wired
// and taken through the order Run uses for its first poll cycle.
func startProcess(t *testing.T, broker *retainedBroker, logs *logCapture) *Coordinator {
	t.Helper()
	c, _ := startProcessWith(t, broker, logs, nil)
	return c
}

// startProcessWith is [startProcess] with the console adjusted by tweak.
// A failing health poll is tolerated, as the health loop tolerates it.
func startProcessWith(
	t *testing.T,
	broker *retainedBroker,
	logs *logCapture,
	tweak func(*fakeSource),
) (*Coordinator, *fakeSource) {
	t.Helper()
	src := newFakeSource()
	upgradeConsole(src)
	if tweak != nil {
		tweak(src)
	}
	caps := capSet{}
	for _, c := range surfaceScenarios()[2].caps {
		caps[c] = true
	}
	c := New(Deps{
		Cfg:          upgradeConfig(t),
		Site:         model.Site{ID: "site-uuid", Name: "Default", Internal: "default"},
		Source:       src,
		MQTT:         broker,
		Capabilities: caps,
		Info:         model.ControllerInfo{ApplicationVersion: "10.5.67"},
		Logger:       slog.New(logs),
		Clock:        testClock,
	})
	t.Cleanup(c.Close)
	c.SetSubscriber(broker)
	c.reconcileTimeout = 100 * time.Millisecond
	c.reconcileWindow = 250 * time.Millisecond

	ctx := t.Context()
	c.OnConnect(ctx)
	for _, fn := range []func(context.Context) error{
		c.refreshStatic, c.refreshDevices, c.refreshDeviceStats, c.refreshClients,
	} {
		if err := fn(ctx); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.refreshHealth(ctx)
	return c, src
}

// migrate runs the two start-up passes Run starts beside the loops.
func migrate(t *testing.T, c *Coordinator) {
	t.Helper()
	ctx := t.Context()
	if err := c.migrateOldLayout(ctx); err != nil {
		t.Fatalf("migrateOldLayout: %v", err)
	}
	if err := c.reconcileOrphans(ctx); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
}

// homeAssistantView evaluates every config of this bridge in store.
func homeAssistantView(store, siblings map[string][]byte) map[string]haEntity {
	out := map[string]haEntity{}
	for topic, payload := range store {
		if !strings.HasPrefix(topic, "homeassistant/") || !strings.HasSuffix(topic, "/config") {
			continue
		}
		if _, sib := siblings[topic]; sib {
			continue
		}
		out[topic] = haEvaluate(topic, payload, store)
	}
	return out
}

const (
	phoneTracker   = "homeassistant/device_tracker/unifi_client_00005e005311/presence/config"
	phoneIP        = "homeassistant/sensor/unifi_client_00005e005311/ip/config"
	phoneSignal    = "homeassistant/sensor/unifi_client_00005e005311/signal/config"
	phoneBlocked   = "homeassistant/switch/unifi_client_00005e005311/blocked/config"
	guestTracker   = "homeassistant/device_tracker/unifi_client_00005e005313/presence/config"
	guestAuthorize = "homeassistant/button/unifi_client_00005e005313/authorize/config"
	nasTracker     = "homeassistant/device_tracker/unifi_client_00005e005312/presence/config"
	nasBlocked     = "homeassistant/switch/unifi_client_00005e005312/blocked/config"
	wlan1Switch    = "homeassistant/switch/unifi_site_default/wlan_wlan-1/config"
	wlan2Switch    = "homeassistant/switch/unifi_site_default/wlan_wlan-2/config"
)

// assertRepaired is the end state the fix promises, whatever the path
// to it: every entity of this bridge re-pointed with a correct state,
// or retracted.
func assertRepaired(t *testing.T, store, siblings map[string][]byte) {
	t.Helper()
	view := homeAssistantView(store, siblings)

	// Unavailable is right for exactly these: an absent client's IP and
	// signal readings are stale, as they were under 1.3.0.
	unavailable := map[string]bool{
		phoneIP: true, phoneSignal: true,
		"homeassistant/sensor/unifi_client_00005e005313/ip/config":     true,
		"homeassistant/sensor/unifi_client_00005e005313/signal/config": true,
	}
	for _, topic := range sortedKeys(view) {
		e := view[topic]
		for _, p := range e.problems {
			t.Errorf("%s: %s", topic, p)
		}
		if e.available == unavailable[topic] {
			t.Errorf("%s: available = %v, want %v", topic, e.available, !unavailable[topic])
		}
		body := string(store[topic])
		if strings.Contains(body, `"unifi/bridge/`) || strings.Contains(body, `"unifi/default/`) {
			t.Errorf("%s still points at the 1.x layout: %s", topic, body)
		}
	}

	for topic, want := range map[string]string{
		phoneTracker: "not_home", guestTracker: "not_home", nasTracker: "home",
		phoneBlocked: "off", nasBlocked: "on", wlan1Switch: "on",
	} {
		if got := view[topic].state; got != want {
			t.Errorf("%s: state %q, want %q", topic, got, want)
		}
	}
	if _, ok := view[guestAuthorize]; !ok {
		t.Errorf("%s was not kept", guestAuthorize)
	}

	// Retracted: the device and the SSID the console no longer has.
	for topic := range view {
		if strings.Contains(topic, surfOldMAC.String()) || topic == wlan2Switch {
			t.Errorf("%s survived, but its object is gone from the console", topic)
		}
	}

	// Nothing of the 1.x layout is left under this instance's name.
	for topic := range store {
		if _, sib := siblings[topic]; sib {
			continue
		}
		if strings.HasPrefix(topic, "unifi/default/") || strings.HasPrefix(topic, "unifi/bridge/") {
			t.Errorf("1.x item %s is still retained", topic)
		}
	}

	// And every sibling is exactly as it was.
	for topic, want := range siblings {
		if got, ok := store[topic]; !ok || !bytes.Equal(got, want) {
			t.Errorf("sibling %s was touched: %q, want %q", topic, got, want)
		}
	}
}

// Upgrading straight from 1.3.0, with clients away and objects gone,
// then a second start that has nothing left to do.
func TestUpgradeFrom130RepointsEveryEntity(t *testing.T) {
	t.Parallel()

	v130 := v130Store(t)
	siblings := siblingTrees(t, v130)
	initial := maps.Clone(v130)
	maps.Copy(initial, siblings)
	broker := newRetainedBroker(initial)

	logs := &logCapture{}
	c := startProcess(t, broker, logs)
	migrate(t, c)
	assertRepaired(t, broker.snapshot(), siblings)

	rec, ok := logs.find("coordinator.migration_retracted")
	if !ok || rec.level != slog.LevelInfo {
		t.Errorf("no migration_retracted line at info: %+v", rec)
	}
	for _, want := range []string{surfOldMAC.String(), "wlan-2"} {
		if !logs.contains(want) {
			t.Errorf("no log line names %s", want)
		}
	}
	if _, ok := logs.find("coordinator.migration_repointed"); !ok {
		t.Error("no migration_repointed line")
	}

	// The second start: a fresh process, the same broker and console.
	// Its own first cycle publishes as any start does; the migration
	// finds nothing of the 1.x layout and writes nothing.
	second := startProcess(t, broker, &logCapture{})
	before := broker.published()
	migrate(t, second)
	if after := broker.published(); after != before {
		broker.mu.Lock()
		extra := slices.Clone(broker.history[before:])
		broker.mu.Unlock()
		t.Errorf("second start's migration published %d messages: %+v", after-before, extra)
	}
	assertRepaired(t, broker.snapshot(), siblings)
}

// An installation already on 2.0.0 — which re-pointed the present
// objects, evicted `bridge/status` and left the absent clients on their
// 1.x configs — is repaired by the fixed release without
// `bridge/status`.
func TestUpgradeFrom200RepairsAbsentClients(t *testing.T) {
	t.Parallel()

	v130 := v130Store(t)
	siblings := siblingTrees(t, v130)
	initial := maps.Clone(v130)
	maps.Copy(initial, siblings)
	broker := newRetainedBroker(initial)

	// 2.0.0: the same first cycle, and a migration that is only the 1.x
	// status sweep.
	v200 := startProcess(t, broker, &logCapture{})
	if err := v200.migrationRound(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	store := broker.snapshot()
	if _, ok := store["unifi/bridge/status"]; ok {
		t.Fatal("2.0.0 emulation left bridge/status")
	}
	// The defect, as Home Assistant saw it.
	if e := haEvaluate(phoneTracker, store[phoneTracker], store); e.available {
		t.Fatalf("2.0.0 emulation: the absent phone's tracker is available (%+v); the fixture no longer shows the defect", e)
	}

	fixed := startProcess(t, broker, &logCapture{})
	migrate(t, fixed)
	assertRepaired(t, broker.snapshot(), siblings)
}

// A class whose source has not reported is left alone until it does:
// with the classic layer down at start, the 1.x site-health configs are
// neither re-pointed nor retracted, and the migration settles them in a
// later round once the health poll answers.
func TestMigrationWaitsForAClassToReport(t *testing.T) {
	t.Parallel()

	broker := newRetainedBroker(v130Store(t))
	c, src := startProcessWith(t, broker, &logCapture{}, func(s *fakeSource) {
		s.healthErr = errors.New("classic login refused")
	})
	if c.readyClasses()[hass.ClassSite] {
		t.Fatal("site class ready without the health poll")
	}

	const wan = "homeassistant/binary_sensor/unifi_site_default/wan_connectivity/config"
	done := make(chan error, 1)
	go func() { done <- c.migrateOldLayout(t.Context()) }()

	// Past the first round: the other classes are settled, the site's
	// configs are still the 1.x ones.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(string(broker.snapshot()[phoneTracker]), `"unifi/connected"`) {
		if time.Now().After(deadline) {
			t.Fatal("the first round never re-pointed the phone")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := string(broker.snapshot()[wan]); !strings.Contains(got, `"unifi/bridge/status"`) {
		t.Fatalf("%s was settled while its class had not reported: %q", wan, got)
	}

	src.mu.Lock()
	src.healthErr = nil
	src.mu.Unlock()
	if err := c.refreshHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the migration did not finish once every class had reported")
	}
	if got := string(broker.snapshot()[wan]); !strings.Contains(got, `"unifi/connected"`) {
		t.Errorf("%s is not in the 2.x layout: %q", wan, got)
	}
}

// Ownership of a 1.x config is exact: the predicate against this
// instance's own configs, and against every near miss.
func TestParseLegacyConfig(t *testing.T) {
	t.Parallel()

	v130 := v130Store(t)
	var own int
	for topic, payload := range v130 {
		if !strings.HasPrefix(topic, "homeassistant/") {
			continue
		}
		if _, ok := parseLegacyConfig("unifi", "default", "default", topic, payload); !ok {
			t.Errorf("1.3.0 config %s not recognised as this instance's", topic)
		}
		own++
	}
	if own < 60 {
		t.Fatalf("only %d configs in the 1.3.0 store", own)
	}

	base := string(v130[phoneBlocked])
	for name, tc := range map[string]struct {
		root, site, siteRef, topic, payload string
	}{
		"another root":      {"unifi2", "default", "default", phoneBlocked, base},
		"another site":      {"unifi", "garage", "garage", phoneBlocked, base},
		"another platform":  {"unifi", "default", "default", strings.Replace(phoneBlocked, "/switch/", "/light/", 1), base},
		"node id mismatch":  {"unifi", "default", "default", strings.Replace(phoneBlocked, "005311", "005399", 1), base},
		"unique_id foreign": {"unifi", "default", "default", phoneBlocked, strings.Replace(base, `"unifi_client_00005e005311_blocked"`, `"other_blocked"`, 1)},
		"2.x availability":  {"unifi", "default", "default", phoneBlocked, strings.Replace(base, `"unifi/bridge/status"`, `"unifi/connected"`, 1)},
		"extra payload key": {"unifi", "default", "default", phoneBlocked, strings.Replace(base, `{"topic":"unifi/bridge/status"}`, `{"payload_available":"online","topic":"unifi/bridge/status"}`, 1)},
		"other object's state": {"unifi", "default", "default", phoneBlocked, strings.Replace(base,
			`"unifi/default/client/00005e005311/blocked"`, `"unifi/default/client/00005e005312/blocked"`, 1)},
		"new-layout state": {"unifi", "default", "default", phoneBlocked, strings.Replace(base,
			`"unifi/default/client/00005e005311/blocked"`, `"unifi/status/default/client/00005e005311/blocked"`, 1)},
		"not JSON": {"unifi", "default", "default", phoneBlocked, "online"},
	} {
		if _, ok := parseLegacyConfig(tc.root, tc.site, tc.siteRef, tc.topic, []byte(tc.payload)); ok {
			t.Errorf("%s: recognised as this instance's 1.x config", name)
		}
	}
}

// legacyClient rebuilds what the old configs and items carry, and
// "unknown" stays unknown: no `blocked` item means no blocked value.
func TestLegacyClientReconstruction(t *testing.T) {
	t.Parallel()

	v130 := v130Store(t)
	h := newHarnessWith(t, upgradeConfig(t), allCaps{})
	var cfgs []legacyConfig
	for topic, payload := range v130 {
		if cfg, ok := parseLegacyConfig("unifi", "default", "default", topic, payload); ok && cfg.id == "00005e005313" {
			cfgs = append(cfgs, cfg)
		}
	}
	cl, blocked := h.c.legacyClient("00005e005313", cfgs, v130)
	if cl.Name != "Guest Tablet" || !cl.IsGuest || cl.Type != model.ClientWireless ||
		cl.MAC.String() != "00005e005313" || cl.UplinkMAC != surfApMAC || cl.SSID != "IoT" {
		t.Errorf("rebuilt %+v", cl)
	}
	if blocked == nil || *blocked {
		t.Errorf("blocked = %v, want a known false", blocked)
	}

	delete(v130, "unifi/default/client/00005e005313/blocked")
	if _, blocked := h.c.legacyClient("00005e005313", cfgs, v130); blocked != nil {
		t.Errorf("blocked = %v without a retained item, want nil", *blocked)
	}
}

// A config this process has published since the window read it is a
// live entity of this run, whatever the window saw: the pass neither
// re-points nor retracts it.
func TestRepointLeavesWhatThisProcessPublished(t *testing.T) {
	t.Parallel()

	v130 := v130Store(t)
	broker := newRetainedBroker(v130)
	c := startProcess(t, broker, &logCapture{})

	// The Old AP is gone from the console, so its 1.x configs would be
	// retracted — unless this process has published the topic itself.
	const live = "homeassistant/binary_sensor/unifi_00005e005304/reachable/config"
	if err := c.pub.publishConfig(t.Context(), live, []byte(`{"live":true}`)); err != nil {
		t.Fatal(err)
	}
	stale := map[string][]byte{live: v130[live]}
	res := c.repointLegacyConfigs(t.Context(), stale, v130, map[hass.Class]bool{hass.ClassDevice: true})
	if res.retracted != 0 || res.repointed != 0 {
		t.Errorf("settled a config this process published: %+v", res)
	}
	if got := string(broker.snapshot()[live]); got != `{"live":true}` {
		t.Errorf("%s = %q, want the live config", live, got)
	}
}
