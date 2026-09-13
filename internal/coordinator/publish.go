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

	hapub "github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"
	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
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
	// state is the go-hamqtt state plane: the dedup gate, the retained
	// publish and the index a removed device's topics are dropped from,
	// all at [StateQoS]. ADR 0070 phase 9 step 5 moved every state
	// publish onto it; the discovery configs below still go out through
	// `out` directly, in the per-entity form, and the bundle is step 6.
	state *hapub.StatePublisher
	log   *slog.Logger
	now   func() time.Time

	// forceEvery is how often the next publish of every topic bypasses
	// change detection. Zero disables forced republishing.
	forceEvery time.Duration

	mu sync.Mutex
	// lastConfig holds the discovery config payload last sent per topic.
	// The state plane's equivalent lives inside [hapub.StatePublisher];
	// this half stays here until step 6 moves discovery too.
	lastConfig map[string]entry
	// sentAt is when each state topic last actually went out, and it is
	// the whole of what this type still owns on the state plane.
	//
	// The library's dedup gate has no notion of age, and the forced
	// periodic republish needs one: a subscriber that missed a message,
	// or one that does not use retained values, would otherwise stay
	// permanently stale (CONCEPT.md §8.3). The timestamp is per topic
	// rather than a single global deadline on purpose. A global one
	// would be consumed by whichever publish happened to run first after
	// it expired, and every other topic in that cycle would still be
	// suppressed — turning "republish everything every 10 minutes" into
	// "republish one topic every 10 minutes". Per-topic ages also spread
	// the forced traffic out instead of bunching it into one burst.
	sentAt map[string]time.Time
	// emptied is the state topics whose last publish carried no bytes.
	//
	// An empty retained payload is a retraction, so it goes out through
	// [hapub.StatePublisher.Evict] — which has no dedup gate, by design,
	// because eviction is normally a one-shot. Here it is not: an absent
	// client publishes an empty `ip` and an empty `signal` on every poll
	// for as long as it stays away, and before this step change
	// detection suppressed the repeats. This set is what keeps that
	// true, for a fleet where "away" is the steady state of most of it.
	emptied map[string]bool
	// configs is the set of discovery config topics currently announced.
	//
	// Separate from last because the two answer different questions.
	// last is "what did we send", and a reconnect wipes it so everything
	// is resent; configs is "which entities do we currently claim", which
	// a reconnect does not change. The orphan reconcile compares the
	// broker's retained configs against this set, so clearing it on
	// reconnect would make every entity look orphaned.
	configs map[string]bool
	// published is every discovery config topic this process has
	// successfully published since it started. Unlike configs it never
	// shrinks: a retracted topic stays in it, which is what lets a
	// retraction that did not stick be retried.
	//
	// A publish the broker refused is not in it. The set is the sweep's
	// licence to delete a retained config, so it records what the broker
	// accepted and never what was merely attempted — see publishConfig.
	//
	// This is the sweep's entire ownership evidence, and the reason it
	// is kept at all is that nothing in a published payload can supply
	// it. Two instances of this daemon bridging two different UniFi
	// consoles to one broker emit byte-identical config topics,
	// unique_ids, availability topics and state topics for the whole
	// site plane, so a retained config that looks exactly like ours may
	// be a sibling console's live entity. Having published a topic is
	// the one fact about it no sibling can forge — see
	// [hass.Claims].
	published map[string]bool
}

// entry is one topic's last publication.
type entry struct {
	payload []byte
	at      time.Time
}

// statePlaneTransport is the publish-only [hapub.Transport] the state
// plane rides.
//
// It resolves [publisher.out] per call rather than capturing it,
// because [Coordinator.SetPublisher] wires the real one in after
// construction — the MQTT client cannot exist until the Last Will does,
// and the will is the discovery runtime's own statement.
// [hapub.StatePublisher] never subscribes, so the subscribe half is not
// supplied.
type statePlaneTransport struct{ p *publisher }

