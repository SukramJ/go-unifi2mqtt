// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package state

import (
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

var (
	gw = model.MustParseMAC("00:00:5e:00:53:01")
	sw = model.MustParseMAC("00:00:5e:00:53:02")
	ap = model.MustParseMAC("00:00:5e:00:53:03")
	t0 = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
)

func devices() []model.Device {
	return []model.Device{
		{MAC: ap, Name: "AP", Type: model.DeviceAccessPoint, State: model.DeviceOnline},
		{MAC: gw, Name: "Gateway", Type: model.DeviceGateway, State: model.DeviceOnline},
		{MAC: sw, Name: "Switch", Type: model.DeviceSwitch, State: model.DeviceOnline},
	}
}

// The device list and the statistics arrive from different loops on
// different cadences. A device refresh must not wipe statistics that
// are still current, or the UI blinks empty every minute.
func TestDeviceRefreshKeepsStatistics(t *testing.T) {
	t.Parallel()

	s := New(t0)
	s.SetDevices(devices())
	s.SetDeviceStats(sw, model.DeviceStats{CPUPct: 12.5}, t0)

	// A second device poll, as the fast loop would do.
	s.SetDevices(devices())

	for _, d := range s.Snapshot().Devices {
		if d.MAC == sw {
			if d.Stats.CPUPct != 12.5 {
				t.Errorf("CPU = %v after a device refresh, want the statistics preserved", d.Stats.CPUPct)
			}
			if d.StatsSeen.IsZero() {
				t.Error("StatsSeen was cleared by a device refresh")
			}
			return
		}
	}
	t.Fatal("the switch disappeared from the snapshot")
}

// The statistics loop can finish just after a device left the site.
// Recording it would resurrect a device with no metadata at all.
func TestStatsForUnknownDeviceAreDropped(t *testing.T) {
	t.Parallel()

	s := New(t0)
	s.SetDeviceStats(gw, model.DeviceStats{CPUPct: 50}, t0)

	if got := len(s.Snapshot().Devices); got != 0 {
		t.Errorf("snapshot has %d devices after stats for an unknown MAC, want 0", got)
	}
}

// An unsorted map iteration would reshuffle the table on every refresh
// and make the page unreadable.
func TestSnapshotOrderIsStable(t *testing.T) {
	t.Parallel()

	s := New(t0)
	s.SetDevices(devices())

	var first []string
	for i := range 5 {
		snap := s.Snapshot()
		order := make([]string, 0, len(snap.Devices))
		for _, d := range snap.Devices {
			order = append(order, d.Name)
		}
		if i == 0 {
			first = order
			continue
		}
		for j := range order {
			if order[j] != first[j] {
				t.Fatalf("order changed between snapshots: %v vs %v", first, order)
			}
		}
	}

	// Gateways first, then switches, then access points — the order the
	// topology reads in.
	if first[0] != "Gateway" || first[1] != "Switch" || first[2] != "AP" {
		t.Errorf("order = %v, want gateway, switch, AP", first)
	}
}

// Handing out the live slice would let a handler observe a device
// mid-update while a poll rewrites it.
func TestSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	s := New(t0)
	s.SetDevices(devices())
	s.SetWLANs([]model.WLAN{{ID: "w1", Name: "Home"}})

	snap := s.Snapshot()
	snap.Devices[0].Name = "mutated"
	snap.WLANs[0].Name = "mutated"

	fresh := s.Snapshot()
	if fresh.Devices[0].Name == "mutated" {
		t.Error("mutating a snapshot changed the store's devices")
	}
	if fresh.WLANs[0].Name == "mutated" {
		t.Error("mutating a snapshot changed the store's WLANs")
	}
}

func TestHealthSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	s := New(t0)
	latency := 12
	s.SetHealth(model.Health{LatencyMs: &latency, WAN: model.SubsystemHealth{Status: "ok"}})

	snap := s.Snapshot()
	if snap.Health == nil {
		t.Fatal("no health in the snapshot")
	}
	snap.Health.WAN.Status = "mutated"

	if s.Snapshot().Health.WAN.Status == "mutated" {
		t.Error("mutating a snapshot changed the store's health")
	}
}

func TestClientLifecycle(t *testing.T) {
	t.Parallel()

	s := New(t0)
	c := model.Client{MAC: gw, Name: "Phone", IP: netip.MustParseAddr("192.0.2.1")}
	s.SetClient(c, true, t0)

	snap := s.Snapshot()
	if len(snap.Clients) != 1 || !snap.Clients[0].Home {
		t.Fatalf("clients = %+v, want one at home", snap.Clients)
	}

	// Away, then dropped entirely.
	s.SetClient(c, false, t0)
	if s.Snapshot().Clients[0].Home {
		t.Error("client still marked home after going away")
	}
	s.DropClients(map[string]bool{})
	if got := len(s.Snapshot().Clients); got != 0 {
		t.Errorf("%d clients left after dropping all", got)
	}
}

// A successful poll must clear the previous failure, or the UI keeps
// showing an error that resolved itself long ago.
func TestPollSuccessClearsTheError(t *testing.T) {
	t.Parallel()

	s := New(t0)
	s.PollFailed("devices", errors.New("connection refused"), t0)
	if got := len(s.Snapshot().LastErrors); got != 1 {
		t.Fatalf("errors = %d, want 1", got)
	}

	s.PollSucceeded("devices", t0.Add(time.Minute))
	snap := s.Snapshot()
	if got := len(snap.LastErrors); got != 0 {
		t.Errorf("errors = %d after a success, want 0", got)
	}
	if _, ok := snap.LastPoll["devices"]; !ok {
		t.Error("the successful poll was not recorded")
	}
}

// Poll loops write while HTTP handlers read; the race detector proves
// the mutex actually covers it.
func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	s := New(t0)
	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				s.SetDevices(devices())
				s.SetDeviceStats(sw, model.DeviceStats{CPUPct: float64(i)}, t0)
				s.SetClient(model.Client{MAC: ap, Name: "c"}, true, t0)
				s.PollSucceeded("devices", t0)
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				snap := s.Snapshot()
				_ = len(snap.Devices) + len(snap.Clients)
			}
		}()
	}
	wg.Wait()
}
