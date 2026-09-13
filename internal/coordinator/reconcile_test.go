// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
)

// Unsubscribe completes the inbound contract the reconcile asserts for.
// Recorded so a test can prove the sweep does not leave the discovery
// prefix subscribed for the process lifetime.
func (s *fakeSubscriber) Unsubscribe(_ context.Context, filter string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribed = append(s.unsubscribed, filter)
	return nil
}

// deliverRetained replays retained payloads to the handler registered
// for filter, the way a broker does right after a subscribe. It waits
// for the subscription first, because the reconcile runs concurrently
// with the test.
func (s *fakeSubscriber) deliverRetained(t *testing.T, filter string, msgs map[string][]byte) {
	t.Helper()

	var h mqtt.MessageHandler
	deadline := time.Now().Add(2 * time.Second)
	for h == nil {
		if time.Now().After(deadline) {
			t.Fatalf("no subscription to %s appeared", filter)
		}
		s.mu.Lock()
		for i, f := range s.filters {
			if f == filter {
				h = s.handlers[i]
			}
		}
		s.mu.Unlock()
		if h == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	for topic, payload := range msgs {
		h(&mqtt.Message{Topic: topic, Payload: payload})
	}
}

// reconcileHarness is a harness whose sweep timings are short enough to
// run in a test, wired to a subscriber that can replay retained configs.
func newReconcileHarness(t *testing.T, cfg *config.Config) (*harness, *fakeSubscriber) {
	t.Helper()

	h := newHarness(t, cfg)
	sub := &fakeSubscriber{}
	h.c.SetSubscriber(sub)
	h.c.reconcileTimeout = 100 * time.Millisecond
	// The collection window has to outlast deliverRetained's 5 ms poll
	// for the subscription by a comfortable margin. At 20 ms it did not
	// always — on a loaded runner, and on Windows, whose timer
	// granularity is of the same order — and the sweep then ran against
	// an empty broker and cleared nothing, which reads exactly like a
	// broken sweep rather than like a lost race.
	h.c.reconcileWindow = 250 * time.Millisecond
	return h, sub
}

// ownConfig is a discovery payload that passes the ownership test: our
// id namespace plus this bridge's availability topic.
func ownConfig(c *Coordinator, uniqueID string) []byte {
	return []byte(`{"unique_id":"` + uniqueID +
		`","availability":[{"topic":"` + c.AvailabilityTopic() + `"}]}`)
}

// The core of the feature, restated: the sweep retracts what this
// process published and has since given up, and nothing else.
//
// Everything in `retained` below other than `givenUp` carries at least
// one mark that puts it out of reach — and `pastRun` carries none at
// all, which is the point: it has this daemon's id namespace, this
// daemon's availability topic and a config topic this daemon's own
// filter matches, and it is still not cleared, because this process did
// not publish it.
func TestReconcileRetractsOnlyWhatThisProcessPublished(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	ctx := t.Context()

	// Announce the current set, exactly as the static loop does.
	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(ctx); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	live := "homeassistant/sensor/unifi_00005e005302/state/config"
	announced := h.c.pub.announcedConfigs()
	if !announced[live] {
		t.Fatalf("test setup: %s was not announced", live)
	}

	const (
		// Published by this process earlier in this same run and then
		// given up — a port that vanished, an entity a filter stopped
		// matching. The broker still has the old payload because the
		// retraction did not stick, and retrying it is what the sweep is
		// for now.
		givenUp = "homeassistant/sensor/unifi_00005e005302/port_9_poe/config"
		// Left by an earlier run of this same daemon, or published by a
		// second console on this same root. Indistinguishable, so
		// untouchable.
		pastRun = "homeassistant/sensor/unifi_00005e0053ff/state/config"
		foreign = "homeassistant/sensor/zigbee_lamp/state/config"
		// A second instance of this daemon on the same broker under its
		// own MQTT root. Its ids look exactly like ours; the availability
		// topic separates them.
		otherRoot = "homeassistant/sensor/unifi_00005e0053aa/state/config"
	)

	// Claim givenUp and then give it up, through the real publish path.
	if err := h.c.pub.publishConfig(ctx, givenUp, ownConfig(h.c, "unifi_00005e005302_port_9_poe")); err != nil {
		t.Fatalf("publishConfig: %v", err)
	}
	if err := h.c.clearConfig(ctx, givenUp); err != nil {
		t.Fatalf("clearConfig: %v", err)
	}
	if h.c.pub.announcedConfigs()[givenUp] {
		t.Fatal("test setup: the given-up config is still announced")
	}

	h.broker.reset()

	done := make(chan error, 1)
	go func() { done <- h.c.reconcileOrphans(ctx) }()

	sub.deliverRetained(t, h.c.hass.ConfigFilter(), map[string][]byte{
		live:    ownConfig(h.c, "unifi_00005e005302_state"),
		givenUp: ownConfig(h.c, "unifi_00005e005302_port_9_poe"),
		pastRun: ownConfig(h.c, "unifi_00005e0053ff_state"),
		foreign: []byte(`{"unique_id":"zigbee_lamp_state"}`),
		otherRoot: []byte(`{"unique_id":"unifi_00005e0053aa_state",` +
			`"availability":[{"topic":"unifi-garage/bridge/status"}]}`),
	})

	if err := <-done; err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}

	// The sweep must still be able to clear something, or every
	// assertion below passes vacuously.
	if payload, ok := h.broker.latest(givenUp); !ok || payload != "" {
		t.Errorf("a config this process published and gave up: payload %q, published %v "+
			"— want an empty retained clear", payload, ok)
	}
	for name, topic := range map[string]string{
		"a currently announced config":          live,
		"a config this process never published": pastRun,
		"another integration's config":          foreign,
		"a second bridge instance's config":     otherRoot,
	} {
		if _, ok := h.broker.latest(topic); ok {
			t.Errorf("%s was cleared (%s)", name, topic)
		}
	}
}

