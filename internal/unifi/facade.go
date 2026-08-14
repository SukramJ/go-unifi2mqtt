// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package unifi

import (
	"context"
	"log/slog"
	"sync"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Facade combines the two API flavours behind one contract, so nothing
// above it needs to know which surface answered a call
// (CONCEPT.md §3.3).
//
// The classic client is optional and may fail at runtime. When it does,
// the affected capabilities are switched off and the official path
// keeps running — degrading is the whole reason the split exists.

// Capability names one thing the daemon can do only with the classic
// layer.
type Capability string

// Capabilities.
const (
	// CapHealth is the site health aggregate (WAN status, latency,
	// throughput, client counts).
	CapHealth Capability = "health"
	// CapClientDetails is per-client SSID, signal strength, hostname and
	// the blocked flag.
	CapClientDetails Capability = "client_details"
	// CapPortPower is PoE draw in watts per port.
	CapPortPower Capability = "port_power"
	// CapClientBlock is blocking and unblocking clients.
	CapClientBlock Capability = "client_block"
	// CapDeviceLocate is the locate LED.
	CapDeviceLocate Capability = "device_locate"
	// CapWLANToggle is enabling and disabling an SSID.
	//
	//nolint:gosec // G101 flags this const group; these are capability
	// names, not credentials. The finding lands on the last line of the
	// group rather than on any particular entry.
	CapWLANToggle Capability = "wlan_toggle"
)

// integrationClient is the official API surface the facade delegates to.
type integrationClient interface {
	Info(ctx context.Context) (model.ControllerInfo, error)
	Devices(ctx context.Context, siteID string) ([]model.Device, error)
	DevicesWithDetails(ctx context.Context, siteID string) ([]model.Device, error)
	DeviceStats(ctx context.Context, siteID, deviceID string) (model.DeviceStats, error)
	Clients(ctx context.Context, siteID string) ([]model.Client, error)
	Networks(ctx context.Context, siteID string) ([]model.Network, error)
	WLANs(ctx context.Context, siteID string) ([]model.WLAN, error)
	RestartDevice(ctx context.Context, siteID, deviceID string) error
	PowerCyclePort(ctx context.Context, siteID, deviceID string, portIdx int) error
	AuthorizeGuest(ctx context.Context, siteID, clientID string, minutes int) error
}

// classicClient is the unofficial surface, present only when enabled.
type classicClient interface {
	Login(ctx context.Context) error
	Health(ctx context.Context, siteRef string) (model.Health, error)
	ClientDetails(ctx context.Context, siteRef string) (map[model.MAC]model.Client, error)
	PortPower(ctx context.Context, siteRef string) (map[model.MAC]map[int]float64, error)
	SetClientBlocked(ctx context.Context, siteRef string, mac model.MAC, blocked bool) error
	SetLocate(ctx context.Context, siteRef string, mac model.MAC, on bool) error
	SetWLANEnabled(ctx context.Context, siteRef, wlanID string, enabled bool) error
}

// Facade is the combined client.
type Facade struct {
	integration integrationClient
	classic     classicClient
	// siteRef is the classic API's site identifier ("default"), which
	// differs from the Integration API's UUID.
	siteRef string
	log     *slog.Logger

	// mu guards degraded. The classic layer can fail at any time, and
	// several poll loops read the capability set concurrently.
	mu sync.RWMutex
	// degraded records capabilities switched off after a failure, so a
	// broken classic layer stops being retried on every single poll.
	degraded map[Capability]bool
}

// FacadeConfig configures a [Facade].
type FacadeConfig struct {
	// Integration is the official client. Required.
	Integration integrationClient
	// Classic is the optional classic client; nil disables every
	// capability that needs it.
	Classic classicClient
	// SiteRef is the classic API's site identifier.
	SiteRef string
	// Logger receives diagnostics; nil uses slog.Default().
	Logger *slog.Logger
}

// NewFacade builds a Facade.
func NewFacade(cfg FacadeConfig) *Facade {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Facade{
		integration: cfg.Integration,
		classic:     cfg.Classic,
		siteRef:     cfg.SiteRef,
		log:         log,
		degraded:    make(map[Capability]bool),
	}
}

// Has reports whether a capability is currently available.
//
// The coordinator queries this before building discovery and never
// creates an entity it cannot serve — offering one and failing on click
// is worse than not offering it (CONCEPT.md §3.3).
func (f *Facade) Has(c Capability) bool {
	if f.classic == nil {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return !f.degraded[c]
}

// degrade switches a capability off after a failure and logs it once.
func (f *Facade) degrade(c Capability, err error) {
	f.mu.Lock()
	already := f.degraded[c]
	f.degraded[c] = true
	f.mu.Unlock()

	if !already {
		f.log.Warn("unifi.capability_degraded",
			slog.String("capability", string(c)),
			slog.String("err", err.Error()),
			slog.String("note", "the official API path continues unaffected"))
	}
}

// restore switches a capability back on after a success.
func (f *Facade) restore(c Capability) {
	f.mu.Lock()
	was := f.degraded[c]
	delete(f.degraded, c)
	f.mu.Unlock()

	if was {
		f.log.Info("unifi.capability_restored", slog.String("capability", string(c)))
	}
}

// --- official surface, delegated straight through ---

// Info returns the console's application version.
func (f *Facade) Info(ctx context.Context) (model.ControllerInfo, error) {
	return f.integration.Info(ctx)
}

// Devices lists the adopted devices.
func (f *Facade) Devices(ctx context.Context, siteID string) ([]model.Device, error) {
	return f.integration.Devices(ctx, siteID)
}

// DeviceStats fetches one device's latest statistics.
func (f *Facade) DeviceStats(ctx context.Context, siteID, deviceID string) (model.DeviceStats, error) {
	return f.integration.DeviceStats(ctx, siteID, deviceID)
}

// Networks returns the network/VLAN catalogue.
func (f *Facade) Networks(ctx context.Context, siteID string) ([]model.Network, error) {
	return f.integration.Networks(ctx, siteID)
}

// WLANs returns the SSID catalogue.
func (f *Facade) WLANs(ctx context.Context, siteID string) ([]model.WLAN, error) {
	return f.integration.WLANs(ctx, siteID)
}

// RestartDevice reboots a device.
func (f *Facade) RestartDevice(ctx context.Context, siteID, deviceID string) error {
	return f.integration.RestartDevice(ctx, siteID, deviceID)
}

// PowerCyclePort cuts and restores power on a PoE port.
func (f *Facade) PowerCyclePort(ctx context.Context, siteID, deviceID string, portIdx int) error {
	return f.integration.PowerCyclePort(ctx, siteID, deviceID, portIdx)
}

// AuthorizeGuest grants a guest client access.
func (f *Facade) AuthorizeGuest(ctx context.Context, siteID, clientID string, minutes int) error {
	return f.integration.AuthorizeGuest(ctx, siteID, clientID, minutes)
}

// --- combined surface ---

// DevicesWithDetails lists devices with ports, radios and uplinks,
// enriched with PoE power draw when the classic layer can supply it.
func (f *Facade) DevicesWithDetails(ctx context.Context, siteID string) ([]model.Device, error) {
	devices, err := f.integration.DevicesWithDetails(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if !f.Has(CapPortPower) {
		return devices, nil
	}

	power, err := f.classic.PortPower(ctx, f.siteRef)
	if err != nil {
		// A failing enrichment must not fail the poll: the devices are
		// fine, they just have no wattage.
		f.degrade(CapPortPower, err)
		return devices, nil
	}
	f.restore(CapPortPower)

	for i := range devices {
		byPort := power[devices[i].MAC]
		if byPort == nil {
			continue
		}
		for j := range devices[i].Ports {
			p := &devices[i].Ports[j]
			if p.PoE == nil {
				continue
			}
			if w, ok := byPort[p.Idx]; ok {
				p.PoE.PowerW = w
			}
		}
	}
	return devices, nil
}

// Clients lists the site's clients, enriched with SSID, signal
// strength, hostname and the blocked flag when the classic layer can
// supply them.
//
// The Integration API decides which clients exist; the classic layer
// only adds fields. That ordering matters: the official surface is the
// one with a schema, and a client missing from the classic response
// must not disappear.
func (f *Facade) Clients(ctx context.Context, siteID string) ([]model.Client, error) {
	clients, err := f.integration.Clients(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if !f.Has(CapClientDetails) {
		return clients, nil
	}

	details, err := f.classic.ClientDetails(ctx, f.siteRef)
	if err != nil {
		f.degrade(CapClientDetails, err)
		return clients, nil
	}
	f.restore(CapClientDetails)

	for i := range clients {
		d, ok := details[clients[i].MAC]
		if !ok {
			continue
		}
		c := &clients[i]
		c.SSID = d.SSID
		c.SignalDBm = d.SignalDBm
		c.Hostname = d.Hostname
		c.Blocked = d.Blocked
		c.LastSeen = d.LastSeen
		c.ClassicID = d.ClassicID
		// The classic VLAN is authoritative where present: it is what
		// the controller assigned, rather than what we inferred from the
		// client's IP.
		if d.VLAN != 0 {
			c.VLAN = d.VLAN
		}
		if d.Network != "" {
			c.Network = d.Network
		}
	}
	return clients, nil
}

// Health returns the site health aggregate, or
// [ErrCapabilityUnavailable] without the classic layer.
func (f *Facade) Health(ctx context.Context, _ string) (model.Health, error) {
	if !f.Has(CapHealth) {
		return model.Health{}, ErrCapabilityUnavailable
	}
	h, err := f.classic.Health(ctx, f.siteRef)
	if err != nil {
		f.degrade(CapHealth, err)
		return model.Health{}, err
	}
	f.restore(CapHealth)
	return h, nil
}

// SetClientBlocked blocks or unblocks a client.
func (f *Facade) SetClientBlocked(ctx context.Context, _ string, mac model.MAC, blocked bool) error {
	if !f.Has(CapClientBlock) {
		return ErrCapabilityUnavailable
	}
	return f.classic.SetClientBlocked(ctx, f.siteRef, mac, blocked)
}

// SetLocate turns a device's locate LED on or off.
func (f *Facade) SetLocate(ctx context.Context, _ string, mac model.MAC, on bool) error {
	if !f.Has(CapDeviceLocate) {
		return ErrCapabilityUnavailable
	}
	return f.classic.SetLocate(ctx, f.siteRef, mac, on)
}

// SetWLANEnabled enables or disables an SSID.
func (f *Facade) SetWLANEnabled(ctx context.Context, _, wlanID string, enabled bool) error {
	if !f.Has(CapWLANToggle) {
		return ErrCapabilityUnavailable
	}
	return f.classic.SetWLANEnabled(ctx, f.siteRef, wlanID, enabled)
}

// StartClassic logs the classic client in and reports whether the layer
// came up.
//
// A failure here is not fatal: it disables every classic capability and
// lets the daemon run on the official surface alone. That is the
// difference between "site health is missing" and "the bridge is down".
func (f *Facade) StartClassic(ctx context.Context) bool {
	if f.classic == nil {
		return false
	}
	if err := f.classic.Login(ctx); err != nil {
		f.log.Warn("unifi.classic_login_failed",
			slog.String("err", err.Error()),
			slog.String("note", "site health, per-client SSID/signal and PoE wattage stay unavailable"))
		for _, c := range []Capability{
			CapHealth, CapClientDetails, CapPortPower,
			CapClientBlock, CapDeviceLocate, CapWLANToggle,
		} {
			f.degrade(c, err)
		}
		return false
	}
	f.log.Info("unifi.classic_enabled")
	return true
}
