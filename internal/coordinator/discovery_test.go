// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
)

// fakeSubscriber captures the handler so tests can deliver a birth
// message without a broker.
type fakeSubscriber struct {
	mu       sync.Mutex
	filters  []string
	handlers []mqtt.MessageHandler
	err      error
}

func (s *fakeSubscriber) Subscribe(_ context.Context, filter string, _ mqtt.QoS, h mqtt.MessageHandler, _ ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return mqtt.SubscribeResult{}, s.err
	}
	s.filters = append(s.filters, filter)
	s.handlers = append(s.handlers, h)
	return mqtt.SubscribeResult{}, nil
}

func (s *fakeSubscriber) deliver(payload string) {
	s.mu.Lock()
	handlers := append([]mqtt.MessageHandler(nil), s.handlers...)
	s.mu.Unlock()
	for _, h := range handlers {
		h(&mqtt.Message{Topic: "homeassistant/status", Payload: []byte(payload)})
	}
}

// hassConfig enables discovery with a short birth grace period.
func hassConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(`
HOST: 192.0.2.1
API_KEY: k
MQTT_SERVER: broker
MQTT_TOPIC: unifi
HASS_ENABLE: true
HASS_BIRTH_GRACETIME: 1
`), config.MapEnv{})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func TestDiscoveryPublishedFromStaticLoop(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}

	configs := h.broker.topicsWithPrefix("homeassistant/")
	if len(configs) == 0 {
		t.Fatal("no discovery configs were published")
	}

	// The switch's port entities only exist because the static loop
	// carries the details; publishing discovery from the fast loop would
	// announce a device with no ports.
	want := []string{
		"homeassistant/sensor/unifi_00005e005302/state/config",
		"homeassistant/binary_sensor/unifi_00005e005302/port_1_poe/config",
		"homeassistant/sensor/unifi_00005e005303/radio_5g_channel/config",
	}
	for _, topic := range want {
		if !configs[topic] {
			t.Errorf("missing discovery config %s", topic)
		}
	}

	// Configs create entities, so a lost one leaves a device silently
	// missing — they go out retained and at QoS 1, unlike state values.
	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()
	for _, m := range h.broker.msgs {
		if !strings.HasPrefix(m.topic, "homeassistant/") {
			continue
		}
		if m.qos != mqtt.QoS1 || !m.retain {
			t.Errorf("%s published at qos %v retain %v, want qos 1 retained", m.topic, m.qos, m.retain)
		}
	}
}

func TestDiscoveryDisabled(t *testing.T) {
	t.Parallel()

	// The default config has HASS_ENABLE off.
	h := newHarness(t, nil)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if got := h.broker.topicsWithPrefix("homeassistant/"); len(got) != 0 {
		t.Errorf("published %d discovery configs with HASS_ENABLE off", len(got))
	}
}

// A decommissioned device must have its entities removed, or Home
// Assistant keeps a set of permanently unavailable ones forever.
func TestRemovedDeviceClearsItsEntities(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	// Across every platform, not just sensor: the AP also has
	// binary_sensor entities and all of them must be cleared.
	apConfigs := map[string]bool{}
	for topic := range h.broker.topicsWithPrefix("homeassistant/") {
		if strings.Contains(topic, "unifi_00005e005303") {
			apConfigs[topic] = true
		}
	}
	if len(apConfigs) == 0 {
		t.Fatal("the AP announced no entities to begin with")
	}

	// The AP is removed from the site.
	h.src.mu.Lock()
	h.src.devices = h.src.devices[:2]
	h.src.mu.Unlock()

	h.broker.reset()
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	// An empty retained payload is how MQTT deletes a message and how
	// Home Assistant is told the entity is gone.
	var cleared int
	h.broker.mu.Lock()
	for _, m := range h.broker.msgs {
		if strings.HasPrefix(m.topic, "homeassistant/") && strings.Contains(m.topic, "unifi_00005e005303") {
			if m.payload != "" {
				t.Errorf("%s was republished with a payload instead of cleared", m.topic)
			}
			if !m.retain {
				t.Errorf("%s was cleared without the retain flag, so the retained config survives", m.topic)
			}
			cleared++
		}
	}
	h.broker.mu.Unlock()

	if cleared != len(apConfigs) {
		t.Errorf("cleared %d of the AP's %d entities", cleared, len(apConfigs))
	}
}

