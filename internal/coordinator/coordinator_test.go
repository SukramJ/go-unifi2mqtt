// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// --- test doubles ----------------------------------------------------

// fakeBroker records every publish so tests can assert on the resulting
// topic tree rather than on call sequences.
type fakeBroker struct {
	mu sync.Mutex
	// msgs is the full ordered history, including repeats.
	msgs []published
	// fail, when set, makes every publish fail.
	fail error
}

type published struct {
	topic   string
	payload string
	qos     mqtt.QoS
	retain  bool
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, _ ...mqtt.PublishOption) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.msgs = append(b.msgs, published{topic: topic, payload: string(payload), qos: qos, retain: retain})
	return nil
}

// latest returns the most recent payload for topic.
func (b *fakeBroker) latest(topic string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.msgs) - 1; i >= 0; i-- {
		if b.msgs[i].topic == topic {
			return b.msgs[i].payload, true
		}
	}
	return "", false
}

// count returns how often topic was published.
func (b *fakeBroker) count(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int
	for _, m := range b.msgs {
		if m.topic == topic {
			n++
		}
	}
	return n
}

func (b *fakeBroker) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

func (b *fakeBroker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = nil
}

// topicsWithPrefix returns the distinct topics published under prefix.
func (b *fakeBroker) topicsWithPrefix(prefix string) map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]bool{}
	for _, m := range b.msgs {
		if strings.HasPrefix(m.topic, prefix) {
			out[m.topic] = true
		}
	}
	return out
}

// fakeSource is a stub console. Each field can be swapped per test.
type fakeSource struct {
	mu sync.Mutex

	devices  []model.Device
	details  []model.Device
	networks []model.Network
	wlans    []model.WLAN
	clients  []model.Client
	stats    map[string]model.DeviceStats

	health    model.Health
	healthErr error

	devicesErr  error
	detailsErr  error
	statsErr    error
	actuatorErr error

	actuators []actuatorCall
	// actuatorCh, when set, receives each call so a test can wait for
	// the asynchronous command loop without sleeping.
	actuatorCh chan actuatorCall

	// calls counts requests per method for cadence assertions.
	calls map[string]int
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		stats: map[string]model.DeviceStats{},
		calls: map[string]int{},
	}
}

func (s *fakeSource) note(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[name]++
}

func (s *fakeSource) callCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

func (s *fakeSource) Info(context.Context) (model.ControllerInfo, error) {
	s.note("Info")
	return model.ControllerInfo{ApplicationVersion: "10.5.67"}, nil
}

func (s *fakeSource) Devices(context.Context, string) ([]model.Device, error) {
	s.note("Devices")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.devicesErr != nil {
		return nil, s.devicesErr
	}
	return append([]model.Device(nil), s.devices...), nil
}

func (s *fakeSource) DevicesWithDetails(context.Context, string) ([]model.Device, error) {
	s.note("DevicesWithDetails")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detailsErr != nil {
		return nil, s.detailsErr
	}
	return append([]model.Device(nil), s.details...), nil
}

func (s *fakeSource) DeviceStats(_ context.Context, _, deviceID string) (model.DeviceStats, error) {
	s.note("DeviceStats")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statsErr != nil {
		return model.DeviceStats{}, s.statsErr
	}
	return s.stats[deviceID], nil
}

func (s *fakeSource) Networks(context.Context, string) ([]model.Network, error) {
	s.note("Networks")
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Network(nil), s.networks...), nil
}

func (s *fakeSource) WLANs(context.Context, string) ([]model.WLAN, error) {
	s.note("WLANs")
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.WLAN(nil), s.wlans...), nil
}

// actuatorCall records one write-back so tests can assert on what
// actually reached the console.
type actuatorCall struct {
	kind    string
	id      string
	mac     model.MAC
	portIdx int
	on      bool
	minutes int
}

func (s *fakeSource) RestartDevice(_ context.Context, _, deviceID string) error {
	s.note("RestartDevice")
	return s.recordActuator(actuatorCall{kind: "restart", id: deviceID})
}

func (s *fakeSource) PowerCyclePort(_ context.Context, _, deviceID string, portIdx int) error {
	s.note("PowerCyclePort")
	return s.recordActuator(actuatorCall{kind: "power_cycle", id: deviceID, portIdx: portIdx})
}

