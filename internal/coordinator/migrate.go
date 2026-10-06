// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hatopic "github.com/SukramJ/go-hamqtt/topic"
	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
)

// The retained sweep of openccu-loom ADR 0083 ("Migration").
//
// 2.0.0 moved every topic from `<root>/<site>/…` to
// `<name>/status/<site>/…` and `<name>/set/<site>/…`, and replaced
// `<root>/bridge/status` and `<root>/bridge/info` with `<name>/connected`
// and `<name>/info`. The old layout's retained values would otherwise
// stand on the broker forever. This sweep clears them on every start for
// the life of the 2.x line — it is idempotent, and it also cleans up
// after a rollback and re-upgrade — and goes with the next breaking
// release.
//
// The old root equals the new name: both are MQTT_TOPIC, sanitised to
// one segment the same way. What keeps the sweep from clearing anything
// it does not own:
//
//   - It reads `<name>/<site>/#` and `<name>/bridge/+` only — this
//     instance's site and its own bridge topics. Another instance's name
//     and another site are never subscribed, so never seen.
//   - A topic whose second level is a function name (`status`, `set`, …)
//     is new by definition and never touched. [CheckTopics] refuses a
//     site spelled like a function, which is what makes the test sound:
//     no 1.x topic can have a function name on its second level.
//   - It clears only retained messages, and only the exact 1.x shapes
//     ([oldLayoutTopic]) — never a prefix match. Below a device, client
//     or WLAN that means the identifier must be one this instance has
//     polled from its own console in this run, and the item one the 1.x
//     layout published there. A second console bridged under the same
//     name and site — which 1.x already could not tell apart — keeps
//     every device, client and SSID this one does not know.
//
// An object this run has not polled keeps its old retained topics unless
// the re-point of its 1.x discovery configs (repoint.go) settled it: an
// absent client re-pointed to the new layout, a device or SSID the
// console no longer has retracted. Everything else stays: stale, never
// wrongly deleted.

// oldDeviceItems are the 1.x items below `<root>/<site>/device/<mac>/`,
// apart from the port and radio sub-trees.
var oldDeviceItems = map[string]bool{
	keyState: true, keyUptime: true, keyCPUUtilization: true, keyMemoryUtilization: true,
	keyUplinkTxBps: true, keyUplinkRxBps: true, keyFirmware: true, keyUpdateAvailable: true,
	keyAttributes: true, keyLocate: true,
	// The 1.x command topics. Nothing in this daemon ever retained one,
	// but a `mosquitto_pub -r` an operator once sent is exactly the
	// stale replay a clean break should not leave behind.
	"cmd/restart": true, "cmd/locate/set": true,
}

// oldPortItems are the 1.x items below `…/device/<mac>/port/<idx>/`.
var oldPortItems = map[string]bool{
	keyPortState: true, keyPortSpeed: true, keyPortPoE: true, keyPortPoEPower: true,
	"cmd/power_cycle": true,
}

// oldRadioItems are the 1.x items below `…/device/<mac>/radio/<band>/`.
var oldRadioItems = map[string]bool{keyRadioChannel: true, keyRadioTxRetries: true}

// oldClientItems are the 1.x items below `<root>/<site>/client/<key>/`.
var oldClientItems = map[string]bool{
	keyState: true, keyClientIP: true, keyClientSignal: true, keyClientBlocked: true,
	keyAttributes: true, "blocked/set": true, "cmd/authorize": true,
}

// oldWLANItems are the 1.x items below `<root>/<site>/wlan/<id>/`.
var oldWLANItems = map[string]bool{keyWLANEnabled: true, keyWLANName: true, "enabled/set": true}

// oldHealthItems are the 1.x items below `<root>/<site>/health/`. The
// site plane has no identifier of its own: the site is this instance's.
var oldHealthItems = map[string]bool{
	keyWANState: true, keyWANIP: true, keyWANLatency: true, keyWANRx: true, keyWANTx: true,
	keyLANState: true, keyWLANState: true, keyVPNState: true,
	keyClientsTotal: true, keyClientsGuest: true, keyAttributes: true,
}

// oldBridgeItems are the retained 1.x bridge topics. `bridge/error` was
// never retained, so there is nothing of it to clear.
var oldBridgeItems = map[string]bool{"status": true, "info": true}

// ownedIDs are the identifiers this instance has polled from its own
// console, as the 1.x layout spelled them in a topic.
type ownedIDs struct {
	devices, clients, wlans map[string]bool
}