// A port that disappears — a stacked switch losing a member, say —
// must lose its entities without touching the rest of the device.
func TestRemovedPortClearsOnlyItsEntities(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}

	// Port 25 goes away.
	h.src.mu.Lock()
	details := sampleDetails()
	details[1].Ports = details[1].Ports[:1]
	h.src.details = details
	h.src.mu.Unlock()

	h.broker.reset()
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}

	var clearedPort25, touchedPort1 bool
	h.broker.mu.Lock()
	for _, m := range h.broker.msgs {
		if !strings.HasPrefix(m.topic, "homeassistant/") {
			continue
		}
		if strings.Contains(m.topic, "port_25_") && m.payload == "" {
			clearedPort25 = true
		}
		if strings.Contains(m.topic, "port_1_") && m.payload == "" {
			touchedPort1 = true
		}
	}
	h.broker.mu.Unlock()

	if !clearedPort25 {
		t.Error("the removed port's entities were not cleared")
	}
	if touchedPort1 {
		t.Error("a surviving port's entities were cleared")
	}
}

// Home Assistant forgets MQTT-discovered entities on restart and relies
// on integrations re-announcing them. Without this every HA restart
// leaves the UniFi devices missing until the daemon restarts too.
func TestHomeAssistantBirthTriggersRepublish(t *testing.T) {
	t.Parallel()

	sub := &fakeSubscriber{}
	h := newHarness(t, hassConfig(t))
	h.c.SetSubscriber(sub)

	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.watchHomeAssistant(t.Context(), sub); err != nil {
		t.Fatalf("watchHomeAssistant: %v", err)
	}
	if got, want := sub.filters[0], "homeassistant/status"; got != want {
		t.Errorf("subscribed to %q, want %q", got, want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = h.c.rediscoverLoop(ctx)
		close(done)
	}()

	h.broker.reset()
	sub.deliver("online")

	// The grace period is 1s in this config; poll until the republish
	// lands rather than sleeping a fixed amount.
	deadline := time.After(5 * time.Second)
	for len(h.broker.topicsWithPrefix("homeassistant/")) == 0 {
		select {
		case <-deadline:
			t.Fatal("discovery was not republished after the birth message")
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// Anything other than "online" is not a birth message — an "offline"
// payload republishing discovery would be pointless traffic.
func TestNonOnlinePayloadIsIgnored(t *testing.T) {
	t.Parallel()

	sub := &fakeSubscriber{}
	h := newHarness(t, hassConfig(t))
	if err := h.c.watchHomeAssistant(t.Context(), sub); err != nil {
		t.Fatalf("watchHomeAssistant: %v", err)
	}

	sub.deliver("offline")
	select {
	case <-h.c.rediscover:
		t.Error("an offline payload queued a re-announce")
	default:
	}

	sub.deliver("online")
	select {
	case <-h.c.rediscover:
	default:
		t.Error("an online payload did not queue a re-announce")
	}
}

// The birth handler runs inline in the MQTT read loop, where blocking
// stalls acknowledgement processing and trips the keep-alive watchdog.
// Repeated births must therefore never block.
func TestBirthHandlerNeverBlocks(t *testing.T) {
	t.Parallel()

	sub := &fakeSubscriber{}
	h := newHarness(t, hassConfig(t))
	if err := h.c.watchHomeAssistant(t.Context(), sub); err != nil {
		t.Fatalf("watchHomeAssistant: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for range 100 {
			sub.deliver("online")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the birth handler blocked — nobody was draining the channel")
	}
}

func TestDiscoveryPayloadIsValidJSON(t *testing.T) {
	t.Parallel()

	h := newHarness(t, hassConfig(t))
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}

	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()
	var checked int
	for _, m := range h.broker.msgs {
		if !strings.HasPrefix(m.topic, "homeassistant/") || m.payload == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(m.payload), &payload); err != nil {
			t.Errorf("%s: %v", m.topic, err)
			continue
		}
		for _, required := range []string{"unique_id", "object_id", "state_topic", "device"} {
			if _, ok := payload[required]; !ok {
				t.Errorf("%s is missing %s", m.topic, required)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no discovery payloads to check")
	}
}
