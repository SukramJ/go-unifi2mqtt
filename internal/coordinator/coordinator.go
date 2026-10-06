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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/SukramJ/go-hamqtt/discovery"
	hapub "github.com/SukramJ/go-hamqtt/publisher"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
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
	// Store, when non-nil, receives a copy of everything polled so the
	// diagnostic web UI can render it. Nil keeps the daemon a pure MQTT
	// bridge with no second copy of the data.
	Store *state.Store
	// Logger receives diagnostics; nil uses slog.Default().
	Logger *slog.Logger

	// SetLogLevel applies `<name>/maintenance/set/loglevel` to the
	// daemon's own handler. Nil refuses the command with a warning.
	SetLogLevel func(slog.Level)
	// Supervised answers whether something restarts the process after a
	// clean exit; `<name>/maintenance/set/restart` is refused unless it
	// answers true. Nil refuses.
	Supervised func() bool
	// Shutdown starts the daemon's graceful shutdown — the path a signal
	// takes, which writes `<name>/connected` 0 and exits 0. It is what a
	// maintenance restart calls.
	Shutdown func()
	// Clock stamps the status objects' `ts`; nil is [time.Now]. Tests
	// pin it so the published surface is reproducible.
	Clock func() time.Time
}

// ProjectName is `<name>/info`'s `name`: the Go project, deliberately
// not `unifi2mqtt`, which is hobbyquaker's npm package — a tool reading
// `info` would offer that package's versions as an update for this
// daemon (openccu-loom ADR 0083).
const ProjectName = "go-unifi2mqtt"

// Coordinator is the UniFi → MQTT data-flow root.
type Coordinator struct {
	cfg    *config.Config
	site   model.Site
	src    Source
	sub    Subscriber
	caps   Capabilities
	info   model.ControllerInfo
	log    *slog.Logger
	topics topicBuilder
	pub    *publisher
	store  *state.Store

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
	// wlanIDs is every SSID id the WLAN catalogue reported in this run,
	// for the migration sweep's ownership test.
	wlanIDs map[string]bool
	// wlanByID is the latest catalogue record per SSID id, which the
	// one-time re-point of 1.x discovery configs renders an SSID switch
	// from (repoint.go).
	wlanByID map[string]model.WLAN
	// windowMu serialises the snapshot windows over the discovery
	// prefix. The orphan reconcile and the 1.x re-point both open one,
	// and two concurrent windows on the same filter would share one
	// broker-side subscription: the first UNSUBSCRIBE would end the
	// second window early.
	windowMu sync.Mutex

	// hass builds discovery payloads; nil when HASS_ENABLE is false.
	hass *hass.Discovery
	// rediscover carries Home Assistant birth messages from the MQTT
	// read loop to the goroutine that re-announces discovery. Buffered
	// with room for one: several births in a row need one re-announce.
	rediscover chan struct{}
	// healthAnnounced records that the site-health entities have been
	// announced at least once in this process, which is the orphan
	// sweep's ClassSite readiness signal. It only ever moves forward —
	// the announce latch that a reconnect re-opens is
	// [Coordinator.healthDiscovered].
	healthAnnounced atomic.Bool

	// readyDevices, readyStatic and readyClients record that a loop has
	// completed one cycle that actually produced data. The orphan
	// reconcile gates on them: an empty announced set means "not polled
	// yet" until the corresponding source says otherwise, and sweeping
	// before that would delete every entity the daemon owns.
	readyDevices atomic.Bool
	readyStatic  atomic.Bool
	readyClients atomic.Bool
	// reconcileTimeout and reconcileWindow are the sweep's two waits.
	reconcileTimeout time.Duration
	reconcileWindow  time.Duration

	// commands carries parsed inbound commands from the MQTT read loop
	// to the goroutine that executes them, so the handler never blocks.
	commands chan command
	// nudgeDevices and nudgeClients let a completed command pull the
	// affected object's state forward, instead of waiting out the poll
	// interval.
	nudgeDevices chan struct{}
	nudgeClients chan struct{}
	// nudgeStatic serves the two controls whose state the *static* loop
	// owns: the locate LED and the SSID toggle. Nudging the device loop
	// for those republishes a snapshot the device loop never refreshed,
	// which looks like a working nudge and is not one.
	nudgeStatic chan struct{}

	// --- the go-hamqtt planes (ADR 0070 phase 9, step 5) ---

	// direct is the publisher the bridge availability marker goes out
	// through, deliberately not the circuit breaker the rest of the
	// traffic rides. Nil means "the same one as everything else", which
	// is what every test wants and what a daemon without a breaker
	// would have. See [planePublisher.Publish].
	direct Publisher
	// newRuntime rebuilds the discovery runtime. It is a factory rather
	// than an instance because everything a [hapub.Runtime] remembers —
	// what it superseded, declared and announced — is a statement about
	// one *broker connection*, while the object would otherwise live for
	// the process. A QoS 0 retraction to a dying socket returns nil (that
	// only means Write+Flush returned), the memo records it done, and an
	// in-process retry after the reconnect re-sends zero retractions and
	// publishes anyway — which at step 6 is one device bundle landing in
	// a tree still holding every per-entity config, refused by Home
	// Assistant with a single `WARNING [mqtt.entity] Received a
	// conflicting MQTT discovery message` and no entities. go-mtec2mqtt
	// shipped that shape. Rebuilding here makes it unavailable to the
	// step that would pay for it.
	//
	// The sweep's ownership evidence deliberately does NOT live in the
	// runtime — it is [publisher.published], which survives the swap —
	// so the rebuild cannot weaken the claim gate. See reconcile.go.
	newRuntime func() *hapub.Runtime
	// haRuntime is the current one. Never store the result of ha()
	// across a call that can block on the broker: a reconnect swaps it
	// underneath, and acting on the old one is the defect the swap
	// exists to remove.
	haRuntime atomic.Pointer[hapub.Runtime]
	// router subscribes the command tree and the maintenance commands.
	// Nil until Run wires it.
	router *hapub.CommandRouter
	// instance publishes `<name>/info` and serves the maintenance topics
	// (mqtt-smarthome 2.0 §6 and §7).
	instance *hapub.Instance
	// upstream is the `<name>/connected` level this daemon last decided:
	// 2 while the console answers the device poll, 1 while it does not.
	// It lives here rather than only in the runtime because the runtime
	// is rebuilt on every reconnect and starts at 1 again; see
	// [Coordinator.announceConnected].
	upstream atomic.Int32
	// healthDiscovered is the one-shot latch for the site-health configs,
	// and it is deliberately NOT healthAnnounced: that one is the sweep's
	// readiness signal and must only ever move forward, while this one is
	// cleared on every (re)connect so a broker that came back without its
	// retained store gets the health configs again.
	healthDiscovered atomic.Bool
	// clientsDiscovered records which clients' configs have been
	// announced, for the same reason and with the same reset.
	clientsDiscovered map[string]bool
}

