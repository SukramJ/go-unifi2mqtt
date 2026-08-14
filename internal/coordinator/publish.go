// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"
)

// ErrNoPublisher is returned when a publish is attempted before an
// outbound publisher was wired in.
//
// This is a guard against a wiring order that is genuinely easy to get
// wrong: the MQTT lifecycle invokes its connect hook from inside
// Start(), so a hook that publishes runs before any code written after
// Start() does. Getting that wrong should surface as a logged error,
// never as a nil dereference that takes the daemon down.
var ErrNoPublisher = errors.New("coordinator: no MQTT publisher configured")

// Publisher is the outbound MQTT contract, narrowed to what the
// coordinator uses. It matches the interface in go-mqtt verbatim so the
// real client satisfies it for free.
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte, qos mqtt.QoS, retain bool, opts ...mqtt.PublishOption) error
}

// publisher wraps a Publisher with change detection.
//
// Publishing only on change is what keeps a broker with many
// subscribers from carrying pointless traffic: a site with 12 devices
// and 121 clients produces hundreds of topics, and almost none of them
// change between two polls. The periodic forced republish exists
// because "only on change" alone would leave a subscriber that missed a
// message — or one that does not use retained values — permanently
// stale (CONCEPT.md §8.3).
//
// It is safe for concurrent use: several poll loops publish at once.
type publisher struct {
	out Publisher
	log *slog.Logger
	now func() time.Time

	// forceEvery is how often the next publish of every topic bypasses
	// change detection. Zero disables forced republishing.
	forceEvery time.Duration

	mu sync.Mutex
	// last holds what was most recently sent per topic: the payload
	// change detection compares against, plus when it went out.
	//
	// The timestamp is per topic rather than a single global deadline on
	// purpose. A global one would be consumed by whichever publish
	// happened to run first after it expired, and every other topic in
	// that cycle would still be suppressed — turning "republish
	// everything every 10 minutes" into "republish one topic every 10
	// minutes". Per-topic ages also spread the forced traffic out
	// instead of bunching it into one burst.
	last map[string]entry
}

// entry is one topic's last publication.
type entry struct {
	payload []byte
	at      time.Time
}

func newPublisher(out Publisher, forceEvery time.Duration, now func() time.Time, log *slog.Logger) *publisher {
	return &publisher{
		out:        out,
		log:        log,
		now:        now,
		forceEvery: forceEvery,
		last:       make(map[string]entry),
	}
}

// publish sends payload to topic unless an identical payload was
// already sent and no forced republish is due.
//
// Errors are returned rather than logged here: the caller knows whether
// a failed publish should abort its poll (it should not) or be counted.
func (p *publisher) publish(ctx context.Context, topic, payload string) error {
	if p.out == nil {
		return ErrNoPublisher
	}

	body := []byte(payload)
	now := p.now()

	p.mu.Lock()
	prev, known := p.last[topic]
	skip := known && bytes.Equal(prev.payload, body) && !p.staleLocked(prev, now)
	if skip {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	// Publish outside the lock: a QoS 1 publish blocks until PUBACK, and
	// holding the mutex across that would serialise every poll loop
	// behind the slowest broker round-trip.
	if err := p.out.Publish(ctx, topic, body, mqtt.QoS0, true); err != nil {
		return err
	}

	p.mu.Lock()
	p.last[topic] = entry{payload: body, at: now}
	p.mu.Unlock()
	return nil
}

// staleLocked reports whether an unchanged topic is old enough to be
// republished anyway. Must be called with the mutex held.
func (p *publisher) staleLocked(e entry, now time.Time) bool {
	if p.forceEvery <= 0 {
		return false
	}
	return !now.Before(e.at.Add(p.forceEvery))
}

// publishRaw sends a payload bypassing change detection entirely, for
// the availability topics that must land on every (re)connect even when
// their value did not change.
func (p *publisher) publishRaw(ctx context.Context, topic, payload string, qos mqtt.QoS) error {
	if p.out == nil {
		return ErrNoPublisher
	}

	body := []byte(payload)
	if err := p.out.Publish(ctx, topic, body, qos, true); err != nil {
		return err
	}

	p.mu.Lock()
	p.last[topic] = entry{payload: body, at: p.now()}
	p.mu.Unlock()
	return nil
}

// forget clears the remembered payloads for every topic under prefix
// and returns the topics that were dropped.
//
// This is what makes a device that comes back after being removed
// publish its full state again instead of being suppressed by change
// detection against a stale memory.
func (p *publisher) forget(prefix string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var dropped []string
	for topic := range p.last {
		if strings.HasPrefix(topic, prefix) {
			dropped = append(dropped, topic)
		}
	}
	for _, topic := range dropped {
		delete(p.last, topic)
	}
	slices.Sort(dropped)
	return dropped
}

// clear drops all remembered payloads, forcing the next poll to
// republish everything. Called after a broker reconnect, because a
// broker that lost its retained store (or a different broker entirely)
// would otherwise never receive the values change detection is
// suppressing.
func (p *publisher) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.last)
}

// knownTopics returns the topics with a remembered payload, sorted.
// Used by tests and by the orphan sweep.
func (p *publisher) knownTopics() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(maps.Keys(p.last))
}
