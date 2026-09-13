// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	hapub "github.com/SukramJ/go-hamqtt/publisher"
	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// The go-hamqtt plane assertions — ADR 0070 phase 9, step 5.
//
// Every test here covers something no golden can: the pinned surface
// records topics, payloads, QoS and retain flags, and most of what this
// step changed moves none of those. A QoS field left unset on the
// command plane, a runtime that outlives the connection it remembers, a
// state publish that bypasses the dedup gate and an availability marker
// refused by a tripped circuit breaker are all invisible to a byte
// comparison — which is why this file exists at all.

// --- QoS ---------------------------------------------------------------

// Every QoS is stated, and none of the three resolves to the library's
// default by accident.
//
// This is the cheap half of the proof. It catches the one mutation that
// moves no published byte — CommandQoS, which governs a SUBSCRIBE — and
// it is not sufficient on its own: a constant nothing reads passes it.
// TestEveryPlanePublishReachesTheWireAtTheStatedQoS is the other half.
func TestQoSIsStatedNotDefaulted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		qos  hapub.QoS
		wire byte
	}{
		{"state", StateQoS, 0},
		{"availability", AvailabilityQoS, 1},
		{"command", CommandQoS, 1},
	} {
		if tc.qos == hapub.QoSUnset {
			t.Errorf("%s QoS is QoSUnset, which the library resolves to QoS 1", tc.name)
		}
		got, ok := tc.qos.Wire()
		if !ok {
			t.Errorf("%s QoS does not resolve to a wire value", tc.name)
			continue
		}
		if got != tc.wire {
			t.Errorf("%s QoS reaches the wire as %d, want %d", tc.name, got, tc.wire)
		}
	}
}

// The QoS a publish actually carries, read off the transport call.
//
// A test that asserts a constant passes even when the constant is never
// reached; a test that reads the recorded publish cannot. This drives
// the two planes that publish — the availability marker through
// [hapub.Runtime] and a state value through [hapub.StatePublisher] —
// and one retained clear, and requires the exact split measurement §2.6
// recorded: availability at 1, state at 0, everything retained.
func TestEveryPlanePublishReachesTheWireAtTheStatedQoS(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{}
	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: newFakeSource(),
		MQTT:   broker,
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	t.Cleanup(c.Close)
	ctx := t.Context()

	if err := c.ha().AnnounceOnline(ctx); err != nil {
		t.Fatalf("AnnounceOnline: %v", err)
	}
	if err := c.pub.publish(ctx, "unifi/default/device/aa/state", "ONLINE"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := c.pub.publish(ctx, "unifi/default/client/x/ip", ""); err != nil {
		t.Fatalf("publish of an empty payload: %v", err)
	}

	broker.mu.Lock()
	got := slices.Clone(broker.msgs)
	broker.mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("recorded %d publishes, want 3: %+v", len(got), got)
	}
	for _, m := range got {
		if !m.retain {
			t.Errorf("%s published unretained; every topic this daemon owns is retained", m.topic)
		}
	}
	if got[0].topic != "unifi/bridge/status" || got[0].payload != "online" || got[0].qos != mqtt.QoS1 {
		t.Errorf("availability publish = %+v, want unifi/bridge/status online at QoS 1", got[0])
	}
	if got[1].qos != mqtt.QoS0 || got[1].payload != "ONLINE" {
		t.Errorf("state publish = %+v, want QoS 0", got[1])
	}
	if got[2].qos != mqtt.QoS0 || got[2].payload != "" {
		t.Errorf("retained clear = %+v, want no bytes at QoS 0", got[2])
	}
}

