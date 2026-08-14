// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package coordinator orchestrates the UniFi → MQTT data flow.
//
// One coordinator owns the console client and the MQTT publisher and
// runs a fan-out of long-lived goroutines — one per polling cadence —
// until its context is cancelled or a loop returns a fatal error.
//
// The split between loops follows how fast the underlying data actually
// changes, not how it is grouped in the API (CONCEPT.md §8.1):
//
//	devices  60 s   state, firmware, update available
//	stats    60 s   CPU, memory, uptime, uplink rates (one call per device)
//	static    1 h   ports, radios, uplinks, network and WLAN catalogues
//
// Device details sit on the hourly loop because cabling and radio
// configuration change rarely, while the per-device statistics that do
// change constantly stay on the fast one. Polling everything on the
// fast cadence would cost 1+2N requests per minute against a box that
// also routes the household's traffic.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
	"github.com/SukramJ/go-unifi2mqtt/internal/version"
)

// Capabilities reports which optional features the console client can
// currently serve. The coordinator queries it before announcing an
// entity, so it never offers one it cannot back with values
// (CONCEPT.md §3.3).
type Capabilities interface {
	Has(c unifi.Capability) bool
}

// noCapabilities is the nil-safe default: without a classic layer there
// is nothing extra to offer.
type noCapabilities struct{}

func (noCapabilities) Has(unifi.Capability) bool { return false }

// Source is the console-side contract, narrowed to what the coordinator
// reads. Defined here rather than imported so tests can stub it without
// dragging in the HTTP machinery.
type Source interface {
	Info(ctx context.Context) (model.ControllerInfo, error)
	Devices(ctx context.Context, siteID string) ([]model.Device, error)
	DevicesWithDetails(ctx context.Context, siteID string) ([]model.Device, error)
	DeviceStats(ctx context.Context, siteID, deviceID string) (model.DeviceStats, error)
	Networks(ctx context.Context, siteID string) ([]model.Network, error)
	WLANs(ctx context.Context, siteID string) ([]model.WLAN, error)
	Clients(ctx context.Context, siteID string) ([]model.Client, error)
	// Health returns the site aggregate, or unifi.ErrCapabilityUnavailable
	// when the classic layer is off — which is a configuration choice,
	// not a failure.
	Health(ctx context.Context, siteID string) (model.Health, error)

	// Actuators. The classic-only ones return
	// unifi.ErrCapabilityUnavailable when that layer is off; the
	// coordinator asks Capabilities.Has before offering the entity, so
	// that should not happen in practice.
	RestartDevice(ctx context.Context, siteID, deviceID string) error
	PowerCyclePort(ctx context.Context, siteID, deviceID string, portIdx int) error
	AuthorizeGuest(ctx context.Context, siteID, clientID string, minutes int) error
	SetLocate(ctx context.Context, siteID string, mac model.MAC, on bool) error
	SetClientBlocked(ctx context.Context, siteID string, mac model.MAC, blocked bool) error
	SetWLANEnabled(ctx context.Context, siteID, wlanID string, enabled bool) error
}

// Deps bundles the wired-in collaborators. A struct rather than a long
// parameter list so test setup can swap one dependency at a time.
type Deps struct {
	Cfg    *config.Config
	Site   model.Site
	Source Source
	MQTT   Publisher
	// Info is what the console reported at startup; republished on the
	// bridge info topic.
	Info model.ControllerInfo
	// Subscriber receives the Home Assistant birth message. Nil disables
	// re-announcing on Home Assistant restarts.
	Subscriber Subscriber
	// Capabilities reports which classic-layer features are available.
	// Nil means none, which is the correct reading when the classic
	// layer is off.
	Capabilities Capabilities
	// Logger receives diagnostics; nil uses slog.Default().
	Logger *slog.Logger
	// Now supplies wall-clock time. Tests inject a fixed or steerable
	// clock; nil uses time.Now.
	Now func() time.Time
}