// The sweep must not outlive its own subscription: left in place, every
// later discovery publish would loop back through the read loop.
func TestReconcileUnsubscribes(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	ctx := t.Context()

	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(ctx); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if err := h.c.reconcileOrphans(ctx); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	want := h.c.hass.ConfigFilter()
	for _, f := range sub.unsubscribed {
		if f == want {
			return
		}
	}
	t.Errorf("unsubscribed = %v, want %s among them", sub.unsubscribed, want)
}

// The failure this design exists to prevent. Before the device poll has
// reported, the announced set is empty — and a sweep that read that as
// "nothing is ours any more" would delete every device entity on the
// broker, taking its history with it.
func TestReconcileNeverSweepsBeforeItsSourceReported(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	ctx := t.Context()

	const device = "homeassistant/sensor/unifi_00005e005302/state/config"

	done := make(chan error, 1)
	go func() { done <- h.c.reconcileOrphans(ctx) }()

	sub.deliverRetained(t, h.c.hass.ConfigFilter(), map[string][]byte{
		device: ownConfig(h.c, "unifi_00005e005302_state"),
	})

	if err := <-done; err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
	if _, ok := h.broker.latest(device); ok {
		t.Error("a device config was cleared before the device poll had reported")
	}
}

// A device poll that comes back empty is far more often a broken
// permission than a genuinely empty site, so it must not count as a
// complete picture.
func TestEmptyDevicePollDoesNotMarkDevicesReady(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	h.src.devices = nil

	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if h.c.readyClasses()[hass.ClassDevice] {
		t.Error("devices are ready after a poll that returned none")
	}
}

// Readiness follows what is enabled. A source that is off produces no
// entities by definition, so its leftovers are exactly what the sweep
// should remove — while one that is on but has not answered yet must
// hold the sweep back.
func TestReadyClassesFollowConfiguration(t *testing.T) {
	t.Parallel()

	t.Run("clients off are immediately sweepable", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t, hassConfig(t))
		if !h.c.readyClasses()[hass.ClassClient] {
			t.Error("clients not ready although client publication is off")
		}
	})

	t.Run("clients on wait for the first poll", func(t *testing.T) {
		t.Parallel()

		cfg := hassConfig(t)
		cfg.Clients.Enable = true
		h := newHarness(t, cfg)

		if h.c.readyClasses()[hass.ClassClient] {
			t.Fatal("clients ready before the first client poll")
		}
		if err := h.c.refreshClients(t.Context()); err != nil {
			t.Fatalf("refreshClients: %v", err)
		}
		if !h.c.readyClasses()[hass.ClassClient] {
			t.Error("clients still not ready after a successful poll")
		}
	})

	t.Run("site health waits for the classic layer", func(t *testing.T) {
		t.Parallel()

		cfg := hassConfig(t)
		cfg.ClassicEnable = true
		h := newHarness(t, cfg)

		if h.c.readyClasses()[hass.ClassSite] {
			t.Error("site ready although the classic layer has not answered")
		}
		h.c.healthAnnounced.Store(true)
		if !h.c.readyClasses()[hass.ClassSite] {
			t.Error("site not ready after health was announced")
		}
	})
}

