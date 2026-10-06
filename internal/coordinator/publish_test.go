// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"
)

var errBroker = errors.New("broker unreachable")

// Change detection compares the value and nothing else: a clock that
// moves on does not make an unchanged reading new (spec §3.2's "MUST NOT
// republish unchanged state"), and there is no age after which it is
// forced out again — 1.x's FORCE_REPUBLISH is gone.
func TestUnchangedValueIsNeverRepublished(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		broker := &fakeBroker{}
		p := newPublisher(broker, nil, time.Now, slog.New(slog.DiscardHandler))

		if err := p.publish(t.Context(), "a", 21.5); err != nil {
			t.Fatalf("publish: %v", err)
		}
		broker.reset()

		time.Sleep(24 * time.Hour)
		if err := p.publish(t.Context(), "a", 21.5); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if got := broker.total(); got != 0 {
			t.Errorf("an unchanged value went out %d times a day later, want 0", got)
		}
	})
}

// `ts` is the observation and `lc` the last change: a changed value
// moves both, and the replay after a reconnect re-sends the cached
// object, original `ts` included.
func TestStatusTimestamps(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		broker := &fakeBroker{}
		p := newPublisher(broker, nil, time.Now, slog.New(slog.DiscardHandler))

		t0 := time.Now().UnixMilli()
		if err := p.publish(t.Context(), "a", "ONLINE"); err != nil {
			t.Fatalf("publish: %v", err)
		}
		first, _ := broker.latestRaw("a")
		if want := fmt.Sprintf(`{"val":"ONLINE","ts":%d,"lc":%d}`, t0, t0); first != want {
			t.Errorf("first = %s, want %s", first, want)
		}

		time.Sleep(time.Minute)
		t1 := time.Now().UnixMilli()
		if err := p.publish(t.Context(), "a", "OFFLINE"); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if got, _ := broker.latestRaw("a"); got != fmt.Sprintf(`{"val":"OFFLINE","ts":%d,"lc":%d}`, t1, t1) {
			t.Errorf("after a change = %s", got)
		}

		time.Sleep(time.Minute)
		broker.reset()
		if n, err := p.republish(t.Context()); err != nil || n != 1 {
			t.Fatalf("republish = %d, %v; want 1, nil", n, err)
		}
		if got, _ := broker.latestRaw("a"); got != fmt.Sprintf(`{"val":"OFFLINE","ts":%d,"lc":%d}`, t1, t1) {
			t.Errorf("the replay = %s, want the cached object with its original ts", got)
		}
	})
}

// A failed publish must not be recorded as sent, or change detection
// would suppress the retry and the value would never arrive.
func TestFailedPublishIsNotRemembered(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{fail: errBroker}
	p := newPublisher(broker, nil, nil, slog.New(slog.DiscardHandler))

	if err := p.publish(t.Context(), "a", "v"); err == nil {
		t.Fatal("publish succeeded against a failing broker")
	}
	if got := p.knownTopics(); len(got) != 0 {
		t.Errorf("remembered %v after a failed publish", got)
	}

	broker.fail = nil
	if err := p.publish(t.Context(), "a", "v"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := broker.count("a"); got != 1 {
		t.Errorf("value published %d times after recovery, want 1", got)
	}
}

func TestForgetOnlyDropsMatchingPrefix(t *testing.T) {
	t.Parallel()

	p := newPublisher(&fakeBroker{}, nil, nil, slog.New(slog.DiscardHandler))

	for _, topic := range []string{"d/aa/state", "d/aa/uptime", "d/bb/state"} {
		if err := p.publish(t.Context(), topic, "v"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	dropped := p.forget("d/aa/")
	if len(dropped) != 2 {
		t.Errorf("forget dropped %v, want the two d/aa topics", dropped)
	}
	remaining := p.knownTopics()
	if len(remaining) != 1 || remaining[0] != "d/bb/state" {
		t.Errorf("remaining topics = %v, want only d/bb/state", remaining)
	}
}

// The claim list records what the broker accepted, never what was
// merely attempted.
//
// published is the sweep's entire licence to delete a retained config,
// so a publish the broker refused must not create one. This is
// go-hamqtt's own discipline, stated there in the same words, and
// before this change the two diverged: the topic was claimed before the
// send and never released on failure. No failing case followed from it
// — a wrongly claimed topic must also leave the announced set, which
// only a successful publish does — but a licence to delete is not a
// thing to hold on the strength of an argument that nothing enforces.
func TestAConfigPublishTheBrokerRefusedIsNotClaimed(t *testing.T) {
	t.Parallel()

	broker := &fakeBroker{fail: errBroker}
	p := newPublisher(broker, nil, nil, slog.New(slog.DiscardHandler))
	const topic = "homeassistant/sensor/unifi_00005e005301/state/config"

	if err := p.publishConfig(t.Context(), topic, []byte(`{"unique_id":"unifi_00005e005301_state"}`)); err == nil {
		t.Fatal("publishConfig succeeded against a broker that refuses everything")
	}
	if p.claims().Published[topic] {
		t.Errorf("%s is claimed as published although the broker refused it; "+
			"the sweep would take that as licence to retract whatever is retained there", topic)
	}

	// And the accepted one is claimed, so the assertion above is not
	// simply "nothing is ever claimed".
	broker.mu.Lock()
	broker.fail = nil
	broker.mu.Unlock()
	if err := p.publishConfig(t.Context(), topic, []byte(`{"unique_id":"unifi_00005e005301_state"}`)); err != nil {
		t.Fatalf("publishConfig: %v", err)
	}
	if !p.claims().Published[topic] {
		t.Errorf("%s is not claimed although the broker accepted it; the sweep could "+
			"never retract this daemon's own leftover", topic)
	}
	// A republish the dedup gate suppresses needs no record of its own —
	// the send it was deduplicated against already made one.
	if err := p.publishConfig(t.Context(), topic, []byte(`{"unique_id":"unifi_00005e005301_state"}`)); err != nil {
		t.Fatalf("publishConfig (deduplicated): %v", err)
	}
	if !p.claims().Published[topic] {
		t.Errorf("%s lost its claim to change detection", topic)
	}
}
