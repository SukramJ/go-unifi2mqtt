// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// allCaps reports every classic capability as available.
type allCaps struct{}

func (allCaps) Has(unifi.Capability) bool { return true }

func controlConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(`
HOST: 192.0.2.1
API_KEY: k
MQTT_SERVER: broker
MQTT_TOPIC: unifi
HASS_ENABLE: true
CLASSIC_ENABLE: true
CLASSIC_USERNAME: admin
CLASSIC_PASSWORD: pw
CLIENTS:
  ENABLE: true
  TYPES: []
CONTROLS:
  ENABLE: true
  DEVICE_RESTART: true
  PORT_POWER_CYCLE: true
  DEVICE_LOCATE: true
  CLIENT_BLOCK: true
  GUEST_AUTHORIZE: true
  WLAN_ENABLE: true
`+extra), config.MapEnv{})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

// controlHarness wires a coordinator with controls on and a channel
// that reports executed actuator calls.
func controlHarness(t *testing.T, extra string) *harness {
	t.Helper()

	cfg := controlConfig(t, extra)
	h := newHarnessWith(t, cfg, allCaps{})
	h.src.actuatorCh = make(chan actuatorCall, 8)

	// Prime the caches so MAC→ID resolution works.
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	return h
}

// deliver feeds a message to the command handler as the broker would.
func deliver(c *Coordinator, topic, payload string, retained bool) {
	c.onCommand(&mqtt.Message{Topic: topic, Payload: []byte(payload), Retain: retained})
}