// HASS_CLEANUP is the operator's off switch; with it off nothing may be
// read or cleared.
func TestReconcileDisabled(t *testing.T) {
	t.Parallel()

	cfg := hassConfig(t)
	cfg.HASSCleanup = false
	h, sub := newReconcileHarness(t, cfg)

	if err := h.c.reconcileOrphans(t.Context()); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.filters) != 0 {
		t.Errorf("subscribed to %v with cleanup disabled", sub.filters)
	}
}

// Change detection suppresses a republished config, and the announced
// set must survive that — otherwise the sweep reads a live entity back
// off the broker as an orphan and deletes it.
func TestAnnouncedConfigsSurviveChangeDetection(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	ctx := t.Context()

	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	first := h.c.pub.announcedConfigs()

	// The identical second pass publishes nothing new.
	h.broker.reset()
	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	configs := h.broker.topicsWithPrefix("homeassistant/")
	if len(configs) != 0 {
		t.Fatalf("test setup: %d configs republished, want change detection to suppress them", len(configs))
	}

	if got := h.c.pub.announcedConfigs(); len(got) != len(first) {
		t.Errorf("announced configs = %d after a suppressed republish, want %d", len(got), len(first))
	}
}

// Clearing a config drops the claim, so a later sweep does not try to
// clear it again.
func TestClearedConfigLeavesTheAnnouncedSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	ctx := t.Context()

	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	var topic string
	for t := range h.c.pub.announcedConfigs() {
		if strings.HasSuffix(t, "/state/config") {
			topic = t
			break
		}
	}
	if topic == "" {
		t.Fatal("test setup: no state config was announced")
	}

	if err := h.c.clearConfig(ctx, topic); err != nil {
		t.Fatalf("clearConfig: %v", err)
	}
	if h.c.pub.announcedConfigs()[topic] {
		t.Errorf("%s is still announced after being cleared", topic)
	}
	// The claim, unlike the announcement, survives. That is what lets
	// the sweep retry a retraction that did not stick — and it is the
	// only route by which the sweep can clear anything at all now.
	if !h.c.pub.claims().Published[topic] {
		t.Errorf("%s left the claim list when it was cleared; a retraction "+
			"that did not stick could then never be retried", topic)
	}
}

// The claim list is process-local and first-hand: it records what this
// process put on the broker, never what it read back off it. A fresh
// process claims nothing, which is why the sweep runs only after the
// first publish pass — see Coordinator.awaitReady.
func TestAFreshProcessClaimsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	if got := h.c.pub.claims(); len(got.Published) != 0 || len(got.Announced) != 0 {
		t.Errorf("a process that has published nothing claims %v / %v", got.Published, got.Announced)
	}

	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	got := h.c.pub.claims()
	if len(got.Published) == 0 {
		t.Fatal("the publish pass claimed nothing")
	}
	for topic := range got.Announced {
		if !got.Published[topic] {
			t.Errorf("%s is announced but not published — Announced must be a "+
				"subset of Published or the sweep can retract an unclaimed topic", topic)
		}
	}
}