// Coordinator is the UniFi → MQTT data-flow root.
type Coordinator struct {
	cfg    *config.Config
	site   model.Site
	src    Source
	sub    Subscriber
	caps   Capabilities
	info   model.ControllerInfo
	log    *slog.Logger
	now    func() time.Time
	topics topicBuilder
	pub    *publisher

	// mu guards the cross-loop caches below. The static loop writes them
	// and the fast loops read them, so a plain map would race.
	mu sync.RWMutex
	// details holds the per-device data only the detail endpoint carries
	// (ports, radios, uplink), refreshed by the static loop and merged
	// into every device publish.
	details map[model.MAC]model.Device
	// networks is the catalogue the client VLAN mapping resolves against
	// (phase 4).
	networks []model.Network
	// seen tracks which devices have been published, so one that
	// disappears can have its topics cleared instead of lingering.
	seen map[model.MAC]bool
	// announced maps a device to the discovery config topics it created,
	// which is what makes removing its entities possible when it
	// disappears or loses a port.
	announced map[model.MAC][]string
	// announcedClients is the same for clients, keyed by client key.
	announcedClients map[string][]string
	// clients holds presence state between polls: a client is only
	// declared away after AWAY_TIMEOUT, not on the first missed poll.
	clients map[string]clientState
	// deviceIDToMAC resolves the uplink UUIDs clients report, refreshed
	// by the static loop alongside the device details.
	deviceIDToMAC map[string]model.MAC

	// hass builds discovery payloads; nil when HASS_ENABLE is false.
	hass *hass.Discovery
	// rediscover carries Home Assistant birth messages from the MQTT
	// read loop to the goroutine that re-announces discovery. Buffered
	// with room for one: several births in a row need one re-announce.
	rediscover chan struct{}
	// healthAnnounced guards the one-shot site-health discovery, which
	// waits until the classic layer has actually answered.
	healthAnnounced atomic.Bool

	// commands carries parsed inbound commands from the MQTT read loop
	// to the goroutine that executes them, so the handler never blocks.
	commands chan command
	// nudgeDevices and nudgeClients let a completed command pull the
	// affected object's state forward, instead of waiting out the poll
	// interval.
	nudgeDevices chan struct{}
	nudgeClients chan struct{}
}

// New builds a Coordinator from deps.
func New(d Deps) *Coordinator {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}

	caps := d.Capabilities
	if caps == nil {
		caps = noCapabilities{}
	}

	topics := newTopicBuilder(d.Cfg.MQTTTopic, d.Site.Internal)
	c := &Coordinator{
		cfg:              d.Cfg,
		site:             d.Site,
		src:              d.Source,
		caps:             caps,
		info:             d.Info,
		sub:              d.Subscriber,
		log:              log,
		now:              now,
		topics:           topics,
		pub:              newPublisher(d.MQTT, d.Cfg.ForceRepublishDuration(), now, log),
		details:          make(map[model.MAC]model.Device),
		seen:             make(map[model.MAC]bool),
		announced:        make(map[model.MAC][]string),
		announcedClients: make(map[string][]string),
		clients:          make(map[string]clientState),
		deviceIDToMAC:    make(map[string]model.MAC),
		rediscover:       make(chan struct{}, 1),
		commands:         make(chan command, commandQueueSize),
		nudgeDevices:     make(chan struct{}, 1),
		nudgeClients:     make(chan struct{}, 1),
	}
	if d.Cfg.HASSEnable {
		c.hass = hass.New(c.DiscoveryConfig(d.Cfg.HASSBaseTopic, d.Cfg.Language))
	}
	return c
}

// SetPublisher swaps in the outbound publisher after construction.
//
// This exists because of a genuine ordering knot: the MQTT client needs
// the will topic, the will topic comes from the coordinator's topic
// builder, and the publisher we actually want is the circuit breaker
// wrapping that client. Constructing the coordinator first and handing
// it the breaker afterwards is the smallest way out. Call before Run.
func (c *Coordinator) SetPublisher(p Publisher) {
	c.pub.out = p
}

// SetSubscriber wires the inbound MQTT client. Like [SetPublisher] this
// happens after construction because the MQTT client needs the will
// topic the coordinator owns. Call before Run.
func (c *Coordinator) SetSubscriber(s Subscriber) { c.sub = s }

// AvailabilityTopic is the retained topic carrying the bridge's
// online/offline state. main wires it as the MQTT will and republishes
// "online" on every reconnect.
func (c *Coordinator) AvailabilityTopic() string { return c.topics.bridge(statusKey) }

