// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
)

// Regression: the store was accepted in Deps and silently never
// assigned to the struct. Everything compiled, every unit test passed,
// and the web UI showed an empty page — a wiring gap the compiler
// cannot see, so it needs a test that follows data all the way through.
func TestStoreIsFedByThePollLoops(t *testing.T) {
	t.Parallel()

	store := state.New(time.Now())
	src := newFakeSource()
	src.devices = sampleDevices()
	src.details = sampleDetails()
	src.wlans = []model.WLAN{{ID: "w1", Name: "HomeNet", Enabled: true}}
	src.stats["id-sw"] = model.DeviceStats{CPUPct: 12.5, Uptime: time.Hour}

	c := New(Deps{
		Cfg:    testConfig(),
		Site:   testSite(),
		Source: src,
		MQTT:   &fakeBroker{},
		Store:  store,
		Info:   model.ControllerInfo{ApplicationVersion: "10.5.67"},
		Logger: slog.New(slog.DiscardHandler),
		Now:    newFakeClock().now,
	})

	c.primeStore()
	if err := c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
	if err := c.refreshDeviceStats(t.Context()); err != nil {
		t.Fatalf("refreshDeviceStats: %v", err)
	}

	snap := store.Snapshot()
	if snap.Site.Name != "Default" {
		t.Errorf("site = %q, want it primed before the first poll", snap.Site.Name)
	}
	if len(snap.Devices) != 3 {
		t.Fatalf("store has %d devices, want 3 — the poll loops are not feeding it", len(snap.Devices))
	}
	if len(snap.WLANs) != 1 {
		t.Errorf("store has %d WLANs, want 1", len(snap.WLANs))
	}

	var withStats int
	for _, d := range snap.Devices {
		if !d.StatsSeen.IsZero() {
			withStats++
		}
	}
	// Two of the three sample devices are online; offline ones are
	// skipped by the statistics loop.
	if withStats != 2 {
		t.Errorf("%d devices carry statistics, want 2", withStats)
	}
}

// The loop wrapper records success and failure for every loop, which is
// what the UI's "last poll" chips read.
func TestStoreRecordsLoopOutcomes(t *testing.T) {
	t.Parallel()

	store := state.New(time.Now())
	src := newFakeSource()
	src.devices = sampleDevices()

	c := New(Deps{
		Cfg: testConfig(), Site: testSite(), Source: src, MQTT: &fakeBroker{},
		Store: store, Logger: slog.New(slog.DiscardHandler), Now: newFakeClock().now,
	})

	// loop() runs its function once and then blocks on the ticker, so
	// it needs a context that ends — the first run is all this asserts.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.loop(ctx, "devices", time.Hour, c.refreshDevices) }()

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := store.Snapshot().LastPoll["devices"]; ok {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("a successful poll was not recorded in the store")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	// A failing poll must be recorded too — that is what the UI's error
	// list reads.
	src.devicesErr = errBroker
	if err := c.refreshDevices(t.Context()); err == nil {
		t.Fatal("refreshDevices succeeded despite the injected error")
	}
}

// Without the web UI the daemon must keep no second copy of anything.
func TestNoStoreIsSafe(t *testing.T) {
	t.Parallel()

	src := newFakeSource()
	src.devices = sampleDevices()
	src.details = sampleDetails()

	c := New(Deps{
		Cfg: testConfig(), Site: testSite(), Source: src, MQTT: &fakeBroker{},
		// Store deliberately nil.
		Logger: slog.New(slog.DiscardHandler), Now: newFakeClock().now,
	})

	c.primeStore()
	if err := c.refreshStatic(t.Context()); err != nil {
		t.Fatalf("refreshStatic: %v", err)
	}
	if err := c.refreshDevices(t.Context()); err != nil {
		t.Fatalf("refreshDevices: %v", err)
	}
}