func (t statePlaneTransport) Publish(
	ctx context.Context,
	topic string,
	payload []byte,
	qos mqtt.QoS,
	retain bool,
	opts ...mqtt.PublishOption,
) error {
	if t.p.out == nil {
		return ErrNoPublisher
	}
	return t.p.out.Publish(ctx, topic, payload, qos, retain, opts...)
}

func newPublisher(
	out Publisher,
	commandFilters []string,
	forceEvery time.Duration,
	now func() time.Time,
	log *slog.Logger,
) *publisher {
	p := &publisher{
		out:        out,
		log:        log,
		now:        now,
		forceEvery: forceEvery,
		lastConfig: make(map[string]entry),
		sentAt:     make(map[string]time.Time),
		emptied:    make(map[string]bool),
		configs:    make(map[string]bool),
		published:  make(map[string]bool),
	}
	p.state = newStatePlane(hagomqtt.Split(statePlaneTransport{p}, nil), commandFilters, log)
	return p
}

// publish sends payload to topic unless an identical payload was
// already sent and no forced republish is due.
//
// The comparison is [hapub.StatePublisher]'s hash-dedup gate; what this
// method adds is the age check the library has no notion of, and the
// empty-payload branch. Errors are returned rather than logged here:
// the caller knows whether a failed publish should abort its poll (it
// should not) or be counted.
func (p *publisher) publish(ctx context.Context, topic, payload string) error {
	if p.state == nil {
		return ErrNoPublisher
	}
	now := p.now()

	p.mu.Lock()
	at, known := p.sentAt[topic]
	stale := known && p.staleLocked(at, now)
	wasEmpty := p.emptied[topic]
	p.mu.Unlock()

	// An empty retained payload deletes the value rather than writing
	// one, which the library refuses through Publish and says on purpose
	// through Evict. The bytes on the wire are identical either way.
	if payload == "" {
		if wasEmpty && !stale {
			return nil
		}
		if err := p.state.Evict(ctx, topic); err != nil {
			return err
		}
		p.mu.Lock()
		p.sentAt[topic] = now
		p.emptied[topic] = true
		p.mu.Unlock()
		return nil
	}

	// Forgetting is how an age check is expressed against a gate that
	// only knows payloads: the next publish then goes out because the
	// gate has nothing to compare against, not because anything changed.
	if stale {
		p.state.Forget(topic)
	}
	written, err := p.state.Publish(ctx, topic, []byte(payload))
	if err != nil {
		return err
	}
	if !written {
		return nil
	}
	p.mu.Lock()
	p.sentAt[topic] = now
	delete(p.emptied, topic)
	p.mu.Unlock()
	return nil
}

// staleLocked reports whether an unchanged topic is old enough to be
// republished anyway. Must be called with the mutex held.
func (p *publisher) staleLocked(at, now time.Time) bool {
	if p.forceEvery <= 0 {
		return false
	}
	return !now.Before(at.Add(p.forceEvery))
}