// OnConnect is registered as the MQTT lifecycle's connect hook.
//
// It announces availability and drops the change-detection memory: a
// reconnect may have landed on a broker that lost its retained store,
// or on a different broker entirely, and suppressing every unchanged
// value against a stale memory would leave that broker permanently
// empty.
func (c *Coordinator) OnConnect(ctx context.Context) {
	c.pub.clear()
	if err := c.pub.publishRaw(ctx, c.AvailabilityTopic(), payloadOnline, mqtt.QoS1); err != nil {
		c.log.Warn("coordinator.availability_publish_failed", slog.String("err", err.Error()))
	}
	if err := c.publishBridgeInfo(ctx); err != nil {
		c.log.Warn("coordinator.bridge_info_failed", slog.String("err", err.Error()))
	}
}

// AnnounceOffline publishes the retained "offline" payload during a
// graceful shutdown. A clean MQTT DISCONNECT suppresses the broker-side
// will, so without this the availability topic would stay "online"
// after an orderly stop.
func (c *Coordinator) AnnounceOffline(ctx context.Context) {
	if err := c.pub.publishRaw(ctx, c.AvailabilityTopic(), payloadOffline, mqtt.QoS1); err != nil {
		c.log.Warn("coordinator.availability_publish_failed", slog.String("err", err.Error()))
	}
}

// Run starts the poll loops and blocks until ctx is cancelled or a loop
// fails fatally.
//
// The loops are deliberately independent: a console that stops
// answering the statistics endpoint must not stop device state from
// being published. Only an unrecoverable condition — an invalid API key
// — aborts, because retrying that forever would just hammer the console
// while publishing nothing.
func (c *Coordinator) Run(ctx context.Context) error {
	// Prime the caches before the fast loops start, so the first device
	// publish already carries ports, radios and uplinks rather than
	// briefly announcing a flat topology.
	if err := c.refreshStatic(ctx); err != nil {
		if fatal(err) {
			return err
		}
		c.log.Warn("coordinator.initial_static_failed", slog.String("err", err.Error()))
	}

	// Subscribing before the loops start means a Home Assistant that
	// restarts during the first poll is still caught.
	if err := c.watchHomeAssistant(ctx, c.sub); err != nil {
		c.log.Warn("coordinator.hass_status_subscribe_failed", slog.String("err", err.Error()))
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error { return c.rediscoverLoop(gctx) })
	if c.cfg.Controls.Enable {
		if err := c.subscribeCommands(ctx); err != nil {
			c.log.Warn("coordinator.command_subscribe_failed", slog.String("err", err.Error()))
		}
		g.Go(func() error { return c.commandLoop(gctx) })
	}
	g.Go(func() error {
		return c.loopWithNudge(gctx, "devices", c.cfg.RefreshDevicesDuration(),
			c.nudgeDevices, c.refreshDevices)
	})
	g.Go(func() error {
		return c.loop(gctx, "device_stats", c.cfg.RefreshDeviceStatsDuration(), c.refreshDeviceStats)
	})
	g.Go(func() error {
		return c.loop(gctx, "static", c.cfg.RefreshStaticDuration(), c.refreshStatic)
	})
	if c.cfg.Clients.Enable {
		g.Go(func() error {
			return c.loopWithNudge(gctx, "clients", c.cfg.RefreshClientsDuration(),
				c.nudgeClients, c.refreshClients)
		})
	}
	if c.cfg.ClassicEnable {
		g.Go(func() error {
			return c.loop(gctx, "health", c.cfg.RefreshHealthDuration(), c.refreshHealth)
		})
	}

	err := g.Wait()
	// A finished context is an orderly stop however it finished —
	// cancellation from a signal, or a deadline a caller set. Reporting
	// either as a failure would make systemd record a clean stop as a
	// unit failure.
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil
	}
	return err
}

// loop runs fn immediately and then on every tick.
//
// A non-fatal error is logged and the loop continues — a console
// rebooting after a firmware update must not take the daemon with it.
func (c *Coordinator) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context) error) error {
	return c.loopWithNudge(ctx, name, every, nil, fn)
}