func (s *fakeSource) AuthorizeGuest(_ context.Context, _, clientID string, minutes int) error {
	s.note("AuthorizeGuest")
	return s.recordActuator(actuatorCall{kind: "authorize", id: clientID, minutes: minutes})
}

func (s *fakeSource) SetLocate(_ context.Context, _ string, mac model.MAC, on bool) error {
	s.note("SetLocate")
	return s.recordActuator(actuatorCall{kind: "locate", mac: mac, on: on})
}

func (s *fakeSource) SetClientBlocked(_ context.Context, _ string, mac model.MAC, blocked bool) error {
	s.note("SetClientBlocked")
	return s.recordActuator(actuatorCall{kind: "block", mac: mac, on: blocked})
}

func (s *fakeSource) SetWLANEnabled(_ context.Context, _, wlanID string, enabled bool) error {
	s.note("SetWLANEnabled")
	return s.recordActuator(actuatorCall{kind: "wlan", id: wlanID, on: enabled})
}

func (s *fakeSource) recordActuator(c actuatorCall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.actuatorErr != nil {
		return s.actuatorErr
	}
	s.actuators = append(s.actuators, c)
	if s.actuatorCh != nil {
		select {
		case s.actuatorCh <- c:
		default:
		}
	}
	return nil
}

func (s *fakeSource) Health(context.Context, string) (model.Health, error) {
	s.note("Health")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.healthErr != nil {
		return model.Health{}, s.healthErr
	}
	return s.health, nil
}

func (s *fakeSource) Clients(context.Context, string) ([]model.Client, error) {
	s.note("Clients")
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Client(nil), s.clients...), nil
}

// --- fixtures --------------------------------------------------------

var (
	gwMAC = model.MustParseMAC("00:00:5e:00:53:01")
	swMAC = model.MustParseMAC("00:00:5e:00:53:02")
	apMAC = model.MustParseMAC("00:00:5e:00:53:03")
)

func testConfig() *config.Config {
	cfg, err := config.Load(strings.NewReader(`
HOST: 192.0.2.1
API_KEY: k
MQTT_SERVER: broker
MQTT_TOPIC: unifi
`), config.MapEnv{})
	if err != nil {
		panic(err)
	}
	return cfg
}

func testSite() model.Site {
	return model.Site{ID: "site-uuid", Name: "Default", Internal: "default"}
}

func sampleDevices() []model.Device {
	return []model.Device{
		{
			MAC: gwMAC, ID: "id-gw", Name: "Gateway", Model: "UCG-Ultra",
			Type: model.DeviceGateway, State: model.DeviceOnline,
			IP: netip.MustParseAddr("192.0.2.1"), Supported: true,
			Firmware: "4.3.6",
		},
		{
			MAC: swMAC, ID: "id-sw", Name: "Switch", Model: "USW-Pro-24-PoE",
			Type: model.DeviceSwitch, State: model.DeviceOnline,
			IP: netip.MustParseAddr("192.0.2.2"), Supported: true,
			Firmware: "7.0.25", UpdateAvail: true,
		},
		{
			MAC: apMAC, ID: "id-ap", Name: "AP", Model: "U6-Pro",
			Type: model.DeviceAccessPoint, State: model.DeviceOffline,
			Supported: true, Firmware: "6.6.55",
		},
	}
}

// sampleDetails adds what only the detail endpoint carries.
func sampleDetails() []model.Device {
	d := sampleDevices()
	d[1].UplinkID = "id-gw"
	d[1].UplinkMAC = gwMAC
	d[1].Ports = []model.Port{
		{
			Idx: 1, State: model.PortUp, Connector: "RJ45", SpeedMbps: 1000, MaxSpeedMbps: 1000,
			PoE: &model.PoEState{Enabled: true, Standard: "802.3at", Type: 2, State: "UP"},
		},
		{Idx: 25, State: model.PortUp, Connector: "SFPPLUS", SpeedMbps: 10000, MaxSpeedMbps: 10000},
	}
	d[2].UplinkID = "id-sw"
	d[2].UplinkMAC = swMAC
	d[2].Radios = []model.Radio{
		{FrequencyGHz: 2.4, Channel: 6, ChannelWidth: 20, Standard: "802.11ax"},
		{FrequencyGHz: 5, Channel: 36, ChannelWidth: 80, Standard: "802.11ax"},
	}
	return d
}

