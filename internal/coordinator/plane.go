// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"

	"github.com/SukramJ/go-hamqtt/discovery"
	hapub "github.com/SukramJ/go-hamqtt/publisher"
	hagomqtt "github.com/SukramJ/go-hamqtt/publisher/gomqtt"
	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
)

// The go-hamqtt publishing planes — ADR 0070 phase 9, step 5.
//
// Three of this daemon's four planes now go out through
// github.com/SukramJ/go-hamqtt's publisher package: entity state
// ([hapub.StatePublisher]), the bridge's own birth/death marker and the
// orphan sweep ([hapub.Runtime]), and the inbound command tree
// ([hapub.CommandRouter]). Discovery configs still go out through this
// package's own [publisher.publishConfig] in the per-entity form; the
// device bundle is step 6.
//
// # Every QoS is stated
//
// [hapub.QoS]'s zero value is [hapub.QoSUnset] and every runtime type in
// that package resolves it to **QoS 1**. This is the one bridge in the
// programme that does not publish everything at one level — measurement
// §2.6 read 315 discovery configs and 5 availability markers at QoS 1
// and 317 state publishes at QoS **0**, off the transport call — so an
// omitted field here would not have been "unchanged", it would have
// moved 317 messages per pinned run onto a different delivery guarantee
// with a broker capture as the only evidence. Deliberate QoS 0 is the
// [hapub.QoSAtMostOnce] sentinel (0x80), which is outside the wire's
// 0-2 range precisely so that "unset" and "deliberately at most once"
// cannot be written the same way.
//
// The fourth field measurement F9 names, [hapub.AvailabilityConfig.QoS],
// has **no construction site on this bridge** and that is recorded
// rather than left as an omission: this daemon builds no
// [hapub.AvailabilityPublisher]. Its bridge level is the one retained
// marker [hapub.Runtime] owns, and its device level is not a dedicated
// availability topic at all but the object's own state topic read
// through a `value_template` (measurement F8) — which the state plane
// already writes. A second writer there would fight it.
//
// [hapub.QoSFromWire] (go-hamqtt v0.34.0) has nothing to convert here.
// It exists because `QoS(cfg.MQTT.QoS)` reads correctly and is wrong
// for exactly one input — 0 is [hapub.QoSUnset], which every
// constructor resolves to QoS 1. This bridge exposes no operator knob
// for QoS at all: the values below are compile-time constants, each
// stated as a sentinel.
//
// Only the *publish* levels are measured. StateQoS and the discovery
// and availability levels are read off recorded transport calls by
// TestEveryPlanePublishReachesTheWireAtTheStatedQoS. CommandQoS is not
// among them and cannot be: it is the QoS of a SUBSCRIBE, carried in
// the subscription rather than in any publish, and the transports those
// tests record are publish paths. No test on this bridge records a
// subscribe's QoS anywhere. Its provenance is the value subscribeCommands
// passed before this step, stated so the field reads as a decision
// rather than a default — not as a measurement.
const (
	// StateQoS is every entity state, attributes and `bridge/info`
	// publish: QoS 0, retained. Preservation, not endorsement — the
	// split is argued at internal/coordinator/discovery.go: a lost state
	// value is corrected by the next poll, a lost config is not.
	StateQoS = hapub.QoSAtMostOnce
	// AvailabilityQoS is the bridge's retained online/offline marker,
	// the Last Will derived from it, and the sweep's snapshot
	// subscription — all of which [hapub.Config.QoS] governs together.
	// QoS 1, matching what `publishRaw` passed before this step.
	AvailabilityQoS = hapub.QoSAtLeastOnce
	// CommandQoS is every inbound command subscription. QoS 1, matching
	// what subscribeCommands passed before this step: a command dropped
	// in transit is a button press that did nothing, with nothing
	// anywhere to explain it.
	CommandQoS = hapub.QoSAtLeastOnce
	// PulseQoS is [hapub.StateConfig.PulseQoS]. This daemon never calls
	// [hapub.StatePublisher.Pulse] — it has no event plane — so the
	// field is inert, and it is stated anyway because it is the one
	// field in that package whose default is QoS 0 rather than QoS 1:
	// a plane that states StateQoS and leaves this one alone is saying
	// nothing about its pulses, and go-hamqtt v0.34.0 says so out loud
	// at construction (`publisher.state.pulse_qos_unstated`). Equal to
	// StateQoS by intent, not by coincidence: if this bridge ever grows
	// a pulse it belongs on the same guarantee as every other state
	// publish.
	PulseQoS = StateQoS
)

