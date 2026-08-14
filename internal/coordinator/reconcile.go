// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
)

// Reconciling the broker's retained discovery configs on start.
//
// [Coordinator.removeStale] already clears the entities a device loses
// while the daemon runs. What it cannot see is a config written by an
// earlier run: a version that named an entity differently, a filter
// that used to match more clients, a device unplugged while the daemon
// was stopped. Those configs are retained, so Home Assistant recreates
// the entity on every start and it sits unavailable forever with
// nothing to explain it.
//
// This sweep reads what is actually retained under the discovery prefix
// and clears what this daemon owns but no longer announces. Doing that
// safely is entirely a question of when — see [Coordinator.readyClasses].

// reconcileReadyPoll is how often readiness is re-checked while the
// first poll cycles complete.
const reconcileReadyPoll = 2 * time.Second

// Defaults for the two waits the sweep depends on. They are Coordinator
// fields rather than constants so a test can drive the whole path
// without sitting out three minutes of it.
const (
	// defaultReconcileTimeout bounds the wait for every source to
	// report. One that never succeeds — classic health against a console
	// that rejects the login — must not block the sweep for the sources
	// that did, so after this the reconcile proceeds with whatever is
	// ready.
	defaultReconcileTimeout = 3 * time.Minute
	// defaultReconcileWindow is how long retained configs are collected
	// after subscribing. The broker sends them in a burst, but "the burst
	// has ended" is not something MQTT signals, so this is a
	// wait-and-see. Too short under-collects, which is the safe
	// direction: an orphan missed on this start is swept on the next.
	defaultReconcileWindow = 5 * time.Second
)

