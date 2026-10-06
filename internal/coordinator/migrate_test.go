// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"slices"
	"testing"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"
)

// replay delivers messages to the handler subscribed for filter, the
// way a broker replays its retained store right after a SUBSCRIBE. It
// waits for the subscription, because the sweep runs concurrently.
func replay(t *testing.T, sub *fakeSubscriber, filter string, msgs []*mqtt.Message) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		sub.mu.Lock()
		var h mqtt.MessageHandler
		for i, f := range sub.filters {
			if f == filter {
				h = sub.handlers[i]
			}
		}
		sub.mu.Unlock()
		if h != nil {
			for _, m := range msgs {
				h(m)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no subscription to %s appeared", filter)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func retained(topic string) *mqtt.Message {
	return &mqtt.Message{Topic: topic, Payload: []byte("x"), Retain: true}
}

// The retained sweep of ADR 0083 against a broker holding this
// instance's 1.x leftovers beside everything it must not touch: a
// sibling instance under another name, another site under the same
// name, a device this console does not have, the new layout, and
// messages that are not retained leftovers at all.
func TestMigrationSweepClearsOnlyThisInstancesOldLayout(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, controlConfig(t, ""))
	h.c.caps = allCaps{}
	withSampleClients(h)
	ctx := t.Context()
	for _, fn := range []func() error{
		func() error { return h.c.refreshStatic(ctx) },
		func() error { return h.c.refreshDevices(ctx) },
		func() error { return h.c.refreshClients(ctx) },
	} {
		if err := fn(); err != nil {
			t.Fatalf("warming the fixture: %v", err)
		}
	}

	ownBridge := []string{"unifi/bridge/status", "unifi/bridge/info"}
	// `bridge/error` was never retained; whatever is there is not ours.
	foreignBridge := []string{"unifi/bridge/error", "unifi/bridge/state"}
	own := []string{
		"unifi/default/device/00005e005302/state",
		"unifi/default/device/00005e005302/update_available",
		"unifi/default/device/00005e005302/attributes",
		"unifi/default/device/00005e005302/locate",
		"unifi/default/device/00005e005302/port/1/poe",
		"unifi/default/device/00005e005302/port/1/poe/power_w",
		"unifi/default/device/00005e005302/port/25/speed",
		"unifi/default/device/00005e005302/cmd/locate/set",
		"unifi/default/device/00005e005302/port/1/cmd/power_cycle",
		"unifi/default/device/00005e005303/radio/5g/channel",
		"unifi/default/device/00005e005301/cpu_utilization",
		"unifi/default/client/00005e005310/state",
		"unifi/default/client/00005e005310/ip",
		"unifi/default/client/00005e005310/blocked/set",
		"unifi/default/wlan/wlan-1/enabled",
		"unifi/default/wlan/wlan-1/name",
		"unifi/default/health/wan/state",
		"unifi/default/health/clients/total",
		"unifi/default/health/attributes",
	}
	survive := []string{
		// A device, client and SSID this console never reported: a
		// second console under the same name and site, or an object
		// that is simply away this run. Never a prefix match.
		"unifi/default/device/00005e0053ff/state",
		"unifi/default/client/00005e0053fe/state",
		"unifi/default/wlan/other-console-ssid/enabled",
		// Below a known device, but not a 1.x item.
		"unifi/default/device/00005e005302/something_else",
		"unifi/default/device/00005e005302/port/x/state",
		"unifi/default/device/00005e005302/radio/5g/channel/extra",
		"unifi/default/health/unknown",
	}
	// The new layout under the same name. The sweep never subscribes it,
	// but even delivered it must not be judged old.
	newLayout := []string{
		"unifi/status/default/device/00005e005302/state",
		"unifi/status/bridge/error",
		"unifi/set/default/device/00005e005302/cmd/restart",
	}

	done := make(chan struct{})
	var cleared int
	var err error
	go func() {
		defer close(done)
		cleared, err = h.c.sweepOldLayout(ctx, h.c.ownedIdentifiers())
	}()

	site := make([]*mqtt.Message, 0, len(own)+len(survive)+len(newLayout)+3)
	for _, topic := range append(append(append([]string(nil), own...), survive...), newLayout...) {
		site = append(site, retained(topic))
	}
	// A live message and an already-cleared one are not leftovers.
	site = append(site,
		&mqtt.Message{Topic: "unifi/default/device/00005e005301/firmware", Payload: []byte("x")},
		&mqtt.Message{Topic: "unifi/default/device/00005e005301/uptime", Retain: true},
	)
	replay(t, sub, "unifi/default/#", site)
	bridge := make([]*mqtt.Message, 0, len(ownBridge)+len(foreignBridge))
	for _, topic := range append(append([]string(nil), ownBridge...), foreignBridge...) {
		bridge = append(bridge, retained(topic))
	}
	replay(t, sub, "unifi/bridge/+", bridge)
	<-done
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	clearedTopics := map[string]bool{}
	h.broker.mu.Lock()
	for _, m := range h.broker.msgs {
		if m.payload == "" && m.retain {
			clearedTopics[m.topic] = true
		}
	}
	h.broker.mu.Unlock()

	for _, topic := range slices.Concat(own, ownBridge) {
		if !clearedTopics[topic] {
			t.Errorf("%s is a 1.x leftover of this instance and was not cleared", topic)
		}
	}
	for _, topic := range slices.Concat(survive, foreignBridge, newLayout, []string{
		"unifi/default/device/00005e005301/firmware", "unifi/default/device/00005e005301/uptime",
	}) {
		if clearedTopics[topic] {
			t.Errorf("%s was cleared; it is not this instance's 1.x layout", topic)
		}
	}
	if want := len(own) + len(ownBridge); cleared != want {
		t.Errorf("cleared %d, want %d", cleared, want)
	}

	// Only this instance's own trees were read, and both windows were
	// torn down again.
	sub.mu.Lock()
	filters, unsubscribed := slices.Clone(sub.filters), slices.Clone(sub.unsubscribed)
	sub.mu.Unlock()
	want := []string{"unifi/default/#", "unifi/bridge/+"}
	if !slices.Equal(filters, want) {
		t.Errorf("subscribed %v, want %v", filters, want)
	}
	if !slices.Equal(unsubscribed, want) {
		t.Errorf("unsubscribed %v, want %v", unsubscribed, want)
	}
}