type harness struct {
	c      *Coordinator
	broker *fakeBroker
	src    *fakeSource
	clock  *fakeClock
}

// fakeClock lets tests move time without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newHarness(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	return newHarnessWith(t, cfg, nil)
}

func newHarnessWith(t *testing.T, cfg *config.Config, caps Capabilities) *harness {
	t.Helper()
	if cfg == nil {
		cfg = testConfig()
	}

	src := newFakeSource()
	src.devices = sampleDevices()
	src.details = sampleDetails()
	src.wlans = []model.WLAN{
		{ID: "wlan-1", Name: "HomeNet", Enabled: true},
		{ID: "wlan-2", Name: "IoT", Enabled: false},
	}
	src.networks = []model.Network{
		{
			ID: "net-1", Name: "LAN", VLAN: 1, Enabled: true, Default: true,
			Subnets: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		},
	}
	src.stats["id-gw"] = model.DeviceStats{
		Uptime: 864000 * time.Second, CPUPct: 12.5, MemoryPct: 48,
		UplinkTxBps: 1048576, UplinkRxBps: 2097152,
	}
	src.stats["id-sw"] = model.DeviceStats{
		Uptime: 3600 * time.Second, CPUPct: 5, MemoryPct: 30,
		RadioTxRetry: map[float64]float64{5: 1.2},
	}

	broker := &fakeBroker{}
	clock := newFakeClock()

	c := New(Deps{
		Cfg:          cfg,
		Site:         testSite(),
		Source:       src,
		MQTT:         broker,
		Capabilities: caps,
		Info:         model.ControllerInfo{ApplicationVersion: "10.5.67"},
		Logger:       slog.New(slog.DiscardHandler),
		Now:          clock.now,
	})
	return &harness{c: c, broker: broker, src: src, clock: clock}
}

// --- tests -----------------------------------------------------------

func TestPublishesDeviceTopics(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	want := map[string]string{
		"unifi/default/device/00005e005302/state":            "ONLINE",
		"unifi/default/device/00005e005302/firmware":         "7.0.25",
		"unifi/default/device/00005e005302/update_available": "ON",
		"unifi/default/device/00005e005301/update_available": "OFF",
		"unifi/default/device/00005e005303/state":            "OFFLINE",
		// Ports and radios come from the static loop's snapshot, merged
		// into the fast loop's publish.
		"unifi/default/device/00005e005302/port/1/state":      "UP",
		"unifi/default/device/00005e005302/port/1/speed":      "1000",
		"unifi/default/device/00005e005302/port/1/poe":        "ON",
		"unifi/default/device/00005e005303/radio/2g4/channel": "6",
		"unifi/default/device/00005e005303/radio/5g/channel":  "36",
	}
	for topic, wantPayload := range want {
		got, ok := h.broker.latest(topic)
		if !ok {
			t.Errorf("topic %s was never published", topic)
			continue
		}
		if got != wantPayload {
			t.Errorf("%s = %q, want %q", topic, got, wantPayload)
		}
	}

	// An SFP+ port has no PoE hardware; publishing OFF there would
	// create a Home Assistant entity for a capability it lacks.
	if _, ok := h.broker.latest("unifi/default/device/00005e005302/port/25/poe"); ok {
		t.Error("a non-PoE port got a poe topic")
	}
}

func TestDeviceAttributes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	raw, ok := h.broker.latest("unifi/default/device/00005e005302/attributes")
	if !ok {
		t.Fatal("no attributes topic")
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatalf("attributes is not JSON: %v", err)
	}

	if got, want := attrs["model"], "USW-Pro-24-PoE"; got != want {
		t.Errorf("model = %v, want %v", got, want)
	}
	// The uplink MAC is what drives Home Assistant's via_device, so it
	// has to survive the merge from the static snapshot.
	if got, want := attrs["uplink_mac"], "00:00:5e:00:53:01"; got != want {
		t.Errorf("uplink_mac = %v, want %v", got, want)
	}
	if got, want := attrs["mac"], "00:00:5e:00:53:02"; got != want {
		t.Errorf("mac = %v, want %v", got, want)
	}

	// The gateway has no uplink; the field must be absent rather than
	// present and empty.
	raw, _ = h.broker.latest("unifi/default/device/00005e005301/attributes")
	if strings.Contains(raw, "uplink_mac") {
		t.Errorf("gateway attributes advertise an uplink: %s", raw)
	}
}

