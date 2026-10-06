// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hatopic "github.com/SukramJ/go-hamqtt/topic"
	mqtt "github.com/SukramJ/go-mqtt"
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
// An object this run has not seen (a client that stays away, a device
// unplugged while the daemon was stopped) keeps its old retained topics
// until a later start sees it: stale, never wrongly deleted.

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

// migrateOldLayout runs the sweep once, after the first poll cycles
// have said which devices, clients and SSIDs this instance owns — the
// same readiness the orphan reconcile waits for, bounded by the same
// timeout.
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

	cleared, err := c.sweepOldLayout(ctx, c.ownedIdentifiers())
	switch {
	case ctx.Err() != nil:
		// Shutting down inside the window; the next start sweeps again.
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

// sweepOldLayout subscribes the 1.x trees this instance owns for one
// window, collects the retained topics [oldLayoutTopic] accepts,
// unsubscribes, and clears each with an empty retained payload. It
// returns how many it cleared.
func (c *Coordinator) sweepOldLayout(ctx context.Context, ids ownedIDs) (int, error) {
	root, site := c.topics.name(), c.topics.site
	filters := []string{root + "/" + site + "/#", root + "/" + bridgeSegment + "/+"}

	var (
		mu     sync.Mutex
		found  []string
		seen   = map[string]bool{}
		closed atomic.Bool
	)
	collect := func(msg *mqtt.Message) {
		// Runs inline in the MQTT read loop: a check and a map write,
		// nothing that publishes. An empty payload is a topic already
		// cleared, and a live message is not the old layout's leftover.
		if closed.Load() || !msg.Retain || len(msg.Payload) == 0 ||
			!oldLayoutTopic(root, site, ids, msg.Topic) {
			return
		}
		mu.Lock()
		if !seen[msg.Topic] {
			seen[msg.Topic] = true
			found = append(found, msg.Topic)
		}
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
			return 0, err
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
		return 0, err
	}

	mu.Lock()
	targets := append([]string(nil), found...)
	mu.Unlock()
	if len(targets) == 0 {
		return 0, nil
	}
	// Through the state plane's eviction: an empty retained payload at
	// the state QoS, refused for anything inside this daemon's own
	// command subscriptions — which no 1.x topic can be.
	if err := c.pub.state.Evict(ctx, targets...); err != nil {
		return 0, err
	}
	return len(targets), nil
}
