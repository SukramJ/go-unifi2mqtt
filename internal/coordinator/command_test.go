// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	hapub "github.com/SukramJ/go-hamqtt/publisher"

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

// deliver feeds a message to the command handler as the router would:
// normalised per spec §5.3, with the empty and malformed payloads the
// router never hands over dropped here too.
func deliver(c *Coordinator, topic, payload string, retained bool) {
	v, err := hapub.ParseSet([]byte(payload))
	if err != nil {
		return
	}
	c.onCommand(topic, payload, retained, v)
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
			name:    "restart resolves the MAC to the API id",
			topic:   "unifi/set/default/device/00005e005302/cmd/restart",
			payload: "PRESS",
			want:    actuatorCall{kind: "restart", id: "id-sw"},
		},
		{
			name:    "power cycle carries the port index",
			topic:   "unifi/set/default/device/00005e005302/port/1/cmd/power_cycle",
			payload: `{"val":"PRESS"}`,
			want:    actuatorCall{kind: "power_cycle", id: "id-sw", portIdx: 1},
		},
		{
			name:    "locate on",
			topic:   "unifi/set/default/device/00005e005302/cmd/locate",
			payload: "ON",
			want:    actuatorCall{kind: "locate", mac: swMAC, on: true},
		},
		{
			name:    "locate off",
			topic:   "unifi/set/default/device/00005e005302/cmd/locate",
			payload: "OFF",
			want:    actuatorCall{kind: "locate", mac: swMAC, on: false},
		},
		{
			name:    "wlan toggle",
			topic:   "unifi/set/default/wlan/wlan-1/enabled",
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

	deliver(h.c, "unifi/set/default/device/00005e005302/port/1/cmd/power_cycle", "PRESS", true)

	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a retained command executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}

	// The same command as a live message must work.
	deliver(h.c, "unifi/set/default/device/00005e005302/port/1/cmd/power_cycle", "PRESS", false)
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
			deliver(h.c, "unifi/set/default/device/00005e005302/cmd/restart", "PRESS", false)
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

	deliver(h.c, "unifi/set/default/device/00005e005302/cmd/locate", "ON", false)

	select {
	case <-h.c.nudgeStatic:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed command did not schedule a refresh")
	}
}

// TestEachCommandNudgesTheLoopThatPublishesItsState pins the pairing a
// nudge depends on and nothing checked: the loop a completed command
// wakes must be the loop that publishes that control's state topic.
//
// The locate LED and the SSID enable flag are both read and published
// by the *static* loop — the fast device loop's list carries neither —
// so nudging the device loop for them republished an unchanged snapshot
// and left the entity on its old value until the next hourly poll. It
// looks exactly like a working nudge.
func TestEachCommandNudgesTheLoopThatPublishesItsState(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	// Drained so a stray nudge from an earlier command cannot satisfy a
	// later assertion.
	drain := func() {
		for _, ch := range []chan struct{}{h.c.nudgeDevices, h.c.nudgeClients, h.c.nudgeStatic} {
			select {
			case <-ch:
			default:
			}
		}
	}

	for _, tc := range []struct {
		name string
		cmd  command
		want chan struct{}
		not  []chan struct{}
	}{
		{
			"restart",
			command{kind: cmdKindRestart},
			h.c.nudgeDevices,
			[]chan struct{}{h.c.nudgeStatic, h.c.nudgeClients},
		},
		{
			"power_cycle",
			command{kind: cmdKindPowerCycle},
			h.c.nudgeDevices,
			[]chan struct{}{h.c.nudgeStatic, h.c.nudgeClients},
		},
		{
			"locate",
			command{kind: cmdKindLocate},
			h.c.nudgeStatic,
			[]chan struct{}{h.c.nudgeDevices, h.c.nudgeClients},
		},
		{
			"wlan",
			command{kind: cmdKindWLAN},
			h.c.nudgeStatic,
			[]chan struct{}{h.c.nudgeDevices, h.c.nudgeClients},
		},
		{
			"block",
			command{kind: cmdKindBlock},
			h.c.nudgeClients,
			[]chan struct{}{h.c.nudgeDevices, h.c.nudgeStatic},
		},
		{
			"authorize",
			command{kind: cmdKindAuthorize},
			h.c.nudgeClients,
			[]chan struct{}{h.c.nudgeDevices, h.c.nudgeStatic},
		},
	} {
		drain()
		h.c.scheduleRefresh(tc.cmd)
		select {
		case <-tc.want:
		default:
			t.Errorf("%s: nudged no refresh on the loop that publishes its state", tc.name)
		}
		for _, ch := range tc.not {
			select {
			case <-ch:
				t.Errorf("%s: nudged a loop that does not publish its state", tc.name)
			default:
			}
		}
	}
}