// Change detection is the difference between a quiet broker and one
// carrying hundreds of identical messages a minute.
func TestChangeDetectionSuppressesIdenticalPayloads(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	first := h.broker.total()
	if first == 0 {
		t.Fatal("nothing was published")
	}

	// Nothing changed on the console.
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got := h.broker.total(); got != first {
		t.Errorf("second identical poll published %d more messages, want 0", got-first)
	}

	// One value changes: only that topic goes out.
	h.src.mu.Lock()
	h.src.devices[1].State = model.DeviceUpdating
	h.src.mu.Unlock()

	h.broker.reset()
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got := h.broker.total(); got != 1 {
		t.Errorf("published %d messages for a single changed value, want 1", got)
	}
	if got, _ := h.broker.latest("unifi/default/device/00005e005302/state"); got != "UPDATING" {
		t.Errorf("state = %q, want UPDATING", got)
	}
}

// Without the periodic full republish a subscriber that missed a
// message — or one not using retained values — would stay stale forever.
func TestForcedRepublish(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.ForceRepublish = 600
	h := newHarness(t, cfg)

	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	first := h.broker.total()

	// Before the deadline: still suppressed.
	h.clock.advance(599 * time.Second)
	h.broker.reset()
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got := h.broker.total(); got != 0 {
		t.Errorf("published %d messages before the force deadline, want 0", got)
	}

	// After it: everything again.
	h.clock.advance(2 * time.Second)
	h.broker.reset()
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got := h.broker.total(); got != first {
		t.Errorf("forced republish sent %d messages, want all %d", got, first)
	}
}

func TestDeviceStatsSkipsOfflineDevices(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshDeviceStats(t.Context()); err != nil {
		t.Fatalf("refreshDeviceStats: %v", err)
	}

	// Two of the three sample devices are online. An offline device
	// answers with an empty sample, so calling it is pure overhead —
	// and publishing zeros would draw a CPU drop to 0 in Home Assistant
	// rather than a gap.
	if got, want := h.src.callCount("DeviceStats"), 2; got != want {
		t.Errorf("made %d statistics calls, want %d (offline devices skipped)", got, want)
	}
	if _, ok := h.broker.latest("unifi/default/device/00005e005303/cpu_utilization"); ok {
		t.Error("an offline device got CPU statistics published")
	}

	if got, _ := h.broker.latest("unifi/default/device/00005e005301/uptime"); got != "864000" {
		t.Errorf("uptime = %q, want 864000", got)
	}
	if got, _ := h.broker.latest("unifi/default/device/00005e005301/cpu_utilization"); got != "12.5" {
		t.Errorf("cpu_utilization = %q, want 12.5", got)
	}
	// Radio retry rates are keyed by band, the only identifier the
	// statistics response carries.
	if got, _ := h.broker.latest("unifi/default/device/00005e005302/radio/5g/tx_retries"); got != "1.2" {
		t.Errorf("tx_retries = %q, want 1.2", got)
	}
}

// The console reports values like 12.500000001; formatting with %v
// would make an identical reading look different every poll and defeat
// change detection entirely.
func TestPercentFormattingIsStable(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.src.stats["id-gw"] = model.DeviceStats{CPUPct: 12.500000001}
	if err := h.c.refreshDeviceStats(t.Context()); err != nil {
		t.Fatalf("refreshDeviceStats: %v", err)
	}
	h.src.stats["id-gw"] = model.DeviceStats{CPUPct: 12.499999999}

	h.broker.reset()
	if err := h.c.refreshDeviceStats(t.Context()); err != nil {
		t.Fatalf("refreshDeviceStats: %v", err)
	}
	if got, ok := h.broker.latest("unifi/default/device/00005e005301/cpu_utilization"); ok {
		t.Errorf("a jittering float republished as %q, want it suppressed", got)
	}
}

func TestWLANPublication(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}

	if got, _ := h.broker.latest("unifi/default/wlan/wlan-1/enabled"); got != "ON" {
		t.Errorf("wlan-1 enabled = %q, want ON", got)
	}
	if got, _ := h.broker.latest("unifi/default/wlan/wlan-2/enabled"); got != "OFF" {
		t.Errorf("wlan-2 enabled = %q, want OFF", got)
	}
	if got, _ := h.broker.latest("unifi/default/wlan/wlan-1/name"); got != "HomeNet" {
		t.Errorf("wlan-1 name = %q, want HomeNet", got)
	}
}