// runCommands drains the queue until ctx ends.
func runCommands(t *testing.T, c *Coordinator) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = c.commandLoop(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

// waitForCall waits for one actuator call.
func waitForCall(t *testing.T, h *harness) actuatorCall {
	t.Helper()
	select {
	case c := <-h.src.actuatorCh:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no actuator call arrived")
		return actuatorCall{}
	}
}

// Not parallel: the subtests share one harness and one actuator
// channel, so they have to run in sequence to read their own call.
func TestCommandsReachTheConsole(t *testing.T) {
	h := controlHarness(t, "")
	runCommands(t, h.c)

	tests := []struct {
		name    string
		topic   string
		payload string
		want    actuatorCall
	}{
		{
			name:  "restart resolves the MAC to the API id",
			topic: "unifi/default/device/00005e005302/cmd/restart",
			want:  actuatorCall{kind: "restart", id: "id-sw"},
		},
		{
			name:  "power cycle carries the port index",
			topic: "unifi/default/device/00005e005302/port/1/cmd/power_cycle",
			want:  actuatorCall{kind: "power_cycle", id: "id-sw", portIdx: 1},
		},
		{
			name:    "locate on",
			topic:   "unifi/default/device/00005e005302/cmd/locate/set",
			payload: "ON",
			want:    actuatorCall{kind: "locate", mac: swMAC, on: true},
		},
		{
			name:    "locate off",
			topic:   "unifi/default/device/00005e005302/cmd/locate/set",
			payload: "OFF",
			want:    actuatorCall{kind: "locate", mac: swMAC, on: false},
		},
		{
			name:    "wlan toggle",
			topic:   "unifi/default/wlan/wlan-1/enabled/set",
			payload: "OFF",
			want:    actuatorCall{kind: "wlan", id: "wlan-1", on: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deliver(h.c, tt.topic, tt.payload, false)
			got := waitForCall(t, h)
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Rule 2 from command.go: on every reconnect the broker re-delivers the
// last retained message per filter. Without this check a stale
// `mosquitto_pub -r` from a test months ago power-cycles a real port on
// every daemon start.
func TestRetainedCommandsAreDropped(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	runCommands(t, h.c)

	deliver(h.c, "unifi/default/device/00005e005302/port/1/cmd/power_cycle", "", true)

	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a retained command executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}

	// The same command as a live message must work.
	deliver(h.c, "unifi/default/device/00005e005302/port/1/cmd/power_cycle", "", false)
	if got := waitForCall(t, h).kind; got != "power_cycle" {
		t.Errorf("live command produced %q", got)
	}
}

// Rule 1: the handler runs inline in the MQTT read loop, the same
// goroutine that decodes acknowledgements and feeds the keep-alive
// watchdog. Blocking there makes the watchdog declare a healthy
// connection dead.
func TestHandlerNeverBlocks(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	// Deliberately no command loop: nothing drains the queue.

	done := make(chan struct{})
	go func() {
		// Far more than the queue holds.
		for range commandQueueSize * 4 {
			deliver(h.c, "unifi/default/device/00005e005302/cmd/restart", "", false)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the command handler blocked when the queue filled up")
	}
}

// Rule 3: the state comes back from the console. A failed command must
// therefore trigger a re-poll, so a Home Assistant switch that flipped
// optimistically snaps back to the truth.
func TestFailedCommandStillTriggersRefresh(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	h.src.actuatorErr = errors.New("console rejected it")
	runCommands(t, h.c)

	deliver(h.c, "unifi/default/device/00005e005302/cmd/locate/set", "ON", false)

	select {
	case <-h.c.nudgeDevices:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed command did not schedule a refresh")
	}
}

func TestSuccessfulCommandTriggersRefresh(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	runCommands(t, h.c)

	deliver(h.c, "unifi/default/client/00005e005310/blocked/set", "ON", false)
	waitForCall(t, h)

	select {
	case <-h.c.nudgeClients:
	case <-time.After(2 * time.Second):
		t.Fatal("a client command did not schedule a client refresh")
	}
}

// A disabled control must ignore its topic entirely, even if something
// publishes to it.
func TestDisabledControlsIgnoreCommands(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	h.c.cfg.Controls.DeviceRestart = false
	runCommands(t, h.c)

	deliver(h.c, "unifi/default/device/00005e005302/cmd/restart", "", false)
	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a disabled control executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestControlsOffSubscribesToNothing(t *testing.T) {
	t.Parallel()

	sub := &fakeSubscriber{}
	h := newHarness(t, nil) // default config: controls off
	h.c.SetSubscriber(sub)

	if err := h.c.subscribeCommands(t.Context()); err != nil {
		t.Fatalf("subscribeCommands: %v", err)
	}
	if len(sub.filters) != 0 {
		t.Errorf("subscribed to %v with controls disabled", sub.filters)
	}
}

func TestCommandTopicsAreWildcards(t *testing.T) {
	t.Parallel()

	sub := &fakeSubscriber{}
	h := controlHarness(t, "")
	h.c.SetSubscriber(sub)

	if err := h.c.subscribeCommands(t.Context()); err != nil {
		t.Fatalf("subscribeCommands: %v", err)
	}

	// One subscription per shape, not per object: 120 clients would
	// otherwise need 120 subscriptions and one more per new client.
	for _, f := range sub.filters {
		if !strings.Contains(f, "+") {
			t.Errorf("filter %q is not a wildcard", f)
		}
	}
	if len(sub.filters) > 8 {
		t.Errorf("subscribed to %d filters, want one per command shape", len(sub.filters))
	}
}

// A command naming a device that is not in the current poll cannot be
// resolved to an API id. It must fail loudly rather than sending a
// request with an empty id.
func TestCommandForUnknownDeviceFails(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	runCommands(t, h.c)

	deliver(h.c, "unifi/default/device/aabbccddeeff/cmd/restart", "", false)
	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a command for an unknown device reached the console: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMalformedCommandsAreIgnored(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	runCommands(t, h.c)

	for _, topic := range []string{
		"unifi/default/device/not-a-mac/cmd/restart",
		"unifi/default/device/00005e005302/port/xyz/cmd/power_cycle",
		"unifi/default/device/00005e005302/cmd/unknown",
		"unifi/default/nonsense/00005e005302/cmd/restart",
		"unifi/default/device",
	} {
		deliver(h.c, topic, "", false)
	}

	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a malformed command executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestGuestAuthorizeMinutes(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	// Guests are excluded by default, and an excluded client has no
	// entity — so it has no authorize button either. Publishing the
	// guest means opting into seeing it.
	h.c.cfg.Clients.ExcludeGuests = false

	// The client has to be known for its API id to resolve.
	h.src.mu.Lock()
	h.src.clients = []model.Client{{
		MAC: model.MustParseMAC("00:00:5e:00:53:10"), ID: "c1", Name: "Guest", IsGuest: true,
	}}
	h.src.mu.Unlock()
	if err := h.c.refreshClients(t.Context()); err != nil {
		t.Fatalf("refreshClients: %v", err)
	}
	runCommands(t, h.c)

	tests := []struct {
		payload string
		want    int
	}{
		{"", 0},                // the button's default press
		{"PRESS", 0},           // Home Assistant's button payload
		{"60", 60},             // a plain number
		{`{"minutes":30}`, 30}, // the documented JSON form
	}
	for _, tt := range tests {
		deliver(h.c, "unifi/default/client/00005e005310/cmd/authorize", tt.payload, false)
		got := waitForCall(t, h)
		if got.kind != "authorize" || got.minutes != tt.want {
			t.Errorf("payload %q produced %+v, want minutes %d", tt.payload, got, tt.want)
		}
	}
}

func TestSwitchPayloadForms(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		payload string
		want    bool
	}{
		{"ON", true},
		{"on", true},
		{"true", true},
		{"1", true},
		{"OFF", false},
		{"off", false},
		{"false", false},
		{"0", false},
		{"nonsense", false},
	} {
		if got := isOn(tt.payload); got != tt.want {
			t.Errorf("isOn(%q) = %v, want %v", tt.payload, got, tt.want)
		}
	}
}