// RuntimeConfig is the one [hapub.Config] this daemon builds.
//
// A method rather than a literal at the composition root, because a
// second copy is how a daemon and its tests end up configured
// differently — go-mtec2mqtt found exactly that with a mutation:
// dropping LegacyEntityTopics from its main package was caught by
// nothing, because every fixture carried its own copy.
//
// [hapub.Config.Layout] is stated and [hapub.Config.StatusTopic] is
// not: with a layout in hand the runtime derives the status topic from
// [hatopic.Layout.Bridge] and refuses a StatusTopic that disagrees with
// it, so the will, the announcements and all 315 configs' availability
// lists are one string by construction rather than by convention.
func (c *Coordinator) RuntimeConfig(log *slog.Logger) hapub.Config {
	return hapub.Config{
		Prefix:             c.cfg.HASSBaseTopic,
		Layout:             hass.NewLayout(c),
		QoS:                AvailabilityQoS,
		LegacyEntityTopics: hass.LegacyConfigTopicForms(),
		Logger:             log,
	}
}

// newStatePlane builds the state publisher over tr.
//
// [hapub.StateConfig.Encoding] is stated even though this daemon calls
// [hapub.StatePublisher.Publish] with bytes it rendered itself and
// never PublishValue, so the field is inert — the zero value is
// EnvelopeEncoding, and leaving it would be a statement that reads as
// the opposite of what this bridge publishes. Asserted inert by
// TestStateEncodingIsInertAndStatedAnyway.
//
// CommandFilters is the guard that makes a self-echo impossible: a
// broker delivers this process's own publishes back to it, so a state
// topic matching one of its own command filters is a write it performs
// on itself. It is usable here — unlike on go-homeconnect2mqtt, whose
// command filter is a whole device sub-tree — because all six of this
// bridge's filters end in a command suffix no state topic carries.
func newStatePlane(tr hapub.Transport, filters []string, log *slog.Logger) *hapub.StatePublisher {
	return hapub.NewStatePublisher(tr, hapub.StateConfig{
		QoS:            StateQoS,
		PulseQoS:       PulseQoS,
		Encoding:       discovery.RawEncoding,
		CommandFilters: filters,
		Logger:         log,
	})
}

// newCommandRouter builds the inbound router over tr.
//
// Workers is 1: this daemon's handler parses and enqueues onto its own
// bounded channel and nothing else, and the execution behind that
// channel has always been serial. A wider router would buy nothing and
// would reorder two commands for one device.
//
// DeliverRetained stays false, which is the policy the hand-written
// `if msg.Retain` check in onCommand enforced: a retained command is a
// replay, not a request, and the broker re-delivers it on every
// (re)subscribe.
func newCommandRouter(ctx context.Context, tr hapub.Transport, log *slog.Logger) *hapub.CommandRouter {
	return hapub.NewCommandRouter(tr, hapub.CommandConfig{
		QoS:             CommandQoS,
		Workers:         1,
		DeliverRetained: false,
		Lifecycle:       ctx,
		Logger:          log,
	})
}

// --- the transport -----------------------------------------------------

