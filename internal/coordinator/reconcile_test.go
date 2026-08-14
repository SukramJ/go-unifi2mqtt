// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
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
	h.c.reconcileWindow = 20 * time.Millisecond
	return h, sub
}

// ownConfig is a discovery payload that passes the ownership test: our
// id namespace plus this bridge's availability topic.
func ownConfig(c *Coordinator, uniqueID string) []byte {
	return []byte(`{"unique_id":"` + uniqueID +
		`","availability":[{"topic":"` + c.AvailabilityTopic() + `"}]}`)
}

// The core of the feature: a config left on the broker by an earlier run
// is cleared, while the ones this run announces are not.
func TestReconcileClearsStaleConfigsOnly(t *testing.T) {
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
		stale   = "homeassistant/sensor/unifi_00005e0053ff/state/config"
		foreign = "homeassistant/sensor/zigbee_lamp/state/config"
		// A second instance of this daemon on the same broker under its
		// own MQTT root. Its ids look exactly like ours; only the
		// availability topic separates them.
		sibling = "homeassistant/sensor/unifi_00005e0053aa/state/config"
	)
	h.broker.reset()

	done := make(chan error, 1)
	go func() { done <- h.c.reconcileOrphans(ctx) }()

	sub.deliverRetained(t, h.c.hass.ConfigFilter(), map[string][]byte{
		live:    ownConfig(h.c, "unifi_00005e005302_state"),
		stale:   ownConfig(h.c, "unifi_00005e0053ff_state"),
		foreign: []byte(`{"unique_id":"zigbee_lamp_state"}`),
		sibling: []byte(`{"unique_id":"unifi_00005e0053aa_state",` +
			`"availability":[{"topic":"unifi-garage/bridge/status"}]}`),
	})

	if err := <-done; err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}

	if payload, ok := h.broker.latest(stale); !ok || payload != "" {
		t.Errorf("stale config: payload %q, published %v — want an empty retained clear", payload, ok)
	}
	if _, ok := h.broker.latest(live); ok {
		t.Error("a currently announced config was cleared")
	}
	if _, ok := h.broker.latest(foreign); ok {
		t.Error("another integration's config was cleared")
	}
	if _, ok := h.broker.latest(sibling); ok {
		t.Error("a second bridge instance's config was cleared")
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
		t.Errorf("%s is still claimed after being cleared", topic)
	}
}