// New builds a Coordinator from deps.
func New(d Deps) *Coordinator {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}

	caps := d.Capabilities
	if caps == nil {
		caps = noCapabilities{}
	}

	topics := newTopicBuilder(d.Cfg.MQTTTopic, d.Site.Internal)
	c := &Coordinator{
		cfg:               d.Cfg,
		site:              d.Site,
		src:               d.Source,
		caps:              caps,
		store:             d.Store,
		info:              d.Info,
		sub:               d.Subscriber,
		log:               log,
		topics:            topics,
		pub:               newPublisher(d.MQTT, commandFilters(topics), d.Clock, log),
		details:           make(map[model.MAC]model.Device),
		seen:              make(map[model.MAC]bool),
		announced:         make(map[model.MAC][]string),
		announcedClients:  make(map[string][]string),
		clients:           make(map[string]clientState),
		deviceIDToMAC:     make(map[string]model.MAC),
		wlanIDs:           make(map[string]bool),
		wlanByID:          make(map[string]model.WLAN),
		rediscover:        make(chan struct{}, 1),
		commands:          make(chan command, commandQueueSize),
		nudgeDevices:      make(chan struct{}, 1),
		nudgeClients:      make(chan struct{}, 1),
		nudgeStatic:       make(chan struct{}, 1),
		reconcileTimeout:  defaultReconcileTimeout,
		reconcileWindow:   defaultReconcileWindow,
		clientsDiscovered: make(map[string]bool),
	}
	if d.Cfg.HASSEnable {
		c.hass = hass.New(c.DiscoveryConfig(d.Cfg.HASSBaseTopic, d.Cfg.Language))
	}

	// The planes are built here, before any MQTT client exists, because
	// the Last Will is part of CONNECT and the will is the runtime's own
	// statement: main reads [Coordinator.Will] to build the client it
	// will later hand back through SetPublisher/SetSubscriber. The
	// transport resolves both of those per call, so nothing is captured
	// before it is wired.
	tr := c.planeTransport()
	c.newRuntime = func() *hapub.Runtime { return hapub.New(tr, c.RuntimeConfig(log)) }
	c.haRuntime.Store(c.newRuntime())
	// The runtime starts every connection at 1, and so does this: the
	// console was reachable a moment ago, at startup, but nothing claims
	// it is operational until the first device poll says so.
	c.upstream.Store(discovery.ConnectedBroker)

	c.instance = hapub.NewInstance(tr, hapub.InstanceConfig{
		Layout:  hass.NewLayout(c),
		Name:    ProjectName,
		Version: version.Version,
		// What `bridge/info` carried before 2.0, as project fields. The
		// console's host is `console_host` because `host` is the spec's
		// own field, this daemon's host name.
		Extra: map[string]any{
			"site":                d.Site.Internal,
			"site_id":             d.Site.ID,
			"application_version": d.Info.ApplicationVersion,
			"console_host":        d.Cfg.Host,
		},
		MaintenanceDisabled: !d.Cfg.MQTTMaintenance,
		SetLogLevel:         d.SetLogLevel,
		Supervised:          d.Supervised,
		Shutdown:            d.Shutdown,
		StatsInterval:       hapub.StatsInterval(d.Cfg.MQTTStatsInterval),
		Logger:              log,
	})
	return c
}