// oldLayoutTopic reports whether t is a retained topic of the 1.x layout
// that this instance owns, under root and site:
//
//	<root>/bridge/{status,info}
//	<root>/<site>/device/<known mac>/<1.x device item>
//	<root>/<site>/client/<known key>/<1.x client item>
//	<root>/<site>/wlan/<known id>/<1.x wlan item>
//	<root>/<site>/health/<1.x health item>
func oldLayoutTopic(root, site string, ids ownedIDs, t string) bool {
	levels := strings.Split(t, "/")
	if len(levels) < 3 || levels[0] != root || hatopic.IsFunction(levels[1]) {
		return false
	}
	if levels[1] == bridgeSegment {
		return len(levels) == 3 && oldBridgeItems[levels[2]]
	}
	if levels[1] != site || hatopic.IsFunction(site) {
		return false
	}
	kind, rest := levels[2], levels[3:]
	if kind == "health" {
		return oldHealthItems[strings.Join(rest, "/")]
	}
	if len(rest) < 2 {
		return false
	}
	id, item := rest[0], rest[1:]
	switch kind {
	case "device":
		return ids.devices[id] && oldDeviceItem(item)
	case "client":
		return ids.clients[id] && oldClientItems[strings.Join(item, "/")]
	case "wlan":
		return ids.wlans[id] && oldWLANItems[strings.Join(item, "/")]
	default:
		return false
	}
}