// publishConfig sends a Home Assistant discovery config.
//
// QoS 1 and retained, unlike state values: a config creates an entity,
// and a lost one leaves a device silently missing from Home Assistant
// while its state topics keep arriving. A nil payload clears the
// retained message, which is how an entity is removed.
//
// It goes through change detection like everything else — a config
// republished on every poll would be pure noise — but a nil payload
// always goes out, because "already deleted" is not something worth
// optimising and skipping it would leave a stale entity behind.
func (p *publisher) publishConfig(ctx context.Context, topic string, payload []byte) error {
	if p.out == nil {
		return ErrNoPublisher
	}

	now := p.now()
	if payload != nil {
		p.mu.Lock()
		// Recorded before the skip check, not after: change detection
		// suppresses the send, but the entity is still claimed. Recording
		// only on the sends that go out would drop a topic from the set
		// the moment its payload stopped changing — and the reconcile
		// would then read it back off the broker as an orphan and delete
		// a live entity.
		p.configs[topic] = true
		prev, known := p.lastConfig[topic]
		skip := known && bytes.Equal(prev.payload, payload) && !p.staleLocked(prev.at, now)
		p.mu.Unlock()
		if skip {
			return nil
		}
	}

	if err := p.out.Publish(ctx, topic, payload, mqtt.QoS1, true); err != nil {
		return err
	}
	if payload == nil {
		p.mu.Lock()
		delete(p.configs, topic)
		p.mu.Unlock()
		return nil
	}

	p.mu.Lock()
	p.lastConfig[topic] = entry{payload: payload, at: now}
	// Recorded only now, and never removed: what the broker accepted,
	// never what was merely attempted. configs answers "is this a live
	// entity of ours" and is recorded before the send, because change
	// detection suppresses the send while the entity stays claimed;
	// published answers "did this process ever put that topic on the
	// broker", which is the sweep's sole entitlement to retract it, so a
	// publish that returned an error must not create one. A skipped
	// republish needs no record here — the send it was deduplicated
	// against is the one that made it.
	//
	// This is go-hamqtt's discipline, stated there in the same words.
	p.published[topic] = true
	p.mu.Unlock()
	return nil
}

// announcedConfigs is the set of discovery config topics currently
// claimed, copied so the caller cannot race the poll loops.
func (p *publisher) announcedConfigs() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.configs)
}

// claims is everything this process knows first-hand about its own
// discovery publications, copied under one lock so the two sets cannot
// be read a poll apart — Announced must always be a subset of
// Published, and a torn read could break that.
func (p *publisher) claims() hass.Claims {
	p.mu.Lock()
	defer p.mu.Unlock()
	return hass.Claims{
		Published: maps.Clone(p.published),
		Announced: maps.Clone(p.configs),
	}
}

// forget clears the remembered payloads for every topic under prefix
// and returns the topics that were dropped.
//
// This is what makes a device that comes back after being removed
// publish its full state again instead of being suppressed by change
// detection against a stale memory.
func (p *publisher) forget(prefix string) []string {
	var dropped []string
	if p.state != nil {
		for _, topic := range p.state.Published() {
			if strings.HasPrefix(topic, prefix) {
				dropped = append(dropped, topic)
			}
		}
		p.state.Forget(dropped...)
	}

	p.mu.Lock()
	for topic := range p.sentAt {
		if strings.HasPrefix(topic, prefix) {
			delete(p.sentAt, topic)
			delete(p.emptied, topic)
		}
	}
	for topic := range p.lastConfig {
		if strings.HasPrefix(topic, prefix) {
			dropped = append(dropped, topic)
			delete(p.lastConfig, topic)
		}
	}
	p.mu.Unlock()

	slices.Sort(dropped)
	return slices.Compact(dropped)
}

// clear drops all remembered payloads, forcing the next poll to
// republish everything. Called after a broker reconnect, because a
// broker that lost its retained store (or a different broker entirely)
// would otherwise never receive the values change detection is
// suppressing.
func (p *publisher) clear() {
	if p.state != nil {
		// Reset opens the gate and keeps the index, which is what a
		// reconnect needs: forgetting the fleet as well would leave
		// [publisher.forget] and the eviction path with no worklist.
		p.state.Reset()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.lastConfig)
	clear(p.sentAt)
	clear(p.emptied)
}

// knownTopics returns the topics with a remembered payload, sorted.
// Used by tests and by the orphan sweep.
func (p *publisher) knownTopics() []string {
	var out []string
	if p.state != nil {
		out = append(out, p.state.Published()...)
	}
	p.mu.Lock()
	out = append(out, slices.Collect(maps.Keys(p.lastConfig))...)
	p.mu.Unlock()
	slices.Sort(out)
	return slices.Compact(out)
}