// [hapub.StateConfig.Encoding] is inert here and is stated anyway.
//
// This daemon renders its own payloads and calls Publish, never
// PublishValue, so the encoding decides nothing — but the zero value is
// EnvelopeEncoding, which is a statement that reads as the opposite of
// what this bridge publishes. Asserting the inertness is what keeps the
// field from looking like an oversight, and what makes the day it stops
// being inert visible.
func TestStateEncodingIsInertAndStatedAnyway(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{}
	p := newPublisher(broker, nil, 0, newFakeClock().now, slog.New(slog.DiscardHandler))
	if err := p.publish(t.Context(), "unifi/default/device/aa/state", "ONLINE"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, _ := broker.latest("unifi/default/device/aa/state")
	if got != "ONLINE" {
		t.Fatalf("payload = %q, want the bare scalar; an envelope encoding would have reached the wire", got)
	}
}

// --- the empty payload -------------------------------------------------

// An empty state payload is a retraction, and it still deduplicates.
//
// [hapub.StatePublisher.Publish] refuses zero bytes with
// ErrEmptyStatePayload on purpose, so the eviction path has to be
// explicit — and Evict has no dedup gate of its own, by design. That
// would be a behaviour change here rather than a detail: an absent
// client publishes an empty `ip` and an empty `signal` on every poll
// for as long as it stays away, and change detection suppressed the
// repeats before this step.
func TestEmptyStatePayloadIsEvictedAndStillDeduplicated(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{}
	clock := newFakeClock()
	p := newPublisher(broker, nil, time.Hour, clock.now, slog.New(slog.DiscardHandler))
	const topic = "unifi/default/client/x/ip"
	ctx := t.Context()

	for range 3 {
		if err := p.publish(ctx, topic, ""); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if n := broker.count(topic); n != 1 {
		t.Errorf("an unchanged empty payload was published %d times, want 1", n)
	}
	if got, _ := broker.latest(topic); got != "" {
		t.Errorf("payload = %q, want no bytes", got)
	}

	// A real value after an eviction must go out: the gate has to have
	// forgotten the topic, not merely recorded an empty payload against
	// it.
	if err := p.publish(ctx, topic, "192.0.2.5"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, _ := broker.latest(topic); got != "192.0.2.5" {
		t.Errorf("payload after the eviction = %q, want the new value", got)
	}

	// And the forced republish still reaches the eviction path.
	clock.advance(2 * time.Hour)
	if err := p.publish(ctx, topic, ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := p.publish(ctx, topic, ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if n := broker.count(topic); n != 3 {
		t.Errorf("topic published %d times in total, want 3 (evict, value, forced evict)", n)
	}
}

// --- the runtime is a factory ------------------------------------------

// A (re)connect rebuilds the whole discovery runtime rather than
// resetting a field of it.
//
// Asserting the object identity rather than a field is deliberate: a
// memo the library adds later is covered the day it is added, and this
// is the property step 6 depends on. See [Coordinator.newRuntime].
func TestOnConnectRebuildsTheDiscoveryRuntime(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{}
	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: newFakeSource(),
		MQTT:   broker,
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	t.Cleanup(c.Close)

	first := c.ha()
	c.OnConnect(t.Context())
	second := c.ha()
	c.OnConnect(t.Context())
	third := c.ha()

	if first == second || second == third {
		t.Error("the discovery runtime survived a connect; its memo is per connection, not per process")
	}
	if second == nil || third == nil {
		t.Fatal("the runtime factory produced nil")
	}
}

// What the rebuild is worth, with the defect driven beside the fix.
//
// [hapub.Runtime] remembers what it has already put on the broker so a
// steady-state boot does not re-send it forever. That memo is correct
// per *connection* and wrong per process: a QoS 0 publish "succeeds"
// when the bytes reach a socket, and if that socket then died the
// broker applied none of them. At step 6 the memo in question is the
// retraction of every per-entity config, and a retry that re-sends zero
// of them publishes the device bundle into a tree that still holds them
// — which Home Assistant refuses with one `WARNING [mqtt.entity]
// Received a conflicting MQTT discovery message` and no entities.
//
// The second row asserts the DEFECT. It is what turns red if the
// runtime ever stops being rebuilt.
func TestTheDiscoveryRuntimeMemoDoesNotSurviveAConnection(t *testing.T) {
	t.Parallel()

	const topic = "homeassistant/sensor/unifi_00005e005301/state/config"
	body := []byte(`{"unique_id":"unifi_00005e005301_state"}`)

	for _, tc := range []struct {
		name      string
		reconnect bool
		wantSends int
	}{
		{"a reconnect rebuilds the runtime", true, 2},
		{"the defect: the memo outlives the connection it was written on", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			broker := &fakeBroker{}
			c := New(Deps{
				Cfg:    hassConfig(t),
				Site:   testSite(),
				Source: newFakeSource(),
				MQTT:   broker,
				Logger: slog.New(slog.DiscardHandler),
				Now:    newFakeClock().now,
			})
			t.Cleanup(c.Close)
			ctx := t.Context()

			if _, err := c.ha().Publish(ctx, topic, body); err != nil {
				t.Fatalf("first publish: %v", err)
			}
			if tc.reconnect {
				c.resetPlanes()
			}
			if _, err := c.ha().Publish(ctx, topic, body); err != nil {
				t.Fatalf("second publish: %v", err)
			}
			if n := broker.count(topic); n != tc.wantSends {
				t.Errorf("the config reached the broker %d times, want %d", n, tc.wantSends)
			}
		})
	}
}

// The state plane's dedup gate re-opens on a reconnect, and the claim
// list does not.
//
// Both halves matter and they pull in opposite directions. A broker
// that came back without its retained store holds nothing, so a gate
// that still believes it published every value leaves that broker
// permanently empty. The sweep's ownership evidence is the opposite: it
// is a statement about this *process*, and clearing it would make a
// reconnected daemon believe it had published nothing — the one input
// that makes the sweep dangerous.
func TestAReconnectOpensTheDedupGateAndKeepsTheClaims(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{}
	c := New(Deps{
		Cfg:    hassConfig(t),
		Site:   testSite(),
		Source: newFakeSource(),
		MQTT:   broker,
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	t.Cleanup(c.Close)
	ctx := t.Context()

	const state = "unifi/default/device/aa/state"
	const config = "homeassistant/sensor/unifi_aa/state/config"
	mustPublish := func() {
		t.Helper()
		if err := c.pub.publish(ctx, state, "ONLINE"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	mustPublish()
	mustPublish()
	if n := broker.count(state); n != 1 {
		t.Fatalf("an unchanged value was published %d times before the reconnect, want 1", n)
	}
	if err := c.pub.publishConfig(ctx, config, []byte(`{"unique_id":"unifi_aa_state"}`)); err != nil {
		t.Fatalf("publishConfig: %v", err)
	}

	c.OnConnect(ctx)

	mustPublish()
	if n := broker.count(state); n != 2 {
		t.Errorf("the value was published %d times after the reconnect, want 2: the dedup gate did not re-open", n)
	}
	if !c.pub.claims().Published[config] {
		t.Error("the reconnect dropped the sweep's ownership claim; a reconnected daemon must not forget what it published")
	}
}

// The reconnect re-drives every class of discovery, not only the ones
// whose loop happens to come round again.
//
// An open dedup gate publishes exactly as little as a closed one if
// nothing walks through it. This daemon announces client configs on a
// client's first sighting and the site-health configs on the classic
// layer's first answer, neither of which happens twice while the daemon
// runs — so a broker that came back without its retained store would
// keep those entities missing until the daemon itself was restarted.
func TestAReconnectRepublishesEveryClassOfConfig(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, controlConfig(t, "  SIGNAL_SENSOR: true\n"), allCaps{})
	t.Cleanup(h.c.Close)
	withSampleClients(h)
	ctx := t.Context()

	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshClients(ctx); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	if err := h.c.refreshHealth(ctx); err != nil {
		t.Fatalf("refreshHealth: %v", err)
	}
	first := h.broker.topicsWithPrefix("homeassistant/")
	// Every class this daemon announces has to be in the fixture, or
	// the comparison below proves nothing about the ones that are not:
	// the two that are announced exactly once per process are the whole
	// finding.
	for _, want := range []string{"device_tracker/unifi_client_", "sensor/unifi_site_", "switch/unifi_site_default/wlan_", "sensor/unifi_00005e005301"} {
		if !slices.ContainsFunc(mapKeys(first), func(s string) bool { return strings.Contains(s, want) }) {
			t.Fatalf("the fixture announced no %s config; this test would prove nothing", want)
		}
	}

	// The broker comes back with an empty retained store.
	h.broker.reset()
	h.c.OnConnect(ctx)
	if err := h.c.refreshStatic(ctx); err != nil {
		t.Fatalf("refreshStatic after the reconnect: %v", err)
	}
	if err := h.c.refreshClients(ctx); err != nil {
		t.Fatalf("refreshClients after the reconnect: %v", err)
	}
	if err := h.c.refreshHealth(ctx); err != nil {
		t.Fatalf("refreshHealth after the reconnect: %v", err)
	}

	second := h.broker.topicsWithPrefix("homeassistant/")
	var missing []string
	for topic := range first {
		if !second[topic] {
			missing = append(missing, topic)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("%d configs were not re-announced after the reconnect: %v", len(missing), missing)
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- the command plane -------------------------------------------------

// The six permanent command filters are pairwise disjoint, proved by
// registering them on a router that refuses an ambiguous pair.
//
// A broker sends one PUBLISH copy per matching subscription, and a
// client that re-matches each copy against its whole filter list then
// runs every matching handler per copy — so two overlapping filters
// power-cycle a port twice per press, with nothing in any log.
// openccu-loom measured exactly that against Mosquitto 2.1.2.
func TestSubscriptionFiltersCannotOverlap(t *testing.T) {
	t.Parallel()

	nop := func(context.Context, hapub.Command) {}
	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: newFakeSource(),
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	t.Cleanup(c.Close)

	filters := c.commandFilters()
	if len(filters) != 6 {
		t.Fatalf("the daemon registers %d command filters, want 6", len(filters))
	}
	r := hapub.NewCommandRouter(nopTransport{}, hapub.CommandConfig{QoS: CommandQoS})
	for _, f := range filters {
		if err := r.Handle(f, nop); err != nil {
			t.Fatalf("the permanent command filters overlap: %v", err)
		}
	}

	// Not vacuous: the router really does refuse an overlap.
	if err := r.Handle("unifi/default/device/+/cmd/+", nop); err == nil {
		t.Error("the router accepted a filter overlapping a command route; this test proves nothing")
	} else if !errors.Is(err, hapub.ErrAmbiguousRoutes) {
		t.Errorf("the overlap was refused with %v, want ErrAmbiguousRoutes", err)
	}

	// The sweep window and the Home Assistant birth subscription live in
	// another tree entirely, so no command handler can be reached twice
	// however wide the sweep's `<prefix>/#` is.
	for _, f := range []string{sweepWindow(c), c.cfg.HASSBaseTopic + "/status"} {
		for _, cmd := range filters {
			if strings.HasPrefix(f, c.topics.root+"/") {
				t.Errorf("%s is inside this daemon's own tree alongside %s", f, cmd)
			}
		}
	}
}

// Nothing this daemon publishes lands inside its own command
// subscription.
//
// A broker delivers a client's own publishes back to it, so a state
// topic matching a command filter is a write the process performs on
// itself — and the least diagnosable shape of that is a device that
// appears to change its own settings. It is a property of the topic
// layout and the catalogue, so it cannot be transient: the check fails
// the boot rather than warning.
func TestNothingThisDaemonPublishesIsAlsoSubscribed(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, controlConfig(t, "  SIGNAL_SENSOR: true\n"), allCaps{})
	t.Cleanup(h.c.Close)
	withSampleClients(h)
	ctx := t.Context()
	for _, fn := range []func(context.Context) error{
		h.c.refreshStatic, h.c.refreshDevices, h.c.refreshDeviceStats,
		h.c.refreshClients, h.c.refreshHealth,
	} {
		if err := fn(ctx); err != nil {
			t.Fatalf("warming the fixture: %v", err)
		}
	}

	states := h.c.knownStateTopics()
	if len(states) < 50 {
		t.Fatalf("only %d state topics were published; the probe set is too small to prove anything", len(states))
	}

	r := hapub.NewCommandRouter(nopTransport{}, hapub.CommandConfig{QoS: CommandQoS})
	for _, f := range h.c.commandFilters() {
		if err := r.Handle(f, func(context.Context, hapub.Command) {}); err != nil {
			t.Fatalf("Handle(%s): %v", f, err)
		}
	}
	if err := r.CheckDisjoint(states...); err != nil {
		t.Errorf("this daemon publishes into its own command tree: %v", err)
	}

	// Not vacuous: a topic that genuinely collides is caught.
	collide := h.c.topics.device(surfGwMAC, cmdRestart)
	if err := r.CheckDisjoint(collide); err == nil {
		t.Errorf("CheckDisjoint accepted %s, which is a command topic; this test proves nothing", collide)
	}
	// And the state plane refuses the same topic one message at a time.
	if _, err := h.c.pub.state.Publish(ctx, collide, []byte("x")); !errors.Is(err, hapub.ErrStateCommandCollision) {
		t.Errorf("the state plane published into its own command tree: %v", err)
	}
}

// nopTransport is a [hapub.Transport] for the constructor-level
// assertions, which register routes and never start them.
type nopTransport struct{}

func (nopTransport) Publish(context.Context, string, []byte, byte, bool) error { return nil }
func (nopTransport) Subscribe(context.Context, string, byte, hapub.Handler) error {
	return errors.New("nopTransport: no broker")
}
func (nopTransport) Unsubscribe(context.Context, string) error { return nil }

// --- birth, death and the will -----------------------------------------

// The will names the topic every entity reads, and it is the runtime's
// own statement rather than a second spelling.
func TestTheWillWritesTheTopicEveryEntityReads(t *testing.T) {
	t.Parallel()

	c := New(Deps{
		Cfg:    hassConfig(t),
		Site:   testSite(),
		Source: newFakeSource(),
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	t.Cleanup(c.Close)

	will, err := c.Will()
	if err != nil {
		t.Fatalf("Will: %v", err)
	}
	if will.Topic != c.AvailabilityTopic() {
		t.Errorf("will topic = %q, want the availability topic %q", will.Topic, c.AvailabilityTopic())
	}
	if strings.HasPrefix(will.Topic, c.cfg.HASSBaseTopic+"/") {
		t.Errorf("will topic %q sits inside Home Assistant's own discovery tree", will.Topic)
	}
	if string(will.Payload) != payloadOffline {
		t.Errorf("will payload = %q, want %q", will.Payload, payloadOffline)
	}
	if !will.Retain {
		t.Error("the will is not retained; a Home Assistant that subscribes after the crash would never see it")
	}
	if wire, _ := AvailabilityQoS.Wire(); will.QoS != wire {
		t.Errorf("will QoS = %d, want the availability plane's %d", will.QoS, wire)
	}

	// The same string reaches every published entity.
	entries, err := c.hass.Device(&sampleDevices()[0])
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	for _, e := range entries {
		if !strings.Contains(string(e.Payload), `"topic":"`+will.Topic+`"`) {
			t.Fatalf("%s does not reference the will topic %q", e.ConfigTopic, will.Topic)
		}
	}
}

// The availability marker goes around the circuit breaker, and
// everything else goes through it.
//
// [mqtt.Breaker] counts ErrNotConnected as a failure, so a connection
// drop is exactly what opens the circuit — and the first thing a
// reconnected daemon does is announce itself online. Driven against a
// deliberately tripped breaker, which is the state a reconnect actually
// finds.
func TestTheAvailabilityMarkerDoesNotGoThroughTheBreaker(t *testing.T) {
	t.Parallel()

	direct := &fakeBroker{}
	gated := &fakeBroker{fail: mqtt.ErrCircuitOpen}
	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: newFakeSource(),
		MQTT:   gated,
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	t.Cleanup(c.Close)
	c.SetDirectPublisher(direct)
	ctx := t.Context()

	if err := c.ha().AnnounceOnline(ctx); err != nil {
		t.Errorf("the birth was refused by the open circuit: %v", err)
	}
	if got, _ := direct.latest(c.AvailabilityTopic()); got != payloadOnline {
		t.Errorf("direct publisher saw %q, want the birth marker", got)
	}
	if err := c.ha().AnnounceOffline(ctx); err != nil {
		t.Errorf("the death marker was refused by the open circuit: %v", err)
	}

	// Not vacuous: every other topic really does ride the breaker.
	if err := c.pub.publish(ctx, "unifi/default/device/aa/state", "ONLINE"); !errors.Is(err, mqtt.ErrCircuitOpen) {
		t.Errorf("a state publish bypassed the breaker: %v", err)
	}
	if direct.total() != 2 {
		t.Errorf("the direct publisher carried %d messages, want only the two availability markers", direct.total())
	}
}

// --- the sweep ---------------------------------------------------------

// The sweep's snapshot window is the whole discovery prefix, and it is
// torn down again.
//
// Derived by driving a real pass rather than by asking an accessor, so
// the helper the other reconcile tests use cannot drift from what is
// actually subscribed.
func TestTheSweepWindowIsTheWholeDiscoveryPrefix(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	t.Cleanup(h.c.Close)
	h.c.reconcileWindow = 20 * time.Millisecond

	if _, err := h.c.collectRetainedConfigs(t.Context()); err != nil {
		t.Fatalf("collectRetainedConfigs: %v", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	want := h.c.cfg.HASSBaseTopic + "/#"
	if !slices.Contains(sub.filters, want) {
		t.Errorf("subscribed = %v, want %s among them", sub.filters, want)
	}
	if !slices.Contains(sub.unsubscribed, want) {
		t.Errorf("unsubscribed = %v, want %s among them", sub.unsubscribed, want)
	}
}

// The snapshot window collects only this daemon's own topic shape, and
// nothing else the discovery tree carries.
//
// This is the half the retraction test cannot see. The claim list is
// what decides a retraction, so widening [hass.OwnsConfigTopic] to
// everything retracts nothing extra — and therefore moves no assertion
// about what was cleared. What it does move is what this daemon reads,
// remembers and names in its `reconcile_unclaimed` log line, and what
// [hass.ConfigTopicFor] is asked to rebuild: for a form that carries no
// node id it would rebuild a topic that belongs to nobody.
func TestTheSweepWindowCollectsOnlyThisDaemonsOwnShape(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	t.Cleanup(h.c.Close)
	h.c.reconcileWindow = 200 * time.Millisecond

	tree := map[string][]byte{
		"homeassistant/sensor/unifi_00005e005301/state/config":  ownConfig(h.c, "unifi_00005e005301_state"),
		"homeassistant/switch/unifi_site_default/wlan_a/config": ownConfig(h.c, "unifi_wlan_a_enabled"),
		"homeassistant/sensor/zigbee2mqtt_bridge/state/config":  []byte(`{"unique_id":"z"}`),
		"homeassistant/binary_sensor/tasmota_ABC/status/config": []byte(`{"unique_id":"t"}`),
		"homeassistant/device/unifi_00005e005301/config":        ownConfig(h.c, "unifi_00005e005301_state"),
		"homeassistant/sensor/unifi_00005e005301_state/config":  ownConfig(h.c, "unifi_00005e005301_state"),
		"homeassistant/climate/unifi_00005e005301/therm/config": ownConfig(h.c, "unifi_00005e005301_therm"),
		"homeassistant/status":                                  []byte("online"),
	}
	want := []string{
		"homeassistant/sensor/unifi_00005e005301/state/config",
		"homeassistant/switch/unifi_site_default/wlan_a/config",
	}

	type result struct {
		got map[string][]byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := h.c.collectRetainedConfigs(t.Context())
		done <- result{got, err}
	}()
	sub.deliverRetained(t, sweepWindow(h.c), tree)

	res := <-done
	if res.err != nil {
		t.Fatalf("collectRetainedConfigs: %v", res.err)
	}
	got := mapKeys(toSet(res.got))
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the window collected %v, want %v", got, want)
	}
}

func toSet(m map[string][]byte) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// What a report-only pass offers, claims and would retract, over a
// hostile retained tree — with a distinct reason for every survivor.
//
// The fan-out is the point: a pass that spared everything for one
// reason has only been shown to work in one direction.
func TestReportOnlySweepOverTheRealFleet(t *testing.T) {
	t.Parallel()

	h, sub := newReconcileHarness(t, hassConfig(t))
	t.Cleanup(h.c.Close)
	ctx := t.Context()

	const (
		live     = "homeassistant/sensor/unifi_00005e005301/state/config"
		orphan   = "homeassistant/sensor/unifi_00005e005301/retired/config"
		sibling  = "homeassistant/switch/unifi_site_default/wlan_other-console/config"
		foreign  = "homeassistant/sensor/zigbee2mqtt_bridge/state/config"
		tasmota  = "homeassistant/binary_sensor/tasmota_ABC/status/config"
		bundle   = "homeassistant/device/unifi_00005e005301/config"
		noNodeID = "homeassistant/sensor/unifi_00005e005301_state/config"
		unseen   = "homeassistant/sensor/unifi_00005e005399/state/config"
		platform = "homeassistant/climate/unifi_00005e005301/thermostat/config"
		emptied  = "homeassistant/sensor/unifi_00005e005301/gone/config"
	)

	// The claim list: this process published the live entity and the
	// orphan, and announces only the first.
	for _, topic := range []string{live, orphan} {
		if err := h.c.pub.publishConfig(ctx, topic, ownConfig(h.c, "unifi_00005e005301_state")); err != nil {
			t.Fatalf("publishConfig: %v", err)
		}
	}
	if err := h.c.clearConfig(ctx, orphan); err != nil {
		t.Fatalf("clearConfig: %v", err)
	}
	h.c.readyDevices.Store(true)
	h.c.readyStatic.Store(true)
	h.c.readyClients.Store(true)
	h.c.healthAnnounced.Store(true)
	h.broker.reset()

	tree := map[string][]byte{
		live:     ownConfig(h.c, "unifi_00005e005301_state"),
		orphan:   ownConfig(h.c, "unifi_00005e005301_retired"),
		sibling:  ownConfig(h.c, "unifi_wlan_other-console_enabled"),
		foreign:  []byte(`{"unique_id":"zigbee2mqtt_bridge_state"}`),
		tasmota:  []byte(`{"unique_id":"tasmota_ABC_status"}`),
		bundle:   ownConfig(h.c, "unifi_00005e005301_state"),
		noNodeID: ownConfig(h.c, "unifi_00005e005301_state"),
		unseen:   ownConfig(h.c, "unifi_00005e005399_state"),
		platform: ownConfig(h.c, "unifi_00005e005301_thermostat"),
		emptied:  nil,
	}

	done := make(chan error, 1)
	go func() { done <- h.c.reconcileOrphans(ctx) }()
	sub.deliverRetained(t, sweepWindow(h.c), tree)
	if err := <-done; err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}

	cleared := h.broker.topicsWithPrefix("homeassistant/")
	if len(cleared) != 1 || !cleared[orphan] {
		t.Errorf("the pass retracted %v, want exactly [%s]", mapKeys(cleared), orphan)
	}
	// Counted rather than asserted in prose: the window's own arithmetic
	// is what makes the fan-out below readable — six of the ten never
	// reached a payload check at all.
	ownedByTopic := 0
	for topic, body := range tree {
		parsed, ok := hapub.ParseConfigTopic(h.c.cfg.HASSBaseTopic, topic)
		if ok && len(body) > 0 && hass.OwnsConfigTopic(parsed) {
			ownedByTopic++
		}
	}
	if ownedByTopic != 4 {
		t.Errorf("the topic predicate owned %d of %d; the fan-out below describes 4", ownedByTopic, len(tree))
	}
	claims := h.c.pub.claims()
	t.Logf("ReportOnly over the real fleet: %d retained configs offered, %d owned by topic, "+
		"%d claimed by this process (%d still announced), %d retracted: %v",
		len(tree), ownedByTopic, len(claims.Published), len(claims.Announced), len(cleared), mapKeys(cleared))
	for _, spared := range []struct{ topic, why string }{
		{live, "claimed: this process announces it right now"},
		{sibling, "not published by this process — another console's live entity"},
		{foreign, "another integration's namespace"},
		{tasmota, "another integration's namespace"},
		{bundle, "the device-document form, which this daemon does not publish"},
		{noNodeID, "the node-id-less four-segment form"},
		{unseen, "a device of ours this process never published"},
		{platform, "a platform this daemon never emits"},
		{emptied, "already empty; retracting it again would be a message for nothing"},
	} {
		if cleared[spared.topic] {
			t.Errorf("%s was retracted; it must be spared (%s)", spared.topic, spared.why)
		}
		t.Logf("  spared: %-72s %s", spared.topic, spared.why)
	}
}

// --- what this package tells the library -------------------------------

// [hass.OwnsConfigTopic] is as narrow as what this daemon publishes.
func TestOwnsConfigTopicIsNarrow(t *testing.T) {
	t.Parallel()

	own := hapub.ConfigTopic{Platform: "sensor", NodeID: "unifi_00005e005301", ObjectID: "state"}
	if !hass.OwnsConfigTopic(own) {
		t.Fatalf("%+v is this daemon's own shape and was declined", own)
	}
	for name, t2 := range map[string]hapub.ConfigTopic{
		"a device document":     {Bundle: true, NodeID: "unifi_00005e005301"},
		"no platform":           {NodeID: "unifi_00005e005301", ObjectID: "state"},
		"no node id":            {Platform: "sensor", ObjectID: "state"},
		"no object id":          {Platform: "sensor", NodeID: "unifi_00005e005301"},
		"a foreign namespace":   {Platform: "sensor", NodeID: "zigbee2mqtt_x", ObjectID: "state"},
		"an unpublished platfo": {Platform: "climate", NodeID: "unifi_00005e005301", ObjectID: "state"},
	} {
		if hass.OwnsConfigTopic(t2) {
			t.Errorf("%s was claimed: %+v", name, t2)
		}
	}
}

// The legacy topic form is stated, and it is the one measured at 315 of
// 315 rather than the one that reproduces none.
func TestTheLegacyConfigTopicFormIsTheFiveSegmentOne(t *testing.T) {
	t.Parallel()

	forms := hass.LegacyConfigTopicForms()
	if len(forms) != 1 {
		t.Fatalf("the fleet is stated to be on %d forms, want 1", len(forms))
	}
	e := hapub.LegacyEntity{
		Prefix: "homeassistant", Platform: "sensor",
		NodeID: "unifi_00005e005301", ObjectID: "state",
		UniqueID: "unifi_00005e005301_state",
	}
	const want = "homeassistant/sensor/unifi_00005e005301/state/config"
	if got := forms[0](e); got != want {
		t.Errorf("the stated form renders %q, want the five-segment %q", got, want)
	}
	// Not vacuous: the form this bridge is NOT on renders something else.
	if got := hapub.LegacyTopicByUniqueID(e); got == want {
		t.Error("LegacyTopicByUniqueID renders the same topic; this test proves nothing")
	}
}

// withSampleClients gives a harness a client fleet, so a test that
// depends on the client plane announcing anything cannot pass by
// finding nothing there.
func withSampleClients(h *harness) {
	h.src.mu.Lock()
	h.src.clients = []model.Client{
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:10"), ID: "c1", Name: "Phone",
			Type: model.ClientWireless, IP: netip.MustParseAddr("192.0.2.42"),
			UplinkID: "id-ap", SignalDBm: -55,
			ConnectedAt: time.Date(2026, 8, 14, 7, 0, 0, 0, time.UTC),
		},
		{
			MAC: model.MustParseMAC("00:00:5e:00:53:11"), ID: "c2", Name: "NAS",
			Type: model.ClientWired, IP: netip.MustParseAddr("192.0.2.50"),
			UplinkID: "id-sw",
		},
	}
	h.src.mu.Unlock()
}
