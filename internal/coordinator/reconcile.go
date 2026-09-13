// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	hapub "github.com/SukramJ/go-hamqtt/publisher"

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
// and clears the orphans among them. Doing that safely is a question of
// when — see [Coordinator.readyClasses] — and, since this PR, of what
// counts as ours at all.
//
// # What the sweep may and may not clear
//
// It clears only a topic *this process published since it started* and
// no longer announces. It never clears a config it merely recognises,
// however exactly that config matches this daemon's shape, because on
// this bridge the shape is not distinguishing: two instances bridging
// two different UniFi consoles to one broker with the shipped
// configuration produce byte-identical config topics, unique_ids,
// availability topics and state topics for the whole site plane. The
// sweep used to clear on that recognition, and it deleted the other
// console's entities out of Home Assistant. See the head of
// internal/hass/cleanup.go.
//
// **The cost is stated, not hidden.** A config genuinely left behind by
// an *earlier run of this daemon* — an entity that version named
// differently, a device unplugged while the daemon was stopped — was
// also not published by this process, so it is no longer cleared
// either. It is logged, once, as coordinator.reconcile_unclaimed, with
// the topics named, and an operator who knows there is no second
// console can clear them with a single empty retained publish each. A
// stale entity an operator can see and delete is a far better outcome
// than a neighbour's fleet deleted silently, which is what the
// alternative actually did.
//
// This is the same conclusion go-homeconnect2mqtt reached in its PR #44
// by a different route: look, do not touch, and retract only over a
// list the caller narrowed itself. There the narrowing could still be a
// payload predicate, because its instances differ by MQTT root inside
// the state topic. Here they do not, so the narrowing has to be the
// claim list and nothing else.

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

	orphans, unclaimed, unready := c.hass.OrphanConfigs(retained, c.pub.claims(), ready)
	if len(unclaimed) > 0 {
		// Reported, never cleared. Each of these either belongs to an
		// earlier run of this daemon or is a second console's live
		// entity, and nothing on the wire tells the two apart. Their
		// class has reported, so "this run does not announce it" is a
		// statement about the entity rather than about the source.
		c.log.Info("coordinator.reconcile_unclaimed",
			slog.Int("count", len(unclaimed)),
			slog.Any("topics", unclaimed),
			slog.String("action", "left alone: this process did not publish them, "+
				"and the source that would have is up to date. "+
				"If no second UniFi console shares this broker and MQTT root, "+
				"clear them by publishing an empty retained payload to each"))
	}
	if len(unready) > 0 {
		// The same shape, the opposite advice. These belong to classes
		// that never reported in this run, so their absence from the
		// announced set may mean nothing more than that their source is
		// down: with the classic layer failing, every site-health config
		// this daemon itself publishes lands here. Telling an operator
		// they are safe to clear is telling them to delete their own live
		// entities, and their history with them.
		c.log.Warn("coordinator.reconcile_unclaimed_unready",
			slog.Int("count", len(unready)),
			slog.Any("topics", unready),
			slog.Any("silent_classes", classNames(unreadyClasses(ready))),
			slog.String("action", "left alone and NOT safe to clear: the source that "+
				"would announce them has not reported in this run, so these may be "+
				"this daemon's own live entities. Fix the source, restart, and read "+
				"coordinator.reconcile_unclaimed on a run where every source reported"))
	}
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
// than an oversight: turning CLIENTS.ENABLE off is a decision that its
// entities should go away, so its leftovers are the sweep's business
// rather than something to wait on.
//
// What that readiness buys is narrower than it was before the claim
// list. Ready or not, the sweep only ever retracts a topic *this
// process published*, and a process that runs with CLIENTS.ENABLE off
// publishes no client configs at all — so the configs an earlier run
// left behind are not in Published and are reported as unclaimed rather
// than cleared. Readiness decides which of the two reports a config
// lands in, and therefore what the operator is told about it; it no
// longer decides whether a disabled source's leftovers get swept,
// because nothing does.
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

