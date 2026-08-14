// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package state holds a snapshot of what the daemon last read from the
// console, so the optional web UI can render it without issuing its own
// API calls.
//
// It is only allocated when WEB_ENABLE is on. Without the UI the daemon
// keeps nothing beyond what the coordinator needs to publish, which is
// the point: a pure MQTT bridge should not carry a second copy of every
// device for nobody to look at.
package state

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Store is a thread-safe snapshot of the current site state.
//
// Written by the poll loops and read by HTTP handlers, so every access
// goes through the mutex. Readers get copies rather than references:
// handing out the live slice would let a handler observe a device
// mid-update while a poll rewrites it.
type Store struct {
	mu sync.RWMutex

	devices map[model.MAC]DeviceView
	clients map[string]ClientView
	wlans   []model.WLAN
	health  *model.Health
	site    model.Site
	info    model.ControllerInfo

	// startedAt stamps construction so the UI can show uptime.
	startedAt time.Time
	// lastPoll records when each loop last completed, which is the
	// fastest way to see that one has quietly stopped.
	lastPoll map[string]time.Time
	// lastError records the most recent failure per loop, so a
	// transient problem is visible without trawling the log.
	lastError map[string]LoopError

	// mqttConnected reflects the broker link.
	mqttConnected bool
	// capabilities lists the classic-layer features currently usable.
	capabilities map[string]bool
}

// DeviceView is a device plus its latest statistics.
type DeviceView struct {
	model.Device
	Stats     model.DeviceStats
	StatsSeen time.Time
}

// ClientView is a client plus its presence.
type ClientView struct {
	model.Client
	Home     bool
	LastSeen time.Time
}

// LoopError is the last failure of one poll loop.
type LoopError struct {
	Err  string    `json:"error"`
	At   time.Time `json:"at"`
	Loop string    `json:"loop"`
}

// New builds an empty Store.
func New(now time.Time) *Store {
	return &Store{
		devices:      make(map[model.MAC]DeviceView),
		clients:      make(map[string]ClientView),
		lastPoll:     make(map[string]time.Time),
		lastError:    make(map[string]LoopError),
		capabilities: make(map[string]bool),
		startedAt:    now,
	}
}

// SetSite records the site and controller metadata.
func (s *Store) SetSite(site model.Site, info model.ControllerInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.site = site
	s.info = info
}

// SetDevices replaces the device set, preserving the statistics already
// recorded for each MAC.
//
// The two arrive from different loops on different cadences, so a
// device refresh must not wipe statistics that are still current — that
// would make the UI blink empty every minute.
func (s *Store) SetDevices(devices []model.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[model.MAC]DeviceView, len(devices))
	for i := range devices {
		d := devices[i]
		if d.MAC.IsZero() {
			continue
		}
		view := DeviceView{Device: d}
		if prev, ok := s.devices[d.MAC]; ok {
			view.Stats = prev.Stats
			view.StatsSeen = prev.StatsSeen
		}
		next[d.MAC] = view
	}
	s.devices = next
}

// SetDeviceStats records one device's statistics.
func (s *Store) SetDeviceStats(mac model.MAC, stats model.DeviceStats, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	view, ok := s.devices[mac]
	if !ok {
		// The statistics loop can finish just after a device left the
		// site; recording it would resurrect a device with no metadata.
		return
	}
	view.Stats = stats
	view.StatsSeen = at
	s.devices[mac] = view
}

// SetClient records one client's presence and metadata.
func (s *Store) SetClient(c model.Client, home bool, lastSeen time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.Key()] = ClientView{Client: c, Home: home, LastSeen: lastSeen}
}

// DropClients removes clients no longer published.
func (s *Store) DropClients(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.clients {
		if !keep[key] {
			delete(s.clients, key)
		}
	}
}

// SetWLANs replaces the SSID catalogue.
func (s *Store) SetWLANs(wlans []model.WLAN) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wlans = slices.Clone(wlans)
}

// SetHealth records the site health aggregate.
func (s *Store) SetHealth(h model.Health) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health = &h
}

// SetMQTTConnected records the broker link state.
func (s *Store) SetMQTTConnected(connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mqttConnected = connected
}

// SetCapability records whether a classic-layer feature is usable.
func (s *Store) SetCapability(name string, available bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capabilities[name] = available
}

// PollSucceeded stamps a loop as having completed.
func (s *Store) PollSucceeded(loop string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPoll[loop] = at
	delete(s.lastError, loop)
}

// PollFailed records a loop failure.
func (s *Store) PollFailed(loop string, err error, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError[loop] = LoopError{Err: err.Error(), At: at, Loop: loop}
}

// Snapshot is an immutable view for rendering.
type Snapshot struct {
	Site          model.Site
	Info          model.ControllerInfo
	Devices       []DeviceView
	Clients       []ClientView
	WLANs         []model.WLAN
	Health        *model.Health
	StartedAt     time.Time
	LastPoll      map[string]time.Time
	LastErrors    []LoopError
	MQTTConnected bool
	Capabilities  map[string]bool
}

// Snapshot returns a copy of the current state.
//
// Sorted deterministically: an unsorted map iteration would reshuffle
// the table on every refresh and make the UI unreadable.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	devices := slices.SortedFunc(maps.Values(s.devices), compareDevices)
	clients := slices.SortedFunc(maps.Values(s.clients), func(a, b ClientView) int {
		if a.Name != b.Name {
			return compareStrings(a.Name, b.Name)
		}
		return compareStrings(a.Key(), b.Key())
	})
	errs := slices.SortedFunc(maps.Values(s.lastError), func(a, b LoopError) int {
		return compareStrings(a.Loop, b.Loop)
	})

	var health *model.Health
	if s.health != nil {
		h := *s.health
		health = &h
	}

	return Snapshot{
		Site:          s.site,
		Info:          s.info,
		Devices:       devices,
		Clients:       clients,
		WLANs:         slices.Clone(s.wlans),
		Health:        health,
		StartedAt:     s.startedAt,
		LastPoll:      maps.Clone(s.lastPoll),
		LastErrors:    errs,
		MQTTConnected: s.mqttConnected,
		Capabilities:  maps.Clone(s.capabilities),
	}
}

// compareDevices orders gateways first, then switches, then access
// points, and alphabetically within a kind — the order the topology
// reads in.
func compareDevices(a, b DeviceView) int {
	if r := deviceRank(a.Type) - deviceRank(b.Type); r != 0 {
		return r
	}
	return compareStrings(a.Name, b.Name)
}

func deviceRank(t model.DeviceType) int {
	switch t {
	case model.DeviceGateway:
		return 0
	case model.DeviceSwitch:
		return 1
	case model.DeviceAccessPoint:
		return 2
	default:
		return 3
	}
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