func TestSuccessfulCommandTriggersRefresh(t *testing.T) {
	t.Parallel()

	h := controlHarness(t, "")
	runCommands(t, h.c)

	deliver(h.c, "unifi/set/default/client/00005e005310/blocked", "ON", false)
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

	deliver(h.c, "unifi/set/default/device/00005e005302/cmd/restart", "PRESS", false)
	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a disabled control executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

// With controls off no command filter is subscribed; the maintenance
// filter is, because maintenance is on by default and independent of
// the controls. With both off nothing is subscribed at all.
func TestControlsOffSubscribesToNothing(t *testing.T) {
	t.Parallel()

	sub := &fakeSubscriber{}
	h := newHarness(t, nil) // default config: controls off, maintenance on
	h.c.SetSubscriber(sub)

	if err := h.c.subscribeCommands(t.Context()); err != nil {
		t.Fatalf("subscribeCommands: %v", err)
	}
	if want := []string{"unifi/maintenance/set/#"}; !slices.Equal(sub.filters, want) {
		t.Errorf("subscribed to %v with controls disabled, want only %v", sub.filters, want)
	}

	cfg := testConfig()
	cfg.MQTTMaintenance = false
	sub = &fakeSubscriber{}
	h = newHarness(t, cfg)
	h.c.SetSubscriber(sub)
	if err := h.c.subscribeCommands(t.Context()); err != nil {
		t.Fatalf("subscribeCommands: %v", err)
	}
	if len(sub.filters) != 0 {
		t.Errorf("subscribed to %v with controls and maintenance disabled", sub.filters)
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
		if !strings.ContainsAny(f, "+#") {
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

	deliver(h.c, "unifi/set/default/device/aabbccddeeff/cmd/restart", "PRESS", false)
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
		"unifi/set/default/device/not-a-mac/cmd/restart",
		"unifi/set/default/device/00005e005302/port/xyz/cmd/power_cycle",
		"unifi/set/default/device/00005e005302/cmd/unknown",
		"unifi/set/default/nonsense/00005e005302/cmd/restart",
		"unifi/set/default/device",
	} {
		deliver(h.c, topic, "PRESS", false)
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
		{"PRESS", 0},           // Home Assistant's button payload
		{"{}", 0},              // the documented empty JSON form
		{"60", 60},             // a plain number
		{`{"val":45}`, 45},     // the plain number, wrapped (spec §5.3)
		{`{"minutes":30}`, 30}, // the documented JSON form
	}
	for _, tt := range tests {
		deliver(h.c, "unifi/set/default/client/00005e005310/cmd/authorize", tt.payload, false)
		got := waitForCall(t, h)
		if got.kind != "authorize" || got.minutes != tt.want {
			t.Errorf("payload %q produced %+v, want minutes %d", tt.payload, got, tt.want)
		}
	}

	// 2.0 dropped the empty authorize: an empty payload is what clearing
	// a retained topic looks like, and the convention ignores it.
	deliver(h.c, "unifi/set/default/client/00005e005310/cmd/authorize", "", false)
	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("an empty authorize executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}

// Switch requests take spec §5.3's boolean spellings, plain or wrapped,
// and anything else is rejected rather than read as "off" — which is
// what 1.x did with "nonsense".
func TestSwitchPayloadForms(t *testing.T) {
	h := controlHarness(t, "")
	runCommands(t, h.c)

	topic := "unifi/set/default/device/00005e005302/cmd/locate"
	for _, tt := range []struct {
		payload string
		want    bool
	}{
		{"ON", true},
		{"on", true},
		{"true", true},
		{"1", true},
		{"yes", true},
		{`{"val":true}`, true},
		{"OFF", false},
		{"off", false},
		{"false", false},
		{"0", false},
		{"no", false},
		{`{"val":"off"}`, false},
	} {
		deliver(h.c, topic, tt.payload, false)
		if got := waitForCall(t, h); got.kind != "locate" || got.on != tt.want {
			t.Errorf("payload %q produced %+v, want on=%v", tt.payload, got, tt.want)
		}
	}

	for _, payload := range []string{"nonsense", "home", `{"brightness":3}`} {
		deliver(h.c, topic, payload, false)
	}
	select {
	case c := <-h.src.actuatorCh:
		t.Fatalf("a payload that is not a boolean executed: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}
}