// unreadyClasses is the complement of ready over the four classes, so
// the report of the configs a silent source left behind can name which
// source that was. An operator reading "site" here knows the classic
// layer is what to fix.
func unreadyClasses(ready map[hass.Class]bool) map[hass.Class]bool {
	out := make(map[hass.Class]bool, 4)
	for _, cl := range []hass.Class{hass.ClassDevice, hass.ClassClient, hass.ClassSite, hass.ClassWLAN} {
		if !ready[cl] {
			out[cl] = true
		}
	}
	return out
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

// collectRetainedConfigs opens one report-only snapshot window over the
// discovery prefix and returns the retained configs of this daemon's
// own shape that the broker replayed.
//
// It is [hapub.Runtime.Sweep] with ReportOnly set, and the two halves
// of that sentence matter separately.
//
// # Why the library's own retraction is not used
//
// A retracting pass clears every *owned* topic the window saw that the
// runtime does not claim — where "claims" means "this Runtime published
// it". That is the exact inverse of this bridge's rule. Here the sweep
// may clear only a topic this **process** published and has since given
// up ([hass.Claims]), because the shape is not distinguishing: two
// instances bridging two different UniFi consoles to one broker with
// the shipped configuration emit byte-identical config topics,
// unique_ids, availability topics and state topics for the whole site
// plane, and a retained config that looks exactly like ours may be a
// sibling console's live entity. The runtime cannot express that: it
// publishes no discovery config at all at this step, so its claim set
// is empty and an armed pass would judge this daemon's entire retained
// fleet — and the neighbour's — an orphan, once per boot. [SweepResult.Unclaimed]
// is therefore deliberately unread; the judgement stays in
// [hass.Discovery.OrphanConfigs], over the claim list, which is the
// whole of the evidence and not a second signal beside a payload
// predicate.
//
// What the library does supply is the window itself: one subscription
// that is torn down on every exit path — a cancelled context and a
// broker that refuses the UNSUBSCRIBE included — a gate that stops a
// failed teardown accumulating for the process lifetime, and one parser
// for all three discovery topic forms instead of a wildcard shape that
// matches only one.
//
// # The one wire-visible change
//
// The window is `<prefix>/#` where this daemon subscribed
// `<prefix>/+/+/+/config`. For the few seconds it is open this daemon
// *receives* every retained message under the discovery prefix; it acts
// on none that [hass.OwnsConfigTopic] and [hass.Discovery.IsOwnConfig]
// do not both claim, and the pass retracts nothing at all. The widening
// is in what it reads, never in what it writes.
func (c *Coordinator) collectRetainedConfigs(ctx context.Context) (map[string][]byte, error) {
	rt := c.ha()
	if rt == nil {
		return nil, ErrNoPublisher
	}
	prefix := rt.Prefix()

	var mu sync.Mutex
	retained := make(map[string][]byte)
	res, err := rt.Sweep(ctx, hapub.SweepRequest{
		// Look, never touch. The retraction below is this daemon's own,
		// over a list this daemon narrowed.
		//
		// [hapub.SweepRequest.SelfClaimed] — go-hamqtt v0.34.0, modelled
		// on this bridge's claim list — is deliberately NOT set. It
		// would narrow the sweep to topics this process published,
		// which is the same narrowing the retraction already applies
		// one layer down; what it would additionally remove is the
		// *report*. The unclaimed topics are precisely what this window
		// exists to surface as coordinator.reconcile_unclaimed, with
		// the remedy an operator can act on, so here the guard's cost
		// is the sweep's purpose.
		ReportOnly: true,
		Window:     c.reconcileWindow,
		Owns:       hass.OwnsConfigTopic,
		Inspect: func(t hapub.ConfigTopic, body []byte) {
			// Runs inline in the MQTT read loop, so it must stay this
			// cheap: anything slower than a copy and a map write stalls
			// acknowledgement processing and the keep-alive watchdog.
			topic := hass.ConfigTopicFor(prefix, t)
			if topic == "" {
				return
			}
			mu.Lock()
			retained[topic] = append([]byte(nil), body...)
			mu.Unlock()
		},
	})
	if err != nil {
		return nil, err
	}
	// Inspected is logged beside the verdict because a window that saw
	// none of this daemon's retained configs and one that saw them all
	// and correctly found nothing orphaned both read as "0 cleared"
	// otherwise, and they are completely different faults.
	c.log.Debug("coordinator.reconcile_window",
		slog.Int("inspected", res.Inspected),
		slog.Int("collected", len(retained)))

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