// reconcileOrphans runs the sweep once, after the first poll cycles
// have populated the announced set.
//
// It runs as its own errgroup member and always returns nil for
// anything but cancellation: a bridge that publishes correctly but
// cannot tidy up is degraded, not broken, and taking the daemon down
// over it would be a worse outcome than the stale entity it was trying
// to remove.
func (c *Coordinator) reconcileOrphans(ctx context.Context) error {
	if c.hass == nil || c.sub == nil || !c.cfg.HASSCleanup {
		return nil
	}

	ready, timedOut := c.awaitReady(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if timedOut {
		c.log.Info("coordinator.reconcile_partial",
			slog.String("reason", "not every source reported in time"),
			slog.Any("classes", classNames(ready)))
	}
	if len(ready) == 0 {
		c.log.Warn("coordinator.reconcile_skipped",
			slog.String("reason", "no source reported"))
		return nil
	}

	retained, err := c.collectRetainedConfigs(ctx)
	if err != nil {
		c.log.Warn("coordinator.reconcile_failed", slog.String("err", err.Error()))
		return nil
	}

	orphans := c.hass.OrphanConfigs(retained, c.pub.announcedConfigs(), ready)
	if len(orphans) == 0 {
		c.log.Info("coordinator.reconcile_clean",
			slog.Int("retained", len(retained)),
			slog.Any("classes", classNames(ready)))
		return nil
	}

	cleared := 0
	for _, topic := range orphans {
		if err := c.clearConfig(ctx, topic); err != nil {
			c.log.Warn("coordinator.reconcile_clear_failed",
				slog.String("topic", topic), slog.String("err", err.Error()))
			continue
		}
		c.log.Debug("coordinator.reconcile_cleared", slog.String("topic", topic))
		cleared++
	}
	c.log.Info("coordinator.reconcile_done",
		slog.Int("retained", len(retained)),
		slog.Int("cleared", cleared))
	return nil
}

// awaitReady blocks until every enabled source has completed a first
// successful cycle, or the reconcile timeout passes.
func (c *Coordinator) awaitReady(ctx context.Context) (ready map[hass.Class]bool, timedOut bool) {
	deadline := time.NewTimer(c.reconcileTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(reconcileReadyPoll)
	defer tick.Stop()

	for {
		ready = c.readyClasses()
		if allReady(ready) {
			return ready, false
		}
		select {
		case <-ctx.Done():
			return ready, false
		case <-deadline.C:
			return c.readyClasses(), true
		case <-tick.C:
		}
	}
}

// readyClasses reports which discovery classes may be swept.
//
// A class is ready when its source has produced a complete picture: the
// announced set genuinely lists every entity that class should have.
// Until then its absence from that set means "not polled yet", and
// sweeping on that reading deletes live entities along with their
// history — the one failure this whole file exists to avoid.
//
// A disabled source is ready immediately, and that is deliberate rather
// than an oversight: turning CLIENTS.ENABLE off is a decision that
// those entities should go away, so the configs left behind are exactly
// what the sweep is for.
func (c *Coordinator) readyClasses() map[hass.Class]bool {
	ready := make(map[hass.Class]bool, 4)
	if c.readyDevices.Load() {
		ready[hass.ClassDevice] = true
	}
	if c.readyStatic.Load() {
		ready[hass.ClassWLAN] = true
	}
	if !c.cfg.Clients.Enable || c.readyClients.Load() {
		ready[hass.ClassClient] = true
	}
	// Site health is announced only once the classic layer answers, so
	// with classic enabled but failing this stays false and the health
	// entities are left alone until the timeout — at which point their
	// source really has had every chance.
	if !c.cfg.ClassicEnable || c.healthAnnounced.Load() {
		ready[hass.ClassSite] = true
	}
	return ready
}

// allReady reports whether every class is ready.
func allReady(ready map[hass.Class]bool) bool {
	return ready[hass.ClassDevice] && ready[hass.ClassWLAN] &&
		ready[hass.ClassClient] && ready[hass.ClassSite]
}

// classNames lists the ready classes for a log line.
func classNames(ready map[hass.Class]bool) []string {
	out := make([]string, 0, len(ready))
	for _, cl := range []hass.Class{hass.ClassDevice, hass.ClassClient, hass.ClassSite, hass.ClassWLAN} {
		if ready[cl] {
			out = append(out, cl.String())
		}
	}
	return out
}

// collectRetainedConfigs subscribes to the discovery prefix and gathers
// the retained configs the broker replays.
//
// The subscription is torn down before returning: it exists for this
// one burst, and leaving it open would feed every later discovery
// publish — including this daemon's own — back into the read loop for
// no reason.
func (c *Coordinator) collectRetainedConfigs(ctx context.Context) (map[string][]byte, error) {
	filter := c.hass.ConfigFilter()

	var mu sync.Mutex
	retained := make(map[string][]byte)
	handler := func(msg *mqtt.Message) {
		// Runs inline in the MQTT read loop, so it must stay this cheap:
		// anything slower than a copy and a map write stalls
		// acknowledgement processing and the keep-alive watchdog.
		mu.Lock()
		retained[msg.Topic] = append([]byte(nil), msg.Payload...)
		mu.Unlock()
	}

	if _, err := c.sub.Subscribe(ctx, filter, mqtt.QoS0, handler); err != nil {
		return nil, err
	}
	defer func() {
		if u, ok := c.sub.(unsubscriber); ok {
			if err := u.Unsubscribe(context.WithoutCancel(ctx), filter); err != nil {
				c.log.Warn("coordinator.reconcile_unsubscribe_failed",
					slog.String("err", err.Error()))
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.reconcileWindow):
	}

	mu.Lock()
	defer mu.Unlock()
	out := make(map[string][]byte, len(retained))
	for k, v := range retained {
		out[k] = v
	}
	return out, nil
}

// unsubscriber is the optional half of the inbound MQTT contract.
//
// [Subscriber] is deliberately narrow — the birth-message watcher never
// unsubscribes — so this is asserted rather than required, and a
// subscriber without it simply keeps the filter for the process
// lifetime.
type unsubscriber interface {
	Unsubscribe(ctx context.Context, filter string) error
}