// planePublisher is the publish half of the planes' [hapub.Transport],
// resolved at call time rather than captured.
//
// It has to be late-bound: [Coordinator.SetPublisher] and
// [Coordinator.SetSubscriber] exist because the MQTT client needs the
// Last Will before it is built and the will is the runtime's own
// statement, so the runtime — and therefore its transport — has to
// exist before the client does. Resolving per call is this daemon's
// answer to the ordering knot the sibling bridges solved with a
// mutex-guarded deferred transport; the indirection already existed
// here.
type planePublisher struct{ c *Coordinator }

// Publish routes the bridge's availability marker around the circuit
// breaker and everything else through it.
//
// The asymmetry is deliberate and it closes a self-locking circle.
// [mqtt.Breaker] counts [mqtt.ErrNotConnected] as a failure, so a
// connection drop is exactly what opens the circuit — and the first
// thing a reconnected daemon does is announce itself online. Behind the
// breaker that announcement fails fast with ErrCircuitOpen, nothing
// retries it, and the retained marker stays at the "offline" the
// broker's Last Will just wrote: every entity of the fleet sits
// unavailable under `availability_mode: "all"` until the next
// reconnect, with a daemon that is demonstrably connected and
// publishing state nobody displays. The same applies at shutdown, where
// the offline marker is the only one that goes out at all — a graceful
// DISCONNECT suppresses the will, so a marker the breaker refused
// leaves a retained "online" standing forever.
//
// The breaker exists for volume: several hundred retained topics per
// poll cycle, each otherwise stalling a full AckTimeout while a broker
// is degraded. Birth and death are one publish each, and they are the
// two an operator cannot afford to lose.
func (p planePublisher) Publish(
	ctx context.Context,
	topic string,
	payload []byte,
	qos mqtt.QoS,
	retain bool,
	opts ...mqtt.PublishOption,
) error {
	out := p.c.publisherFor(topic)
	if out == nil {
		return ErrNoPublisher
	}
	return out.Publish(ctx, topic, payload, qos, retain, opts...)
}

// planeSubscriber is the subscribe half.
//
// Subscriptions go to the client directly, never through the breaker —
// the reasoning main.go already carried for the command and birth
// subscriptions: they are startup-path calls with their own
// SUBACK-bounded wait and must not be refused during a publish-side
// brownout.
type planeSubscriber struct{ c *Coordinator }

func (s planeSubscriber) Subscribe(
	ctx context.Context,
	filter string,
	qos mqtt.QoS,
	handler mqtt.MessageHandler,
	opts ...mqtt.SubscribeOption,
) (mqtt.SubscribeResult, error) {
	if s.c.sub == nil {
		return mqtt.SubscribeResult{}, ErrNoSubscriber
	}
	return s.c.sub.Subscribe(ctx, filter, qos, handler, opts...)
}

// Unsubscribe completes [mqtt.Subscriber] over this package's narrower
// [Subscriber], which does not require it — the birth watcher never
// unsubscribes. A subscriber without it reports rather than silently
// leaving a wildcard window installed for the process lifetime, which
// is what the sweep's snapshot would otherwise do.
func (s planeSubscriber) Unsubscribe(ctx context.Context, filter string) error {
	u, ok := s.c.sub.(unsubscriber)
	if !ok {
		return ErrNoSubscriber
	}
	return u.Unsubscribe(ctx, filter)
}

// planeTransport is the [hapub.Transport] all three planes ride.
//
// Built through the shipped adapter rather than by hand: the byte-wise
// QoS translation and the message shim are the fifteen lines ADR 0070
// counted four times over in this family, and the interesting one is
// exactly the QoS.
func (c *Coordinator) planeTransport() hapub.Transport {
	return hagomqtt.Split(planePublisher{c}, planeSubscriber{c})
}

// publisherFor picks the outbound publisher for one topic. See
// [planePublisher.Publish] for why the availability marker is special.
func (c *Coordinator) publisherFor(topic string) Publisher {
	if topic == c.AvailabilityTopic() && c.direct != nil {
		return c.direct
	}
	return c.pub.out
}