// A device that disappears must lose its change-detection memory, or it
// would come back silent: every value would compare equal to what was
// remembered and nothing would be republished.
func TestRemovedDeviceForgetsItsTopics(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}

	prefix := "unifi/default/device/00005e005303/"
	if len(h.c.pub.knownTopics()) == 0 {
		t.Fatal("nothing was remembered")
	}

	// The AP is removed from the site.
	h.src.mu.Lock()
	h.src.devices = h.src.devices[:2]
	h.src.mu.Unlock()

	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	for _, topic := range h.c.pub.knownTopics() {
		if strings.HasPrefix(topic, prefix) {
			t.Errorf("topic %s is still remembered after the device disappeared", topic)
		}
	}

	// It comes back: every value must be republished, not suppressed.
	h.src.mu.Lock()
	h.src.devices = sampleDevices()
	h.src.mu.Unlock()

	h.broker.reset()
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got := h.broker.topicsWithPrefix(prefix); len(got) == 0 {
		t.Error("a returning device published nothing — its memory was not cleared")
	}
}

func TestAvailabilityLifecycle(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	topic := h.c.AvailabilityTopic()
	if got, want := topic, "unifi/bridge/status"; got != want {
		t.Errorf("AvailabilityTopic() = %q, want %q", got, want)
	}

	h.c.OnConnect(t.Context())
	if got, _ := h.broker.latest(topic); got != "online" {
		t.Errorf("after connect = %q, want online", got)
	}
	// A clean DISCONNECT suppresses the broker-side will, so shutdown
	// has to publish offline itself.
	h.c.AnnounceOffline(t.Context())
	if got, _ := h.broker.latest(topic); got != "offline" {
		t.Errorf("after shutdown = %q, want offline", got)
	}

	// Availability must be retained and QoS 1: a subscriber connecting
	// later has to learn the bridge is up.
	h.broker.mu.Lock()
	defer h.broker.mu.Unlock()
	for _, m := range h.broker.msgs {
		if m.topic != topic {
			continue
		}
		if !m.retain {
			t.Error("availability was published without the retain flag")
		}
		if m.qos != mqtt.QoS1 {
			t.Errorf("availability QoS = %v, want 1", m.qos)
		}
	}
}

// A reconnect may land on a broker that lost its retained store, or on
// a different broker entirely. Suppressing every unchanged value
// against a stale memory would leave that broker permanently empty.
func TestReconnectRepublishesEverything(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	first := h.broker.total()

	h.c.OnConnect(t.Context()) // simulates a reconnect
	h.broker.reset()

	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got := h.broker.total(); got != first {
		t.Errorf("after a reconnect %d messages went out, want all %d", got, first)
	}
}

func TestBridgeInfo(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.c.OnConnect(t.Context())

	raw, ok := h.broker.latest("unifi/bridge/info")
	if !ok {
		t.Fatal("no bridge info topic")
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("bridge info is not JSON: %v", err)
	}
	if got, want := info["application_version"], "10.5.67"; got != want {
		t.Errorf("application_version = %v, want %v", got, want)
	}
	if got, want := info["site"], "default"; got != want {
		t.Errorf("site = %v, want %v", got, want)
	}
	// The counters used to live here and permanently read 1, because
	// this topic is written from the connect hook before anything has
	// been published.
	if _, ok := info["published_topics"]; ok {
		t.Error("bridge info carries a publish counter that is always 1")
	}
	// The API key must never reach a payload.
	if strings.Contains(raw, "\"k\"") {
		t.Errorf("bridge info leaked a credential: %s", raw)
	}
}

// A console that stops answering one endpoint must not take the daemon
// down — that is what separates a transient failure from a fatal one.
func TestNonFatalErrorKeepsLoopAlive(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.src.devicesErr = errors.New("connection refused")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- h.c.loop(ctx, "devices", 10*time.Millisecond, h.c.refreshDevices)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Errorf("loop returned %v, want context.Canceled — a refused connection must not stop it", err)
	}
	if got := h.src.callCount("Devices"); got < 2 {
		t.Errorf("loop made %d attempts, want it to keep retrying", got)
	}
	// The failure should be visible on the bridge error topic.
	if _, ok := h.broker.latest("unifi/bridge/error"); !ok {
		t.Error("a loop failure was not surfaced on the bridge error topic")
	}
}