// TestOwnershipCannotSeparateTwoConsolesOnOneRoot is the hard case, and
// it now asserts the fix rather than recording the defect.
//
// go-mtec2mqtt's F4 (its PR #54) established the rule the programme
// settled on: require the payload's state topic to sit under the
// publishing instance's own identity, and claim nothing before that
// identity is known. **This bridge has no such identity.** A state
// topic here is `<root>/<site>/…`; the root is MQTT_TOPIC, which is
// what the availability-topic check already reads, and the site segment
// is `Site.Internal`, which is `default` on every UniFi console out of
// the box. Two instances bridging two different consoles on one broker
// with the shipped configuration therefore produce byte-identical state
// topics, config topics, availability topics and unique_ids for the
// whole site plane, and no predicate over those strings separates them.
//
// PR #23 measured what that cost: this same test, driving this same
// sweep, recorded that one console's daemon **cleared the other
// console's SSID switch out of Home Assistant**. Home Assistant reports
// nothing when an entity disappears with its retained config, so the
// only symptom was entities gone.
//
// The rule is now a claim list instead of a predicate: the sweep may
// retract only a topic this process published since it started. The
// sibling console's switch fails that by construction, whatever it
// looks like — which is why the setup below still asserts that it looks
// exactly like ours. If it stopped doing so, this test would pass while
// measuring nothing.
func TestOwnershipCannotSeparateTwoConsolesOnOneRoot(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	ctx := t.Context()

	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(ctx); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	// A second console's SSID switch, published by a second instance of
	// this daemon under the *same* MQTT root. Its unique_id is in our
	// namespace, its availability topic is ours because the root is
	// ours, and its state topic is under `unifi/default/` because the
	// site segment is `default` on every console. There is nothing in
	// it that is not also true of ours.
	const sibling = "homeassistant/switch/unifi_site_default/wlan_other-console-ssid/config"
	payload := []byte(`{"unique_id":"unifi_wlan_other-console-ssid_enabled",` +
		`"state_topic":"unifi/default/wlan/other-console-ssid/enabled",` +
		`"availability":[{"topic":"` + h.c.AvailabilityTopic() + `"}]}`)

	if !h.c.hass.IsOwnConfig(payload) {
		t.Fatal("setup: the sibling console's config no longer has this " +
			"daemon's shape, so this test no longer measures the hard case")
	}
	if h.c.pub.claims().Published[sibling] {
		t.Fatal("setup: this process published the sibling's topic, so this " +
			"test no longer measures the hard case")
	}
	if len(h.c.pub.claims().Published) == 0 {
		t.Fatal("setup: this process claimed nothing, so a sweep that clears " +
			"nothing would prove nothing")
	}

	h.broker.reset()
	done := make(chan error, 1)
	go func() { done <- h.c.reconcileOrphans(ctx) }()
	sub.deliverRetained(t, h.c.hass.ConfigFilter(), map[string][]byte{sibling: payload})
	if err := <-done; err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}

	if payloadOut, cleared := h.broker.latest(sibling); cleared {
		t.Errorf("the other console's SSID switch was cleared (payload %q). "+
			"This process never published that topic, so it is not ours to "+
			"retract — a Home Assistant user sees the entity disappear with "+
			"nothing in any log to explain it", payloadOut)
	}
}

// TestAStaggeredUpgradeDoesNotDeleteTheSiblingsFleet is the shape that
// falsified go-mtec2mqtt's "harmless" verdict one PR after it was
// written, and that go-homeconnect2mqtt pinned in its PR #44: instance
// A is upgraded and publishes a *different* catalogue, so its announced
// set no longer names the topics the not-yet-upgraded instance B still
// owns and keeps republishing.
//
// Here A and B bridge two different UniFi consoles to one broker under
// one MQTT root — the configuration in which every identity string of
// the site plane collides — and B's whole fleet is the retained tree A
// sweeps against. A must clear none of it.
func TestAStaggeredUpgradeDoesNotDeleteTheSiblingsFleet(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	ctx := t.Context()

	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(ctx); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	// The sibling's fleet: one device of its own with a full entity set,
	// its site-health plane and two SSIDs. Every payload is exactly what
	// this daemon publishes, because the sibling *is* this daemon.
	fleet := map[string][]byte{}
	for _, key := range []string{"state", "uptime", "cpu", "memory", "reachable"} {
		fleet["homeassistant/sensor/unifi_00005e00daad/"+key+"/config"] = ownConfig(h.c, "unifi_00005e00daad_"+key)
	}
	for _, key := range []string{"wan_state", "wan_connectivity", "clients"} {
		fleet["homeassistant/sensor/unifi_site_default/"+key+"/config"] = ownConfig(h.c, "unifi_site_"+key)
	}
	for _, ssid := range []string{"other-console-guest", "other-console-iot"} {
		fleet["homeassistant/switch/unifi_site_default/wlan_"+ssid+"/config"] = ownConfig(h.c, "unifi_wlan_"+ssid+"_enabled")
	}

	announced := h.c.pub.announcedConfigs()
	for topic := range fleet {
		if announced[topic] {
			t.Fatalf("setup: %s is one of this instance's own configs, so the "+
				"fleet is not the sibling's", topic)
		}
	}

	h.broker.reset()
	done := make(chan error, 1)
	go func() { done <- h.c.reconcileOrphans(ctx) }()
	sub.deliverRetained(t, h.c.hass.ConfigFilter(), fleet)
	if err := <-done; err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}

	var cleared []string
	for topic := range fleet {
		if _, ok := h.broker.latest(topic); ok {
			cleared = append(cleared, topic)
		}
	}
	if len(cleared) > 0 {
		slices.Sort(cleared)
		t.Errorf("a staggered upgrade retracted %d of %d topics (%v) — "+
			"that is the not-yet-upgraded sibling instance's fleet",
			len(cleared), len(fleet), cleared)
	}
}
