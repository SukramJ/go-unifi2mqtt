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
// change between two polls. mqtt-smarthome 2.0 §3.2 makes it a rule —
// an adapter "MUST NOT republish unchanged state" — and a subscriber
// that missed a message is served by the retained value and by the full
// replay on every broker reconnect ([publisher.republish]), not by a
// periodic refresh (CONCEPT.md §8.3).
//
// It is safe for concurrent use: several poll loops publish at once.
type publisher struct {
	out Publisher
	// state is the go-hamqtt state plane: the dedup gate on `val`, the
	// `{"val","ts","lc"}` status object, the retained publish and the
	// index a removed device's topics are dropped from, all at
	// [StateQoS]. The discovery configs below still go out through
	// `out` directly, in the per-entity form.
	state *hapub.StatePublisher
	log   *slog.Logger

	mu sync.Mutex
	// lastConfig holds the discovery config payload last sent per topic.
	// The state plane's equivalent lives inside [hapub.StatePublisher].
	lastConfig map[string]entry
	// emptied is the state topics whose last publish carried no value.
	//
	// An empty retained payload is a retraction, so it goes out through
	// [hapub.StatePublisher.Evict] — which has no dedup gate, by design,
	// because eviction is normally a one-shot. Here it is not: an absent
	// client publishes an empty `ip` and an empty `signal` on every poll
	// for as long as it stays away, and change detection has always
	// suppressed the repeats. This set is what keeps that true, for a
	// fleet where "away" is the steady state of most of it.
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
	clock func() time.Time,
	log *slog.Logger,
) *publisher {
	p := &publisher{
		out:        out,
		log:        log,
		lastConfig: make(map[string]entry),
		emptied:    make(map[string]bool),
		configs:    make(map[string]bool),
		published:  make(map[string]bool),
	}
	p.state = newStatePlane(hagomqtt.Split(statePlaneTransport{p}, nil), commandFilters, clock, log)
	return p
}

// publish writes one status item unless its value is unchanged.
//
// value is the item's `val` — a JSON boolean, number, string or a
// structured value — and the comparison is [hapub.StatePublisher]'s
// gate on it, never on the timestamps. A nil value means "no value" and
// clears the retained item instead (spec §5.1), once: repeats of an
// empty item are suppressed here, because the library's eviction has no
// gate. Errors are returned rather than logged here: the caller knows
// whether a failed publish should abort its poll (it should not) or be
// counted.
func (p *publisher) publish(ctx context.Context, topic string, value any) error {
	if p.state == nil {
		return ErrNoPublisher
	}

	if value == nil {
		p.mu.Lock()
		wasEmpty := p.emptied[topic]
		p.mu.Unlock()
		if wasEmpty {
			return nil
		}
		// An empty retained payload deletes the value rather than
		// writing one, which the library refuses through PublishStatus
		// and says on purpose through Evict.
		if err := p.state.Evict(ctx, topic); err != nil {
			return err
		}
		p.mu.Lock()
		p.emptied[topic] = true
		p.mu.Unlock()
		return nil
	}

	written, err := p.state.PublishStatus(ctx, topic, hapub.Observation{Value: value})
	if err != nil {
		return err
	}
	if written {
		p.mu.Lock()
		delete(p.emptied, topic)
		p.mu.Unlock()
	}
	return nil
}

// pulse writes one non-retained status item — an event, which has no
// gate and no memory (spec §3.2).
func (p *publisher) pulse(ctx context.Context, topic string, value any) error {
	if p.state == nil {
		return ErrNoPublisher
	}
	return p.state.PulseStatus(ctx, topic, hapub.Observation{Value: value})
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
		skip := known && bytes.Equal(prev.payload, payload)
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
	p.lastConfig[topic] = entry{payload: payload}
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

// wasPublished reports whether this process has successfully published
// a discovery config to topic since it started.
func (p *publisher) wasPublished(topic string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.published[topic]
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
	for topic := range p.emptied {
		if strings.HasPrefix(topic, prefix) {
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

// clear drops the remembered discovery payloads, so the next announce
// re-sends every config. Called after a broker reconnect, because a
// broker that lost its retained store (or a different broker entirely)
// would otherwise never receive the configs change detection is
// suppressing.
//
// The state plane is not touched here: [publisher.republish] replays
// it whole on the same reconnect, and keeping its memory is what keeps
// each item's `lc` — the moment its value last changed — across the
// drop.
func (p *publisher) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	clear(p.lastConfig)
}

// republish re-sends every remembered status item unchanged, original
// `ts` included, and reports how many went out — spec §3.2's "again
// after every broker reconnect, so the broker's retained state is
// complete". An emptied item needs no replay: absent on the broker is
// what it already is.
func (p *publisher) republish(ctx context.Context) (int, error) {
	if p.state == nil {
		return 0, ErrNoPublisher
	}
	return p.state.Republish(ctx)
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