// loopWithNudge is loop plus an out-of-band trigger.
//
// The nudge is what makes a command's effect visible immediately
// instead of up to a poll interval later: after restarting a device or
// blocking a client, waiting 60 seconds for the state to catch up makes
// the Home Assistant entity look broken (CONCEPT.md §9).
func (c *Coordinator) loopWithNudge(
	ctx context.Context,
	name string,
	every time.Duration,
	nudge <-chan struct{},
	fn func(context.Context) error,
) error {
	run := func() error {
		err := fn(ctx)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case fatal(err):
			c.log.Error("coordinator.loop_fatal",
				slog.String("loop", name), slog.String("err", err.Error()))
			return err
		default:
			c.log.Warn("coordinator.loop_error",
				slog.String("loop", name), slog.String("err", err.Error()))
			c.publishError(ctx, name, err)
			return nil
		}
	}

	if err := run(); err != nil {
		return err
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := run(); err != nil {
				return err
			}
		case <-nudge:
			// A command just changed something; re-read it now rather
			// than at the next tick.
			if err := run(); err != nil {
				return err
			}
		}
	}
}

// fatal reports whether an error means retrying is pointless. An
// invalid API key is the only such case today: it cannot fix itself,
// and a loop retrying it forever publishes nothing while producing an
// endless stream of failed requests.
func fatal(err error) bool {
	return errors.Is(err, unifi.ErrUnauthorized) || errors.Is(err, unifi.ErrForbidden)
}

// refreshStatic reloads the slowly-changing data: device details, the
// network catalogue and the WLAN catalogue.
func (c *Coordinator) refreshStatic(ctx context.Context) error {
	devices, err := c.src.DevicesWithDetails(ctx, c.site.ID)
	if err != nil {
		return fmt.Errorf("device details: %w", err)
	}
	networks, err := c.src.Networks(ctx, c.site.ID)
	if err != nil {
		return fmt.Errorf("networks: %w", err)
	}
	wlans, err := c.src.WLANs(ctx, c.site.ID)
	if err != nil {
		return fmt.Errorf("wlans: %w", err)
	}

	details := make(map[model.MAC]model.Device, len(devices))
	byID := make(map[string]model.MAC, len(devices))
	for i := range devices {
		if devices[i].MAC.IsZero() {
			continue
		}
		details[devices[i].MAC] = devices[i]
		if devices[i].ID != "" {
			byID[devices[i].ID] = devices[i].MAC
		}
	}

	c.mu.Lock()
	c.details = details
	c.networks = networks
	c.deviceIDToMAC = byID
	c.mu.Unlock()

	// Discovery is announced from here rather than the fast loop because
	// ports and radios decide how many entities a device has, and those
	// only arrive with the details.
	for i := range devices {
		if devices[i].MAC.IsZero() {
			continue
		}
		if err := c.publishDiscovery(ctx, &devices[i]); err != nil {
			c.log.Warn("coordinator.discovery_publish_failed",
				slog.String("device", devices[i].Name), slog.String("err", err.Error()))
		}
	}

	return c.publishWLANs(ctx, wlans)
}

// refreshDevices polls the device list and publishes the values it
// carries, merged with the cached details.
func (c *Coordinator) refreshDevices(ctx context.Context) error {
	devices, err := c.src.Devices(ctx, c.site.ID)
	if err != nil {
		return err
	}

	c.mu.RLock()
	details := c.details
	c.mu.RUnlock()

	present := make(map[model.MAC]bool, len(devices))
	for i := range devices {
		d := &devices[i]
		if d.MAC.IsZero() {
			// Without a MAC there is no stable topic to publish under.
			c.log.Debug("coordinator.device_without_mac", slog.String("name", d.Name))
			continue
		}
		present[d.MAC] = true

		// The list has no uplink, ports or radios; the static loop's
		// snapshot does.
		if det, ok := details[d.MAC]; ok {
			d.UplinkMAC = det.UplinkMAC
			d.UplinkID = det.UplinkID
			d.Ports = det.Ports
			d.Radios = det.Radios
			if d.AdoptedAt.IsZero() {
				d.AdoptedAt = det.AdoptedAt
			}
		}

		if err := c.publishDevice(ctx, d); err != nil {
			// One unpublishable device must not stop the rest.
			c.log.Warn("coordinator.device_publish_failed",
				slog.String("device", d.Name), slog.String("err", err.Error()))
		}
	}

	c.sweepDevices(ctx, present)
	return nil
}

