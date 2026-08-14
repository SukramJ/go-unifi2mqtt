// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package unifi_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// stubIntegration is the official surface.
type stubIntegration struct {
	devices []model.Device
	clients []model.Client
}

func (s *stubIntegration) Info(context.Context) (model.ControllerInfo, error) {
	return model.ControllerInfo{ApplicationVersion: "10.5.67"}, nil
}

func (s *stubIntegration) Devices(context.Context, string) ([]model.Device, error) {
	return s.devices, nil
}

func (s *stubIntegration) DevicesWithDetails(context.Context, string) ([]model.Device, error) {
	out := make([]model.Device, len(s.devices))
	copy(out, s.devices)
	return out, nil
}

func (s *stubIntegration) DeviceStats(context.Context, string, string) (model.DeviceStats, error) {
	return model.DeviceStats{}, nil
}

func (s *stubIntegration) Clients(context.Context, string) ([]model.Client, error) {
	out := make([]model.Client, len(s.clients))
	copy(out, s.clients)
	return out, nil
}

func (s *stubIntegration) Networks(context.Context, string) ([]model.Network, error) { return nil, nil }

func (s *stubIntegration) WLANs(context.Context, string) ([]model.WLAN, error)       { return nil, nil }
func (s *stubIntegration) RestartDevice(context.Context, string, string) error       { return nil }
func (s *stubIntegration) PowerCyclePort(context.Context, string, string, int) error { return nil }
func (s *stubIntegration) AuthorizeGuest(context.Context, string, string, int) error { return nil }

// stubClassic is the optional surface, with switchable failures.
type stubClassic struct {
	loginErr   error
	healthErr  error
	detailsErr error
	powerErr   error

	details map[model.MAC]model.Client
	power   map[model.MAC]map[int]float64

	detailCalls atomic.Int32
}

func (s *stubClassic) Login(context.Context) error { return s.loginErr }

func (s *stubClassic) Health(context.Context, string) (model.Health, error) {
	if s.healthErr != nil {
		return model.Health{}, s.healthErr
	}
	latency := 12
	return model.Health{WAN: model.SubsystemHealth{Status: "ok"}, LatencyMs: &latency}, nil
}

func (s *stubClassic) ClientDetails(context.Context, string) (map[model.MAC]model.Client, error) {
	s.detailCalls.Add(1)
	if s.detailsErr != nil {
		return nil, s.detailsErr
	}
	return s.details, nil
}

func (s *stubClassic) PortPower(context.Context, string) (map[model.MAC]map[int]float64, error) {
	if s.powerErr != nil {
		return nil, s.powerErr
	}
	return s.power, nil
}

func (s *stubClassic) SetClientBlocked(context.Context, string, model.MAC, bool) error { return nil }

func (s *stubClassic) SetWLANEnabled(context.Context, string, string, bool) error { return nil }

func (s *stubClassic) SetLocate(context.Context, string, model.MAC, bool) error { return nil }

var (
	swMAC     = model.MustParseMAC("00:00:5e:00:53:02")
	phoneMAC  = model.MustParseMAC("00:00:5e:00:53:10")
	quietLogs = slog.New(slog.DiscardHandler)
)

func newFacade(t *testing.T, classic *stubClassic) (*unifi.Facade, *stubIntegration) {
	t.Helper()

	integration := &stubIntegration{
		devices: []model.Device{{
			MAC: swMAC, ID: "sw", Name: "Switch",
			Ports: []model.Port{
				{Idx: 1, PoE: &model.PoEState{Enabled: true}},
				{Idx: 2}, // no PoE hardware
			},
		}},
		clients: []model.Client{{MAC: phoneMAC, ID: "c1", Name: "Phone"}},
	}

	cfg := unifi.FacadeConfig{Integration: integration, SiteRef: "default", Logger: quietLogs}
	if classic != nil {
		cfg.Classic = classic
	}
	return unifi.NewFacade(cfg), integration
}

// Without the classic layer every capability that needs it must report
// unavailable, and the official path must keep working.
func TestWithoutClassicLayer(t *testing.T) {
	t.Parallel()

	f, _ := newFacade(t, nil)

	for _, c := range []unifi.Capability{
		unifi.CapHealth, unifi.CapClientDetails, unifi.CapPortPower,
		unifi.CapClientBlock, unifi.CapDeviceLocate,
	} {
		if f.Has(c) {
			t.Errorf("capability %s reported available with no classic client", c)
		}
	}

	if _, err := f.Health(t.Context(), "site"); !errors.Is(err, unifi.ErrCapabilityUnavailable) {
		t.Errorf("Health error = %v, want ErrCapabilityUnavailable", err)
	}
	if err := f.SetClientBlocked(t.Context(), "site", phoneMAC, true); !errors.Is(err, unifi.ErrCapabilityUnavailable) {
		t.Errorf("SetClientBlocked error = %v, want ErrCapabilityUnavailable", err)
	}

	// The official surface is unaffected.
	clients, err := f.Clients(t.Context(), "site")
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 1 {
		t.Errorf("got %d clients, want 1", len(clients))
	}
}

