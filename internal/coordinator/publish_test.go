// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"errors"
	"log/slog"
	"testing"
	"time"
)

var errBroker = errors.New("broker unreachable")

// The forced republish deliberately ages each topic on its own clock.
//
// A single global deadline would be consumed by whichever publish ran
// first after it expired, leaving every other topic in that cycle still
// suppressed — turning "republish everything every 10 minutes" into
// "republish one topic every 10 minutes". This test pins that.
func TestForceRepublishIsPerTopic(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	broker := &fakeBroker{}
	p := newPublisher(broker, 10*time.Minute, clock.now, slog.New(slog.DiscardHandler))

	// Two topics published at the same moment.
	for _, topic := range []string{"a", "b"} {
		if err := p.publish(t.Context(), topic, "v"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	broker.reset()

	// Unchanged and not yet stale: both suppressed.
	clock.advance(9 * time.Minute)
	for _, topic := range []string{"a", "b"} {
		if err := p.publish(t.Context(), topic, "v"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if got := broker.total(); got != 0 {
		t.Fatalf("published %d messages before the age limit, want 0", got)
	}

	// Past the limit: BOTH must go out, not just the first one.
	clock.advance(2 * time.Minute)
	for _, topic := range []string{"a", "b"} {
		if err := p.publish(t.Context(), topic, "v"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if got := broker.total(); got != 2 {
		t.Errorf("forced republish sent %d messages, want 2 — the deadline is not per topic", got)
	}
}

// A topic published later than its neighbours ages on its own schedule,
// which spreads forced traffic instead of bunching it into one burst.
func TestForceRepublishStaggers(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	broker := &fakeBroker{}
	p := newPublisher(broker, 10*time.Minute, clock.now, slog.New(slog.DiscardHandler))

	if err := p.publish(t.Context(), "early", "v"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	clock.advance(5 * time.Minute)
	if err := p.publish(t.Context(), "late", "v"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 11 minutes after "early", 6 after "late": only "early" is stale.
	clock.advance(6 * time.Minute)
	broker.reset()
	for _, topic := range []string{"early", "late"} {
		if err := p.publish(t.Context(), topic, "v"); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if got := broker.count("early"); got != 1 {
		t.Errorf("stale topic republished %d times, want 1", got)
	}
	if got := broker.count("late"); got != 0 {
		t.Errorf("fresh topic republished %d times, want 0", got)
	}
}

// With forcing disabled, an unchanged topic must never be republished.
func TestForceRepublishDisabled(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	broker := &fakeBroker{}
	p := newPublisher(broker, 0, clock.now, slog.New(slog.DiscardHandler))

	if err := p.publish(t.Context(), "a", "v"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	broker.reset()

	clock.advance(24 * time.Hour)
	if err := p.publish(t.Context(), "a", "v"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := broker.total(); got != 0 {
		t.Errorf("published %d messages with forcing off, want 0", got)
	}
}

// A failed publish must not be recorded as sent, or change detection
// would suppress the retry and the value would never arrive.
func TestFailedPublishIsNotRemembered(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	broker := &fakeBroker{fail: errBroker}
	p := newPublisher(broker, 0, clock.now, slog.New(slog.DiscardHandler))

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

	clock := newFakeClock()
	p := newPublisher(&fakeBroker{}, 0, clock.now, slog.New(slog.DiscardHandler))

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