// The predicate on its own, including the rule the sweep is sound by:
// a second level spelling a function name is the new layout, whatever
// follows it.
func TestOldLayoutTopic(t *testing.T) {
	t.Parallel()

	ids := ownedIDs{
		devices: map[string]bool{"00005e005302": true},
		clients: map[string]bool{"00005e005310": true},
		wlans:   map[string]bool{"wlan-1": true},
	}
	for topic, want := range map[string]bool{
		"unifi/bridge/status":                                   true,
		"unifi/bridge/info":                                     true,
		"unifi/default/device/00005e005302/state":               true,
		"unifi/default/device/00005e005302/radio/6g/tx_retries": true,
		"unifi/default/client/00005e005310/cmd/authorize":       true,
		"unifi/default/wlan/wlan-1/enabled/set":                 true,
		"unifi/default/health/wan/latency_ms":                   true,
		// Another name, another site, a function on the second level.
		"unifi2/bridge/status":                           false,
		"unifi2/default/device/00005e005302/state":       false,
		"unifi/garage/device/00005e005302/state":         false,
		"unifi/status/default/device/00005e005302/state": false,
		"unifi/set/default/wlan/wlan-1/enabled":          false,
		"unifi/info":                                     false,
		"unifi/connected":                                false,
		// Too short, or an empty level.
		"unifi/default":                                 false,
		"unifi/default/device":                          false,
		"unifi/default/device/00005e005302":             false,
		"unifi/default/device/00005e005302/":            false,
		"unifi/default/device/00005e005302/port//state": false,
	} {
		if got := oldLayoutTopic("unifi", "default", ids, topic); got != want {
			t.Errorf("oldLayoutTopic(%q) = %v, want %v", topic, got, want)
		}
	}

	// A site spelled like a function would make every topic below it
	// look new; CheckTopics refuses it at start, and the predicate does
	// not take it either.
	if oldLayoutTopic("unifi", "status", ids, "unifi/status/device/00005e005302/state") {
		t.Error("a site spelled like a function was swept")
	}
}

// Without an inbound client there is nothing to read, and the sweep
// says nothing rather than failing the daemon.
func TestMigrationSweepWithoutASubscriberIsANoOp(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.migrateOldLayout(t.Context()); err != nil {
		t.Errorf("migrateOldLayout = %v, want nil", err)
	}
}