// An invalid API key cannot fix itself. Retrying forever would hammer
// the console while publishing nothing, so it has to stop the loop.
func TestFatalErrorStopsLoop(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.src.devicesErr = unifi.ErrUnauthorized

	err := h.c.loop(t.Context(), "devices", time.Hour, h.c.refreshDevices)
	if !errors.Is(err, unifi.ErrUnauthorized) {
		t.Errorf("loop returned %v, want ErrUnauthorized", err)
	}
	if got := h.src.callCount("Devices"); got != 1 {
		t.Errorf("made %d attempts on an invalid key, want 1", got)
	}
}

// A broker outage must not abort a poll: the next cycle republishes.
func TestPublishFailureDoesNotAbortPoll(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.broker.fail = errors.New("broker unreachable")

	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Errorf("refreshDevices returned %v, want nil — a broker outage is not a poll failure", err)
	}

	// Once the broker returns, the values must go out — the failed
	// publishes must not have been recorded as sent.
	h.broker.fail = nil
	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if got, ok := h.broker.latest("unifi/default/device/00005e005302/state"); !ok || got != "ONLINE" {
		t.Errorf("state after broker recovery = %q (present=%v), want ONLINE", got, ok)
	}
}

// A device without a MAC has no stable topic to publish under. It must
// be skipped, not published under an empty segment.
func TestDeviceWithoutMACIsSkipped(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.src.devices = []model.Device{{ID: "id-x", Name: "Broken", State: model.DeviceOnline}}

	if err := h.c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	for _, topic := range h.c.pub.knownTopics() {
		if strings.Contains(topic, "device//") {
			t.Errorf("published under an empty MAC segment: %s", topic)
		}
	}
	if h.broker.total() != 0 {
		t.Errorf("published %d messages for a device with no MAC, want 0", h.broker.total())
	}
}

func TestRunPrimesStaticBeforePolling(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.RefreshDevices = 3600
	cfg.RefreshStatic = 3600
	h := newHarness(t, cfg)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	if err := h.c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The very first device publish must already carry ports and radios,
	// otherwise Home Assistant briefly sees a flat topology and creates
	// entities for it.
	if got, _ := h.broker.latest("unifi/default/device/00005e005302/port/1/state"); got != "UP" {
		t.Errorf("port state = %q, want UP — static data was not primed before the first poll", got)
	}
	if got, _ := h.broker.latest("unifi/default/device/00005e005302/attributes"); !strings.Contains(got, "uplink_mac") {
		t.Error("first device publish carried no uplink — static data was not primed")
	}
}

// Regression: the MQTT lifecycle calls its connect hook from inside
// Start(), so a hook that publishes runs before any code written after
// Start() does. Wiring the publisher afterwards used to panic here and
// take the whole daemon down on the first successful broker connect.
func TestOnConnectWithoutPublisherDoesNotPanic(t *testing.T) {
	t.Parallel()

	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: newFakeSource(),
		// MQTT deliberately unset.
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})

	// Must log and carry on rather than dereference a nil publisher.
	c.OnConnect(t.Context())
	c.AnnounceOffline(t.Context())

	if err := c.refreshDevices(t.Context()); err != nil {
		t.Errorf("refreshDevices returned %v, want nil", err)
	}
}

func TestPublishWithoutPublisherReportsAnError(t *testing.T) {
	t.Parallel()

	p := newPublisher(nil, 0, newFakeClock().now, slog.New(slog.DiscardHandler))
	if err := p.publish(t.Context(), "a", "v"); !errors.Is(err, ErrNoPublisher) {
		t.Errorf("publish error = %v, want ErrNoPublisher", err)
	}
	if err := p.publishRaw(t.Context(), "a", "v", mqtt.QoS1); !errors.Is(err, ErrNoPublisher) {
		t.Errorf("publishRaw error = %v, want ErrNoPublisher", err)
	}
}

// SetPublisher has to take effect for subsequent publishes — that is
// the whole point of the two-step wiring.
func TestSetPublisherTakesEffect(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{}
	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: newFakeSource(),
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})
	c.SetPublisher(broker)

	c.OnConnect(t.Context())
	if got, _ := broker.latest("unifi/bridge/status"); got != "online" {
		t.Errorf("availability = %q, want online after SetPublisher", got)
	}
}