func TestClientEnrichment(t *testing.T) {
	t.Parallel()

	classic := &stubClassic{details: map[model.MAC]model.Client{
		phoneMAC: {SSID: "HomeNet", SignalDBm: -52, Hostname: "phone", Blocked: true, VLAN: 20, Network: "IoT"},
	}}
	f, _ := newFacade(t, classic)
	f.StartClassic(t.Context())

	clients, err := f.Clients(t.Context(), "site")
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	c := clients[0]
	if c.SSID != "HomeNet" || c.SignalDBm != -52 || c.Hostname != "phone" || !c.Blocked {
		t.Errorf("client not enriched: %+v", c)
	}
	// The controller's own VLAN assignment beats the one inferred from
	// the client's IP.
	if c.VLAN != 20 || c.Network != "IoT" {
		t.Errorf("network = %q/vlan %d, want IoT/20", c.Network, c.VLAN)
	}
}

// The official API decides which clients exist; the classic layer only
// adds fields. A client missing from the classic response must survive.
func TestClientMissingFromClassicResponseSurvives(t *testing.T) {
	t.Parallel()

	f, _ := newFacade(t, &stubClassic{details: map[model.MAC]model.Client{}})
	f.StartClassic(t.Context())

	clients, err := f.Clients(t.Context(), "site")
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "Phone" {
		t.Errorf("got %v, want the client from the official API", clients)
	}
	if clients[0].SSID != "" {
		t.Errorf("SSID = %q, want empty", clients[0].SSID)
	}
}

func TestPortPowerEnrichment(t *testing.T) {
	t.Parallel()

	classic := &stubClassic{power: map[model.MAC]map[int]float64{
		swMAC: {1: 7.4},
	}}
	f, _ := newFacade(t, classic)
	f.StartClassic(t.Context())

	devices, err := f.DevicesWithDetails(t.Context(), "site")
	if err != nil {
		t.Fatalf("DevicesWithDetails: %v", err)
	}
	if got := devices[0].Ports[0].PoE.PowerW; got != 7.4 {
		t.Errorf("port 1 PowerW = %v, want 7.4", got)
	}
	// A port with no PoE hardware must not gain a PoE block.
	if devices[0].Ports[1].PoE != nil {
		t.Error("a non-PoE port gained a PoE block from enrichment")
	}
}

// A failing enrichment must not fail the poll: the devices and clients
// are fine, they just lack the extra fields.
func TestEnrichmentFailureDegrades(t *testing.T) {
	t.Parallel()

	boom := errors.New("controller unreachable")
	classic := &stubClassic{detailsErr: boom, powerErr: boom}
	f, _ := newFacade(t, classic)
	f.StartClassic(t.Context())

	clients, err := f.Clients(t.Context(), "site")
	if err != nil {
		t.Fatalf("Clients returned %v, want the official data despite the failure", err)
	}
	if len(clients) != 1 {
		t.Errorf("got %d clients, want 1", len(clients))
	}
	if f.Has(unifi.CapClientDetails) {
		t.Error("the capability stayed available after a failure")
	}

	devices, err := f.DevicesWithDetails(t.Context(), "site")
	if err != nil {
		t.Fatalf("DevicesWithDetails returned %v, want the official data", err)
	}
	if len(devices) != 1 {
		t.Errorf("got %d devices, want 1", len(devices))
	}
}

// Once degraded, the failing call must not be retried on every single
// poll — that is the difference between a quiet degradation and a log
// full of the same error.
func TestDegradedCapabilityStopsBeingCalled(t *testing.T) {
	t.Parallel()

	classic := &stubClassic{detailsErr: errors.New("boom")}
	f, _ := newFacade(t, classic)
	f.StartClassic(t.Context())

	for range 5 {
		if _, err := f.Clients(t.Context(), "site"); err != nil {
			t.Fatalf("Clients: %v", err)
		}
	}
	if got := classic.detailCalls.Load(); got != 1 {
		t.Errorf("classic was called %d times after degrading, want 1", got)
	}
}

// A failed login disables everything classic but leaves the daemon
// running — that isolation is the reason the split exists.
func TestFailedLoginDegradesEverything(t *testing.T) {
	t.Parallel()

	f, _ := newFacade(t, &stubClassic{loginErr: errors.New("bad credentials")})
	if f.StartClassic(t.Context()) {
		t.Error("StartClassic reported success on a failed login")
	}

	for _, c := range []unifi.Capability{
		unifi.CapHealth, unifi.CapClientDetails, unifi.CapPortPower,
	} {
		if f.Has(c) {
			t.Errorf("capability %s survived a failed login", c)
		}
	}

	// ...and the official surface still answers.
	if _, err := f.Clients(t.Context(), "site"); err != nil {
		t.Errorf("Clients failed after a classic login failure: %v", err)
	}
}

func TestHealthPassesThrough(t *testing.T) {
	t.Parallel()

	f, _ := newFacade(t, &stubClassic{})
	f.StartClassic(t.Context())

	h, err := f.Health(t.Context(), "site")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.WAN.Status != "ok" || h.LatencyMs == nil || *h.LatencyMs != 12 {
		t.Errorf("health = %+v", h)
	}
}