// sweepDevices drops the change-detection memory for devices that are
// no longer reported, so one that returns republishes its full state
// instead of being suppressed against a stale memory.
//
// The retained topics themselves are left in place here; removing them
// is discovery's job in phase 3, where the config topic that created
// the entity is also cleared.
func (c *Coordinator) sweepDevices(ctx context.Context, present map[model.MAC]bool) {
	c.mu.Lock()
	var gone []model.MAC
	for mac := range c.seen {
		if !present[mac] {
			gone = append(gone, mac)
		}
	}
	for _, mac := range gone {
		delete(c.seen, mac)
	}
	for mac := range present {
		c.seen[mac] = true
	}
	c.mu.Unlock()

	for _, mac := range gone {
		dropped := c.pub.forget(c.topics.devicePrefix(mac))
		c.log.Info("coordinator.device_gone",
			slog.String("mac", mac.Colon()), slog.Int("topics", len(dropped)))
		// Removing the entities matters more than dropping the memory: a
		// device that was decommissioned would otherwise leave a set of
		// permanently unavailable entities in Home Assistant.
		c.forgetDiscovery(ctx, mac)
		c.mu.Lock()
		delete(c.details, mac)
		c.mu.Unlock()
	}
}

// refreshDeviceStats fetches per-device statistics with bounded
// concurrency, skipping devices that are not online.
func (c *Coordinator) refreshDeviceStats(ctx context.Context) error {
	devices, err := c.src.Devices(ctx, c.site.ID)
	if err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(statsConcurrency)

	for i := range devices {
		d := devices[i]
		// An offline device answers with an empty sample, so the call is
		// pure overhead — and publishing zeros would make Home Assistant
		// draw a CPU drop to 0 rather than a gap.
		if d.MAC.IsZero() || !d.State.IsOnline() {
			continue
		}

		g.Go(func() error {
			stats, err := c.src.DeviceStats(gctx, c.site.ID, d.ID)
			if err != nil {
				if fatal(err) {
					return err
				}
				c.log.Warn("coordinator.device_stats_failed",
					slog.String("device", d.Name), slog.String("err", err.Error()))
				return nil
			}
			if err := c.publishDeviceStats(gctx, d.MAC, &stats); err != nil {
				c.log.Warn("coordinator.device_stats_publish_failed",
					slog.String("device", d.Name), slog.String("err", err.Error()))
			}
			return nil
		})
	}
	return g.Wait()
}

// statsConcurrency bounds the per-device statistics fan-out, matching
// the console client's own limit.
const statsConcurrency = 4

// publishBridgeInfo publishes the daemon and console metadata.
//
// Deliberately carries no publish counters: this topic is written from
// the connect hook, before any value has gone out, so a counter here
// would permanently read 1 and mislead rather than inform.
func (c *Coordinator) publishBridgeInfo(ctx context.Context) error {
	payload, err := json.Marshal(struct {
		Site               string `json:"site"`
		SiteID             string `json:"site_id"`
		ApplicationVersion string `json:"application_version"`
		Host               string `json:"host"`
		Version            string `json:"bridge_version"`
	}{
		Site:               c.site.Internal,
		SiteID:             c.site.ID,
		ApplicationVersion: c.info.ApplicationVersion,
		Host:               c.cfg.Host,
		Version:            version.Version,
	})
	if err != nil {
		return err
	}
	return c.pub.publishRaw(ctx, c.topics.bridge(infoKey), string(payload), mqtt.QoS0)
}

// publishError surfaces a non-fatal loop failure on the bridge error
// topic. Not retained: a stale error message outliving the condition it
// described is worse than no message.
func (c *Coordinator) publishError(ctx context.Context, loop string, cause error) {
	payload, err := json.Marshal(struct {
		Loop string `json:"loop"`
		Err  string `json:"error"`
		At   string `json:"at"`
	}{Loop: loop, Err: cause.Error(), At: c.now().UTC().Format(time.RFC3339)})
	if err != nil {
		return
	}
	// Deliberately bypasses change detection: this topic is not retained
	// and repeated identical errors should still be visible.
	if c.pub.out == nil {
		return
	}
	if err := c.pub.out.Publish(ctx, c.topics.bridge(errorKey), payload, mqtt.QoS0, false); err != nil {
		c.log.Debug("coordinator.error_publish_failed", slog.String("err", err.Error()))
	}
}

// Networks returns the cached network catalogue. Phase 4's client
// filtering resolves VLANs against it.
func (c *Coordinator) Networks() []model.Network {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.networks
}

func itoa(n int) string { return strconv.Itoa(n) }