// ha is the current discovery runtime. See [Coordinator.newRuntime] for
// why it may not be stored across a broker call.
func (c *Coordinator) ha() *hapub.Runtime { return c.haRuntime.Load() }

// Will is the Last Will the MQTT client must be configured with for
// this daemon's availability policy to mean anything.
//
// Returned as the runtime's own value rather than composed here: the
// will topic, the birth, the death and the `availability` list of all
// 315 discovery configs are then one string by construction. Two
// sibling bridges in this programme configure a will whose topic no
// published entity references, so the broker dutifully writes "offline"
// on a crash and every entity stays available forever, showing the last
// value it ever saw.
func (c *Coordinator) Will() (hapub.Will, error) { return c.ha().Will() }

// SetDirectPublisher names the publisher the bridge availability marker
// goes out through, around whatever decoration the ordinary one
// carries. Call before Run; see [planePublisher.Publish] for why.
func (c *Coordinator) SetDirectPublisher(p Publisher) { c.direct = p }

// Close releases the current runtime's replay worker.
func (c *Coordinator) Close() {
	if rt := c.ha(); rt != nil {
		rt.Close()
	}
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

// AvailabilityTopic is the retained `<name>/connected` topic carrying
// 0, 1 or 2. main wires it as the MQTT will, whose payload is 0.
func (c *Coordinator) AvailabilityTopic() string { return c.topics.connected() }

// OnConnect is registered as the MQTT lifecycle's connect hook.
//
// It announces `<name>/connected` and `<name>/info` and replays every
// status item (mqtt-smarthome 2.0 §3.1, §3.2 and §6): a reconnect may
// have landed on a broker that lost its retained store, or on a
// different broker entirely, and a value that has not changed since
// would otherwise never reach it.
func (c *Coordinator) OnConnect(ctx context.Context) {
	c.resetPlanes()
	if c.store != nil {
		c.store.SetMQTTConnected(true)
	}
	if err := c.announceConnected(ctx); err != nil {
		c.log.Warn("coordinator.availability_publish_failed", slog.String("err", err.Error()))
	}
	if err := c.instance.AnnounceInfo(ctx); err != nil {
		c.log.Warn("coordinator.info_publish_failed", slog.String("err", err.Error()))
	}
	if n, err := c.pub.republish(ctx); err != nil {
		c.log.Warn("coordinator.status_republish_failed",
			slog.Int("sent", n), slog.String("err", err.Error()))
	}
	c.rediscoverOnReconnect()
}

// announceConnected publishes the current `<name>/connected` level on a
// fresh connection.
//
// The runtime was just rebuilt and starts at 1, so a level of 2 is
// handed to it through [hapub.Runtime.SetConnected], which publishes it
// as the transition it is for that runtime; 1 is
// [hapub.Runtime.AnnounceOnline]'s to publish.
func (c *Coordinator) announceConnected(ctx context.Context) error {
	rt := c.ha()
	if int(c.upstream.Load()) == discovery.ConnectedOperational {
		_, err := rt.SetConnected(ctx, discovery.ConnectedOperational)
		return err
	}
	return rt.AnnounceOnline(ctx)
}

// setUpstream moves `<name>/connected` between 1 (broker reachable,
// console not) and 2 (operational) — spec §3.1, for a bridge of many
// devices: 2 while the bridge's own upstream answers, the reachability
// of a single device being that device's `online` item.
//
// The upstream is the console's device list, the one read this daemon
// cannot do without and makes every minute; [Coordinator.refreshDevices]
// reports each outcome. A repeat of the current level publishes
// nothing.
func (c *Coordinator) setUpstream(ctx context.Context, up bool) {
	level := discovery.ConnectedBroker
	if up {
		level = discovery.ConnectedOperational
	}
	if int(c.upstream.Swap(int32(level))) == level {
		return
	}
	if !up {
		c.log.Warn("coordinator.console_unreachable",
			slog.String("effect", "connected 1: entities unavailable until the console answers again"))
	} else {
		c.log.Info("coordinator.console_reachable")
	}
	if _, err := c.ha().SetConnected(ctx, level); err != nil {
		c.log.Warn("coordinator.availability_publish_failed", slog.String("err", err.Error()))
	}
}

// resetPlanes puts every per-connection memo back to what a fresh
// process would have.
//
// The discovery configs' dedup gate opens ([publisher.clear]) and the
// discovery runtime is rebuilt rather than reset, for the reason written
// on [Coordinator.newRuntime]. The state plane keeps its memory, which
// [publisher.republish] replays.
// What is deliberately *not* touched is the claim list the sweep reads:
// it is a statement about this process, not about this connection, and
// clearing it would turn a reconnect into "this daemon published
// nothing" — which is the one input that would make the sweep dangerous.
func (c *Coordinator) resetPlanes() {
	c.pub.clear()
	if c.newRuntime == nil {
		return
	}
	fresh := c.newRuntime()
	if fresh == nil {
		c.log.Error("coordinator.ha_runtime_reset_failed",
			slog.String("hint", "the runtime factory returned nil; "+
				"the discovery plane keeps the previous connection's memo"))
		return
	}
	if old := c.haRuntime.Swap(fresh); old != nil {
		old.Close()
	}
}

// rediscoverOnReconnect re-opens the one-shot discovery latches and
// pulls the static loop forward.
//
// Opening the dedup gate is only half of a reconnect, and the half that
// does nothing on its own. A broker that came back without its retained
// store — a restart without persistence, a failover to a fresh node —
// holds no discovery configs, and this daemon announces three of its
// four classes *once*: client configs on a client's first sighting and
// the site-health configs on the classic layer's first answer, neither
// of which happens again while the daemon runs. Those entities would
// never come back. The device and SSID configs would, but only on the
// static loop's own hour-long cadence, which is why this also nudges it.
//
// go-homeconnect2mqtt measured exactly this at its own step 5: the gate
// was open on the new connection and nothing walked through it, because
// no hook fired on a *broker* reconnect.
func (c *Coordinator) rediscoverOnReconnect() {
	c.healthDiscovered.Store(false)
	c.mu.Lock()
	clear(c.clientsDiscovered)
	c.mu.Unlock()
	select {
	case c.nudgeStatic <- struct{}{}:
	default: // one pending refresh is enough
	}
}

// AnnounceOffline publishes the retained `<name>/connected` 0 during a
// graceful shutdown. A clean MQTT DISCONNECT suppresses the broker-side
// will, so without this the topic would keep saying 2 after an orderly
// stop.
func (c *Coordinator) AnnounceOffline(ctx context.Context) {
	if err := c.ha().AnnounceOffline(ctx); err != nil {
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
	c.primeStore()

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
	g.Go(func() error { return c.reconcileOrphans(gctx) })
	g.Go(func() error { return c.migrateOldLayout(gctx) })
	// The maintenance commands ride the same router as the controls, so
	// it is subscribed whether or not CONTROLS.ENABLE is on.
	if err := c.subscribeCommands(ctx); err != nil {
		c.log.Warn("coordinator.command_subscribe_failed", slog.String("err", err.Error()))
	}
	if c.cfg.Controls.Enable {
		g.Go(func() error { return c.commandLoop(gctx) })
	}
	g.Go(func() error {
		// Never fatal: ErrStatsOff when the stats are switched off, the
		// context's error on shutdown.
		_ = c.instance.RunStats(gctx)
		return nil
	})
	g.Go(func() error {
		return c.loopWithNudge(gctx, "devices", c.cfg.RefreshDevicesDuration(),
			c.nudgeDevices, c.refreshDevices)
	})
	g.Go(func() error {
		return c.loop(gctx, "device_stats", c.cfg.RefreshDeviceStatsDuration(), c.refreshDeviceStats)
	})
	g.Go(func() error {
		return c.loopWithNudge(gctx, "static", c.cfg.RefreshStaticDuration(),
			c.nudgeStatic, c.refreshStatic)
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

// primeStore fills in the metadata the UI shows before the first poll
// completes, so an operator opening the page during startup sees the
// site and capabilities rather than an empty shell.
func (c *Coordinator) primeStore() {
	if c.store == nil {
		return
	}
	c.store.SetSite(c.site, c.info)
	for _, cap := range []unifi.Capability{
		unifi.CapHealth, unifi.CapClientDetails, unifi.CapPortPower,
		unifi.CapClientBlock, unifi.CapDeviceLocate, unifi.CapWLANToggle,
	} {
		c.store.SetCapability(string(cap), c.caps.Has(cap))
	}
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
			if c.store != nil {
				c.store.PollSucceeded(name, time.Now())
			}
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
			if c.store != nil {
				c.store.PollFailed(name, err, time.Now())
			}
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
	for i := range wlans {
		c.wlanIDs[wlans[i].ID] = true
		c.wlanByID[wlans[i].ID] = wlans[i]
	}
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
		// The locate LED reads back from the same response, and this is
		// the only loop that has it.
		if err := c.publishLocate(ctx, &devices[i]); err != nil {
			c.log.Warn("coordinator.locate_publish_failed",
				slog.String("device", devices[i].Name), slog.String("err", err.Error()))
		}
	}

	if c.store != nil {
		c.store.SetWLANs(wlans)
	}
	if err := c.publishWLANs(ctx, wlans); err != nil {
		return err
	}
	// The WLAN catalogue is authoritative once this returns, so the
	// reconcile may sweep SSID switches from here on.
	c.readyStatic.Store(true)
	return nil
}

// refreshDevices polls the device list and publishes the values it
// carries, merged with the cached details.
func (c *Coordinator) refreshDevices(ctx context.Context) error {
	devices, err := c.src.Devices(ctx, c.site.ID)
	if err != nil {
		// A shutdown cancelling the call says nothing about the console,
		// and the 0 written on the way out must not be preceded by a 1.
		if ctx.Err() == nil {
			c.setUpstream(ctx, false)
		}
		return err
	}
	c.setUpstream(ctx, true)

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
	if c.store != nil {
		c.store.SetDevices(devices)
	}
	// Only a cycle that saw devices counts as ready. A site really can
	// be empty, but a console answering with an empty list is far more
	// often a permission or filter problem — and treating that as "we
	// announce nothing" would let the reconcile clear every device
	// entity on the broker.
	if len(present) > 0 {
		c.readyDevices.Store(true)
	}
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
			if c.store != nil {
				c.store.SetDeviceStats(d.MAC, stats, time.Now())
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

// publishError surfaces a non-fatal loop failure as the status item
// `<name>/status/bridge/error`: a status object whose `val` is the loop
// and the error, not retained — a stale error message outliving the
// condition it described is worse than no message, and spec §3.2 says
// an event must not be retained. No change detection either: repeated
// identical errors should still be visible.
func (c *Coordinator) publishError(ctx context.Context, loop string, cause error) {
	value := struct {
		Loop string `json:"loop"`
		Err  string `json:"error"`
	}{Loop: loop, Err: cause.Error()}
	if err := c.pub.pulse(ctx, c.topics.bridgeError(), value); err != nil {
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