// oldDeviceItem reports whether item is a 1.x item below a device.
func oldDeviceItem(item []string) bool {
	if oldDeviceItems[strings.Join(item, "/")] {
		return true
	}
	if len(item) < 3 || item[1] == "" {
		return false
	}
	switch item[0] {
	case "port":
		return isDigits(item[1]) && oldPortItems[strings.Join(item[2:], "/")]
	case "radio":
		return len(item) == 3 && oldRadioItems[item[2]]
	default:
		return false
	}
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// ownedIdentifiers collects what this run has polled: every device of
// the detail snapshot and the device list, every client seen, every
// SSID of the catalogue.
func (c *Coordinator) ownedIdentifiers() ownedIDs {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := ownedIDs{
		devices: make(map[string]bool, len(c.details)+len(c.seen)),
		clients: make(map[string]bool, len(c.clients)),
		wlans:   make(map[string]bool, len(c.wlanIDs)),
	}
	for mac := range c.details {
		ids.devices[mac.String()] = true
	}
	for mac := range c.seen {
		ids.devices[mac.String()] = true
	}
	for key := range c.clients {
		ids.clients[sanitiseSegment(key)] = true
	}
	for id := range c.wlanIDs {
		ids.wlans[sanitiseSegment(id)] = true
	}
	return ids
}

// migrateOldLayout runs the migration once the first poll cycles have
// said which devices, clients and SSIDs this instance owns — the same
// readiness the orphan reconcile waits for, bounded by the same timeout
// for the first round.
//
// A round re-points or retracts the 1.x discovery configs of every class
// whose source has reported ([Coordinator.repointLegacyConfigs]) and
// then sweeps the 1.x status items. A class that has not reported by the
// first round — the classic layer still failing, say — is settled in a
// later round, as soon as it does: its 1.x configs are the ones Home
// Assistant shows as unavailable until then.
//
// Like the reconcile it returns nil for anything but cancellation: a
// bridge that publishes correctly but cannot tidy up the old layout is
// degraded, not broken.
func (c *Coordinator) migrateOldLayout(ctx context.Context) error {
	if c.sub == nil {
		return nil
	}
	if _, _ = c.awaitReady(ctx); ctx.Err() != nil {
		return ctx.Err()
	}

	done := map[hass.Class]bool{}
	tick := time.NewTicker(reconcileReadyPoll)
	defer tick.Stop()
	for first := true; ; first = false {
		todo := map[hass.Class]bool{}
		if c.hass != nil {
			for cl := range c.readyClasses() {
				if !done[cl] {
					todo[cl] = true
				}
			}
		}
		if first || len(todo) > 0 {
			if err := c.migrationRound(ctx, todo); err != nil {
				return err
			}
			maps.Copy(done, todo)
		}
		if c.hass == nil || allReady(done) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// migrationRound reads the 1.x status items and, for classes, the
// retained discovery configs in one window each, settles the configs,
// and evicts the 1.x status items this instance owns. Only cancellation
// is returned.
func (c *Coordinator) migrationRound(ctx context.Context, classes map[hass.Class]bool) error {
	var (
		configs map[string][]byte
		cfgErr  error
		wg      sync.WaitGroup
	)
	if len(classes) > 0 {
		wg.Go(func() { configs, cfgErr = c.collectRetainedConfigs(ctx) })
	}
	old, oldErr := c.collectOldLayout(ctx)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		// Shutting down inside the window; the next start migrates again.
		return err
	}

	extra := ownedIDs{}
	switch {
	case len(classes) == 0:
	case cfgErr != nil:
		c.log.Warn("coordinator.migration_repoint_failed", slog.String("err", cfgErr.Error()))
	default:
		extra = c.repointLegacyConfigs(ctx, configs, old, classes).ids
	}

	if oldErr != nil {
		c.log.Warn("coordinator.migration_sweep_failed", slog.String("err", oldErr.Error()))
		return nil
	}
	ids := c.ownedIdentifiers()
	for _, pair := range []struct{ dst, src map[string]bool }{
		{ids.devices, extra.devices}, {ids.clients, extra.clients}, {ids.wlans, extra.wlans},
	} {
		maps.Copy(pair.dst, pair.src)
	}
	cleared, err := c.evictOldLayout(ctx, ids, old)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		c.log.Warn("coordinator.migration_sweep_failed", slog.String("err", err.Error()))
	case cleared > 0:
		c.log.Info("coordinator.migration_sweep",
			slog.Int("cleared", cleared),
			slog.String("note", "retained topics of the 1.x layout removed"))
	}
	return nil
}

// sweepOldLayout is one collect-and-evict pass over the 1.x status items
// with a fixed set of owned identifiers. It returns how many it cleared.
func (c *Coordinator) sweepOldLayout(ctx context.Context, ids ownedIDs) (int, error) {
	old, err := c.collectOldLayout(ctx)
	if err != nil {
		return 0, err
	}
	return c.evictOldLayout(ctx, ids, old)
}

// collectOldLayout subscribes the 1.x trees this instance owns for one
// window — `<name>/<site>/#` and `<name>/bridge/+`, never another name or
// site — collects every retained, non-empty message, and unsubscribes.
// What it returns is read, not judged: [Coordinator.evictOldLayout]
// decides what is this instance's to clear, and the re-point reads an
// absent client's last values out of it.
func (c *Coordinator) collectOldLayout(ctx context.Context) (map[string][]byte, error) {
	root, site := c.topics.name(), c.topics.site
	filters := []string{root + "/" + site + "/#", root + "/" + bridgeSegment + "/+"}

	var (
		mu     sync.Mutex
		found  = map[string][]byte{}
		closed atomic.Bool
	)
	collect := func(msg *mqtt.Message) {
		// Runs inline in the MQTT read loop: a check, a copy and a map
		// write, nothing that publishes. An empty payload is a topic
		// already cleared, and a live message is not the old layout's
		// leftover.
		if closed.Load() || !msg.Retain || len(msg.Payload) == 0 {
			return
		}
		payload := append([]byte(nil), msg.Payload...)
		mu.Lock()
		found[msg.Topic] = payload
		mu.Unlock()
	}

	subscribed := make([]string, 0, len(filters))
	defer func() {
		// Its own context: the caller's may be cancelled, which is
		// exactly when leaving a wildcard subscription installed for the
		// process lifetime would do the most harm.
		u, ok := c.sub.(unsubscriber)
		if !ok {
			return
		}
		teardown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, f := range subscribed {
			if err := u.Unsubscribe(teardown, f); err != nil {
				c.log.Warn("coordinator.migration_unsubscribe_failed",
					slog.String("filter", f), slog.String("err", err.Error()))
			}
		}
	}()
	for _, f := range filters {
		if _, err := c.sub.Subscribe(ctx, f, mqtt.QoS0, collect); err != nil {
			return nil, err
		}
		subscribed = append(subscribed, f)
	}

	timer := time.NewTimer(c.reconcileWindow)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
	}
	closed.Store(true)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(found), nil
}

// evictOldLayout clears every topic of old that [oldLayoutTopic] accepts
// for ids, with an empty retained payload, and returns how many.
func (c *Coordinator) evictOldLayout(ctx context.Context, ids ownedIDs, old map[string][]byte) (int, error) {
	root, site := c.topics.name(), c.topics.site
	var targets []string
	for topic := range old {
		if oldLayoutTopic(root, site, ids, topic) {
			targets = append(targets, topic)
		}
	}
	if len(targets) == 0 {
		return 0, nil
	}
	slices.Sort(targets)
	// Through the state plane's eviction: an empty retained payload at
	// the state QoS, refused for anything inside this daemon's own
	// command subscriptions — which no 1.x topic can be.
	if err := c.pub.state.Evict(ctx, targets...); err != nil {
		return 0, err
	}
	return len(targets), nil
}
