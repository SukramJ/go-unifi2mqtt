// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi/integration"
)

const (
	testAPIKey = "test-api-key"
	siteID     = "88f7af54-98f8-306a-a1c7-c9349722b1f6"
	// osPrefix is the UniFi OS layout every current console uses.
	osPrefix = "/proxy/network/integration"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fixture reads a golden response from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func newClient(t *testing.T, baseURL string, retries int) *integration.Client {
	t.Helper()

	tr, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: baseURL,
		Timeout: 5 * time.Second,
		Retries: retries,
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return integration.New(integration.Config{
		Transport: tr,
		APIKey:    testAPIKey,
		Logger:    quietLogger(),
	})
}

// serveFixtures starts a test console answering the given
// path→fixture mapping. It rejects any request without the API key, so
// a client that forgets the header fails loudly instead of appearing to
// work.
func serveFixtures(t *testing.T, routes map[string]string) *integration.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != testAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"statusName":"UNAUTHORIZED","message":"invalid api key"}`))
			return
		}
		name, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"statusName":"NOT_FOUND"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, name))
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	return c
}

func sitePath(suffix string) string { return osPrefix + "/v1/sites/" + siteID + suffix }

// networkRoutes wires the network list plus the per-network detail
// endpoints. The list carries no ipv4Configuration — that is only in
// the detail response, which is exactly why Networks() fans out.
func networkRoutes() map[string]string {
	return map[string]string{
		osPrefix + "/v1/info": "info.json",
		sitePath("/networks"): "networks.json",
		sitePath("/networks/n1111111-1111-4111-8111-111111111111"): "network_default.json",
		sitePath("/networks/n2222222-2222-4222-8222-222222222222"): "network_iot.json",
		sitePath("/networks/n3333333-3333-4333-8333-333333333333"): "network_legacy_vlan.json",
	}
}

func TestProbeFindsUniFiOSPrefix(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{osPrefix + "/v1/info": "info.json"})

	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got, want := info.ApplicationVersion, "10.5.67"; got != want {
		t.Errorf("ApplicationVersion = %q, want %q", got, want)
	}
}

// A standalone software controller serves the API without the
// /proxy/network prefix; the probe has to find it there too.
func TestProbeFallsBackToStandalonePrefix(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/integration/v1/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(fixture(t, "info.json"))
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	info, err := c.Probe(t.Context())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.ApplicationVersion != "10.5.67" {
		t.Errorf("ApplicationVersion = %q", info.ApplicationVersion)
	}
}

// A bad key must stop the probe at once. Trying the second prefix would
// produce a 401 there too and bury the real cause.
func TestProbeStopsOnUnauthorized(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"statusName":"UNAUTHORIZED"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := newClient(t, srv.URL, 0).Probe(t.Context())
	if !errors.Is(err, unifi.ErrUnauthorized) {
		t.Fatalf("Probe error = %v, want ErrUnauthorized", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("probe made %d requests, want 1 (it must not try the other prefix)", got)
	}
}

// A console too old for the Integration API 404s on both prefixes. The
// error has to say so rather than surfacing a bare 404.
func TestProbeReportsMissingAPI(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := newClient(t, srv.URL, 0).Probe(t.Context())
	if err == nil {
		t.Fatal("Probe succeeded against a console without the API")
	}
	if !containsAll(err.Error(), "no Integration API", "10.5") {
		t.Errorf("error = %v, want it to name the missing API and the required version", err)
	}
}

func TestSitesAndResolveSite(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info":  "info.json",
		osPrefix + "/v1/sites": "sites.json",
	})

	sites, err := c.Sites(t.Context())
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("got %d sites, want 1", len(sites))
	}
	if got, want := sites[0].Internal, "default"; got != want {
		t.Errorf("Internal = %q, want %q", got, want)
	}

	// All three spellings an operator might configure must resolve.
	for _, name := range []string{"default", "Default", siteID} {
		s, err := c.ResolveSite(t.Context(), name)
		if err != nil {
			t.Errorf("ResolveSite(%q): %v", name, err)
			continue
		}
		if s.ID != siteID {
			t.Errorf("ResolveSite(%q).ID = %q, want %q", name, s.ID, siteID)
		}
	}

	// An unknown site must list what is actually available, otherwise
	// the operator has no way to find the right value.
	_, err = c.ResolveSite(t.Context(), "nope")
	if err == nil {
		t.Fatal("ResolveSite succeeded for an unknown site")
	}
	if !containsAll(err.Error(), "nope", "default") {
		t.Errorf("error = %v, want it to name the wanted and the known sites", err)
	}
}

func TestDevices(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info": "info.json",
		sitePath("/devices"):  "devices.json",
	})

	devices, err := c.Devices(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("got %d devices, want 3", len(devices))
	}

	gw := devices[0]
	if got, want := gw.MAC, model.MustParseMAC("00:00:5E:00:53:01"); got != want {
		t.Errorf("gateway MAC = %q, want %q", got, want)
	}
	// UCG-Ultra reports only "switching", so the model prefix is what
	// makes it a gateway rather than a switch.
	if gw.Type != model.DeviceGateway {
		t.Errorf("gateway Type = %q, want %q", gw.Type, model.DeviceGateway)
	}
	if gw.IP != netip.MustParseAddr("192.0.2.1") {
		t.Errorf("gateway IP = %v", gw.IP)
	}

	sw := devices[1]
	if sw.Type != model.DeviceSwitch {
		t.Errorf("switch Type = %q, want %q", sw.Type, model.DeviceSwitch)
	}
	// The overview already carries firmware and the update flag, which
	// is why no per-device call is needed for the update sensor.
	if !sw.UpdateAvail {
		t.Error("switch UpdateAvail = false, want true")
	}
	if got, want := sw.Firmware, "7.0.25"; got != want {
		t.Errorf("switch Firmware = %q, want %q", got, want)
	}

	ap := devices[2]
	if ap.Type != model.DeviceAccessPoint {
		t.Errorf("AP Type = %q, want %q", ap.Type, model.DeviceAccessPoint)
	}
	if ap.State != model.DeviceOffline {
		t.Errorf("AP State = %q, want OFFLINE", ap.State)
	}
	if ap.State.IsOnline() {
		t.Error("an OFFLINE device reports IsOnline() = true")
	}
}

func TestDeviceDetails(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info": "info.json",
		sitePath("/devices/22222222-2222-4222-8222-222222222222"): "device_switch.json",
		sitePath("/devices/33333333-3333-4333-8333-333333333333"): "device_ap.json",
	})

	sw, err := c.Device(t.Context(), siteID, "22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatalf("Device: %v", err)
	}

	// The uplink is a UUID here — resolving it to a MAC is the
	// coordinator's job, so the client must not invent one.
	if got, want := sw.UplinkID, "11111111-1111-4111-8111-111111111111"; got != want {
		t.Errorf("UplinkID = %q, want %q", got, want)
	}
	if !sw.UplinkMAC.IsZero() {
		t.Errorf("UplinkMAC = %q, want zero (resolution happens later)", sw.UplinkMAC)
	}
	if got, want := sw.AdoptedAt.Format(time.RFC3339), "2026-01-15T08:30:00Z"; got != want {
		t.Errorf("AdoptedAt = %q, want %q", got, want)
	}

	if len(sw.Ports) != 3 {
		t.Fatalf("got %d ports, want 3", len(sw.Ports))
	}
	p1 := sw.Ports[0]
	if p1.State != model.PortUp || p1.SpeedMbps != 1000 {
		t.Errorf("port 1 = %+v, want UP at 1000 Mbps", p1)
	}
	if p1.PoE == nil || !p1.PoE.Enabled || p1.PoE.Standard != "802.3at" {
		t.Errorf("port 1 PoE = %+v, want enabled 802.3at", p1.PoE)
	}
	// The Integration API has no power field, so this stays zero even
	// on a port actively delivering power.
	if p1.PoE != nil && p1.PoE.PowerW != 0 {
		t.Errorf("port 1 PoE.PowerW = %v, want 0 (classic layer only)", p1.PoE.PowerW)
	}
	// An SFP+ uplink port has no PoE at all — nil, not a disabled PoE.
	if sw.Ports[2].PoE != nil {
		t.Errorf("SFP+ port has PoE = %+v, want nil", sw.Ports[2].PoE)
	}
	// A port that is down reports no speed; the field must stay zero
	// rather than inheriting maxSpeedMbps.
	if got := sw.Ports[1].SpeedMbps; got != 0 {
		t.Errorf("down port SpeedMbps = %d, want 0", got)
	}

	ap, err := c.Device(t.Context(), siteID, "33333333-3333-4333-8333-333333333333")
	if err != nil {
		t.Fatalf("Device(AP): %v", err)
	}
	if ap.Type != model.DeviceAccessPoint {
		t.Errorf("AP Type = %q, want %q", ap.Type, model.DeviceAccessPoint)
	}
	if len(ap.Radios) != 2 {
		t.Fatalf("got %d radios, want 2", len(ap.Radios))
	}
	if got, want := ap.Radios[1].FrequencyGHz, 5.0; got != want {
		t.Errorf("radio[1] FrequencyGHz = %v, want %v", got, want)
	}
	if got, want := ap.Radios[1].Channel, 36; got != want {
		t.Errorf("radio[1] Channel = %d, want %d", got, want)
	}
}

func TestDeviceStats(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info": "info.json",
		sitePath("/devices/22222222-2222-4222-8222-222222222222/statistics/latest"): "device_stats.json",
	})

	s, err := c.DeviceStats(t.Context(), siteID, "22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatalf("DeviceStats: %v", err)
	}

	if got, want := s.Uptime, 864000*time.Second; got != want {
		t.Errorf("Uptime = %v, want %v", got, want)
	}
	if got, want := s.CPUPct, 12.5; got != want {
		t.Errorf("CPUPct = %v, want %v", got, want)
	}
	if got, want := s.MemoryPct, 48.0; got != want {
		t.Errorf("MemoryPct = %v, want %v", got, want)
	}
	if got, want := s.UplinkTxBps, uint64(1048576); got != want {
		t.Errorf("UplinkTxBps = %d, want %d", got, want)
	}
	// Radio statistics are keyed by frequency because the response
	// carries no other radio identifier.
	if got, want := s.RadioTxRetry[5], 1.2; got != want {
		t.Errorf("RadioTxRetry[5] = %v, want %v", got, want)
	}
	if got, want := s.RadioTxRetry[2.4], 3.1; got != want {
		t.Errorf("RadioTxRetry[2.4] = %v, want %v", got, want)
	}
}

func TestClients(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info": "info.json",
		sitePath("/clients"):  "clients.json",
	})

	clients, err := c.Clients(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 4 {
		t.Fatalf("got %d clients, want 4", len(clients))
	}

	phone := clients[0]
	if phone.Type != model.ClientWireless {
		t.Errorf("phone Type = %q", phone.Type)
	}
	if got, want := phone.MAC, model.MustParseMAC("00:00:5E:00:53:10"); got != want {
		t.Errorf("phone MAC = %q, want %q", got, want)
	}
	if phone.IsGuest {
		t.Error("phone IsGuest = true, want false")
	}
	if got, want := phone.Key(), "00005e005310"; got != want {
		t.Errorf("phone Key() = %q, want the MAC %q", got, want)
	}

	guest := clients[2]
	if !guest.IsGuest || !guest.Authorized {
		t.Errorf("guest IsGuest=%v Authorized=%v, want both true", guest.IsGuest, guest.Authorized)
	}

	// A VPN client has no MAC at all; it must decode cleanly and fall
	// back to its UUID as the identity.
	vpn := clients[3]
	if vpn.Type != model.ClientVPN {
		t.Errorf("vpn Type = %q, want VPN", vpn.Type)
	}
	if !vpn.MAC.IsZero() {
		t.Errorf("vpn MAC = %q, want zero", vpn.MAC)
	}
	if got, want := vpn.Key(), "dddddddd-dddd-4ddd-8ddd-dddddddddddd"; got != want {
		t.Errorf("vpn Key() = %q, want the UUID %q", got, want)
	}

	// The Integration API reports none of these; they must stay empty
	// rather than being invented.
	if phone.SSID != "" || phone.SignalDBm != 0 || phone.Hostname != "" {
		t.Errorf("client carries classic-only fields: SSID=%q Signal=%d Hostname=%q",
			phone.SSID, phone.SignalDBm, phone.Hostname)
	}
}

func TestNetworks(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, networkRoutes())

	networks, err := c.Networks(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if len(networks) != 3 {
		t.Fatalf("got %d networks, want 3", len(networks))
	}

	def := networks[0]
	if !def.Default || def.VLAN != 1 {
		t.Errorf("default network = %+v, want default with VLAN 1", def)
	}
	// hostIpAddress is the gateway's address inside the subnet, so the
	// prefix has to be masked to be usable with Contains.
	if got, want := def.Subnets[0].String(), "192.0.2.0/24"; got != want {
		t.Errorf("default subnet = %q, want the masked %q", got, want)
	}

	iot := networks[1]
	if len(iot.Subnets) != 2 {
		t.Fatalf("IoT has %d subnets, want 2 (primary + additional)", len(iot.Subnets))
	}
	if got, want := iot.Subnets[1].String(), "203.0.113.0/28"; got != want {
		t.Errorf("IoT additional subnet = %q, want %q", got, want)
	}

	// UNMANAGED networks carry no IP configuration at all.
	if got := networks[2].Subnets; len(got) != 0 {
		t.Errorf("unmanaged network has subnets %v, want none", got)
	}
	if networks[2].Management != model.NetworkUnmanaged {
		t.Errorf("Management = %q, want UNMANAGED", networks[2].Management)
	}
}

// The whole point of loading the network catalogue: turning a client's
// IP into the VLAN a filter can match on, with no classic API involved.
func TestNetworksDriveClientVLANMapping(t *testing.T) {
	t.Parallel()

	routes := networkRoutes()
	routes[sitePath("/clients")] = "clients.json"
	c := serveFixtures(t, routes)

	networks, err := c.Networks(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	clients, err := c.Clients(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}

	want := map[string]int{
		"Phone":        1,  // 192.0.2.42   -> Default
		"NAS":          1,  // 192.0.2.50   -> Default
		"Guest Laptop": 20, // 198.51.100.20 -> IoT
	}
	for _, cl := range clients {
		wantVLAN, checked := want[cl.Name]
		n, ok := model.ResolveNetwork(cl.IP, networks)
		if !checked {
			continue
		}
		if !ok {
			t.Errorf("%s (%v) mapped to no network", cl.Name, cl.IP)
			continue
		}
		if n.VLAN != wantVLAN {
			t.Errorf("%s (%v) mapped to VLAN %d, want %d", cl.Name, cl.IP, n.VLAN, wantVLAN)
		}
	}
}

func TestWLANs(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info":        "info.json",
		sitePath("/wifi/broadcasts"): "wlans.json",
	})

	wlans, err := c.WLANs(t.Context(), siteID)
	if err != nil {
		t.Fatalf("WLANs: %v", err)
	}
	if len(wlans) != 2 {
		t.Fatalf("got %d WLANs, want 2", len(wlans))
	}

	// A NATIVE network reference carries no id — the WLAN just uses the
	// site default, and inventing an id here would break the join.
	if got := wlans[0].NetworkID; got != "" {
		t.Errorf("native WLAN NetworkID = %q, want empty", got)
	}
	if !wlans[0].Enabled {
		t.Error("HomeNet Enabled = false, want true")
	}
	if got, want := wlans[1].NetworkID, "n2222222-2222-4222-8222-222222222222"; got != want {
		t.Errorf("specific WLAN NetworkID = %q, want %q", got, want)
	}
	if wlans[1].Enabled {
		t.Error("IoT-WiFi Enabled = true, want false")
	}
}

// Pagination has to follow totalCount rather than assuming a short page
// means the end — a page can come back short while more results remain.
func TestPaginationWalksEveryPage(t *testing.T) {
	t.Parallel()

	const total = 450 // more than two full pages of 200

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == osPrefix+"/v1/info" {
			_, _ = w.Write(fixture(t, "info.json"))
			return
		}
		requests.Add(1)

		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			t.Error("request carried no limit")
			limit = 200
		}

		var body strings.Builder
		body.WriteString(`{"offset":` + strconv.Itoa(offset) +
			`,"limit":` + strconv.Itoa(limit) +
			`,"totalCount":` + strconv.Itoa(total) + `,"data":[`)
		n := 0
		for i := offset; i < offset+limit && i < total; i++ {
			if n > 0 {
				body.WriteString(",")
			}
			body.WriteString(`{"type":"WIRED","id":"id-` + strconv.Itoa(i) +
				`","name":"c` + strconv.Itoa(i) + `","access":{"type":"DEFAULT"}}`)
			n++
		}
		body.WriteString(`],"count":` + strconv.Itoa(n) + `}`)
		_, _ = io.WriteString(w, body.String())
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	clients, err := c.Clients(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != total {
		t.Errorf("got %d clients, want %d", len(clients), total)
	}
	if got, want := requests.Load(), int32(3); got != want {
		t.Errorf("made %d list requests, want %d (200+200+50)", got, want)
	}
	if clients[total-1].ID != "id-449" {
		t.Errorf("last client = %q, want id-449", clients[total-1].ID)
	}
}

// A console that reports a totalCount it never delivers must not spin
// forever; an empty page ends the walk.
func TestPaginationStopsOnEmptyPage(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == osPrefix+"/v1/info" {
			_, _ = w.Write(fixture(t, "info.json"))
			return
		}
		requests.Add(1)
		// Claims 9999 results but always returns an empty page.
		_, _ = io.WriteString(w, `{"offset":0,"limit":200,"count":0,"totalCount":9999,"data":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	clients, err := c.Clients(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(clients) != 0 {
		t.Errorf("got %d clients, want 0", len(clients))
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("made %d requests, want 1 — an empty page must end the walk", got)
	}
}

func TestActuators(t *testing.T) {
	t.Parallel()

	type call struct {
		path string
		body string
	}
	got := make(chan call, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == osPrefix+"/v1/info" {
			_, _ = w.Write(fixture(t, "info.json"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		got <- call{path: r.URL.Path, body: string(body)}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	tests := []struct {
		name     string
		run      func() error
		wantPath string
		wantBody string
	}{
		{
			name:     "restart",
			run:      func() error { return c.RestartDevice(t.Context(), siteID, "dev-1") },
			wantPath: sitePath("/devices/dev-1/actions"),
			wantBody: `{"action":"RESTART"}`,
		},
		{
			name:     "power cycle",
			run:      func() error { return c.PowerCyclePort(t.Context(), siteID, "dev-1", 7) },
			wantPath: sitePath("/devices/dev-1/interfaces/ports/7/actions"),
			wantBody: `{"action":"POWER_CYCLE"}`,
		},
		{
			// No time limit means "use the site default", so the field
			// must be omitted rather than sent as 0.
			name:     "authorize guest without limit",
			run:      func() error { return c.AuthorizeGuest(t.Context(), siteID, "cl-1", 0) },
			wantPath: sitePath("/clients/cl-1/actions"),
			wantBody: `{"action":"AUTHORIZE_GUEST_ACCESS"}`,
		},
		{
			name:     "authorize guest with limit",
			run:      func() error { return c.AuthorizeGuest(t.Context(), siteID, "cl-1", 60) },
			wantPath: sitePath("/clients/cl-1/actions"),
			wantBody: `{"action":"AUTHORIZE_GUEST_ACCESS","timeLimitMinutes":60}`,
		},
	}

	for _, tt := range tests {
		if err := tt.run(); err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		c := <-got
		if c.path != tt.wantPath {
			t.Errorf("%s path = %q, want %q", tt.name, c.path, tt.wantPath)
		}
		if c.body != tt.wantBody {
			t.Errorf("%s body = %s, want %s", tt.name, c.body, tt.wantBody)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, unifi.ErrUnauthorized},
		{"forbidden", http.StatusForbidden, unifi.ErrForbidden},
		{"not found", http.StatusNotFound, unifi.ErrNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == osPrefix+"/v1/info" {
					_, _ = w.Write(fixture(t, "info.json"))
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"statusName":"ERR","message":"nope"}`))
			}))
			t.Cleanup(srv.Close)

			c := newClient(t, srv.URL, 0)
			if _, err := c.Probe(t.Context()); err != nil {
				t.Fatalf("Probe: %v", err)
			}

			_, err := c.Devices(t.Context(), siteID)
			if !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}

			// The status and path belong in the message so a log line is
			// actionable without turning on debug.
			var apiErr *unifi.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not an *APIError: %v", err)
			}
			if apiErr.Status != tt.status {
				t.Errorf("APIError.Status = %d, want %d", apiErr.Status, tt.status)
			}
		})
	}
}

// The API key travels in a header; it must never end up in an error
// string, which is where it would leak into logs.
func TestErrorsCarryNoAPIKey(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == osPrefix+"/v1/info" {
			_, _ = w.Write(fixture(t, "info.json"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"statusName":"BOOM"}`))
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	_, err := c.Devices(t.Context(), siteID)
	if err == nil {
		t.Fatal("Devices succeeded against a failing console")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("error leaked the API key: %v", err)
	}
}

func TestContextCancellationAborts(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == osPrefix+"/v1/info" {
			_, _ = w.Write(fixture(t, "info.json"))
			return
		}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := newClient(t, srv.URL, 3)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := c.Devices(ctx, siteID); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// Regression: the network list endpoint carries no ipv4Configuration.
// Reading only the list leaves every network without subnets, which
// makes client→VLAN mapping silently resolve to nothing — the failure
// mode this fan-out exists to prevent.
func TestNetworksFetchesSubnetsFromDetailEndpoint(t *testing.T) {
	t.Parallel()

	var detailCalls atomic.Int32
	routes := networkRoutes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/networks/") {
			detailCalls.Add(1)
		}
		name, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(fixture(t, name))
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	networks, err := c.Networks(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if got, want := detailCalls.Load(), int32(3); got != want {
		t.Errorf("made %d detail calls, want %d (one per network)", got, want)
	}

	var withSubnets int
	for _, n := range networks {
		if len(n.Subnets) > 0 {
			withSubnets++
		}
	}
	if withSubnets == 0 {
		t.Fatal("no network came back with subnets — the detail call is not being made")
	}
}

// A network whose detail call fails keeps its overview data and loses
// only its subnets. Dropping the whole catalogue over one transient
// failure would take every client's VLAN with it.
func TestNetworkDetailFailureDegradesGracefully(t *testing.T) {
	t.Parallel()

	routes := networkRoutes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail exactly one network's detail call.
		if strings.HasSuffix(r.URL.Path, "n2222222-2222-4222-8222-222222222222") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		name, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(fixture(t, name))
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	networks, err := c.Networks(t.Context(), siteID)
	if err != nil {
		t.Fatalf("Networks: %v", err)
	}
	if len(networks) != 3 {
		t.Fatalf("got %d networks, want all 3 despite one failing detail call", len(networks))
	}

	byName := map[string]model.Network{}
	for _, n := range networks {
		byName[n.Name] = n
	}
	// The failed one keeps its identity, just not its subnets.
	iot := byName["IoT"]
	if iot.VLAN != 20 {
		t.Errorf("IoT VLAN = %d, want 20 from the overview", iot.VLAN)
	}
	if len(iot.Subnets) != 0 {
		t.Errorf("IoT has subnets %v, want none after the failed detail call", iot.Subnets)
	}
	// Its neighbours are unaffected.
	if len(byName["Default"].Subnets) == 0 {
		t.Error("Default lost its subnets because a different network failed")
	}
}

// Regression: the device list carries neither uplink nor interfaces.
// Without the per-device follow-up every device reports no uplink and
// Home Assistant's via_device topology collapses into a flat list.
func TestDevicesWithDetailsResolvesUplinks(t *testing.T) {
	t.Parallel()

	c := serveFixtures(t, map[string]string{
		osPrefix + "/v1/info": "info.json",
		sitePath("/devices"):  "devices.json",
		sitePath("/devices/11111111-1111-4111-8111-111111111111"): "device_gateway.json",
		sitePath("/devices/22222222-2222-4222-8222-222222222222"): "device_switch.json",
		sitePath("/devices/33333333-3333-4333-8333-333333333333"): "device_ap.json",
	})

	devices, err := c.DevicesWithDetails(t.Context(), siteID)
	if err != nil {
		t.Fatalf("DevicesWithDetails: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("got %d devices, want 3", len(devices))
	}

	byMAC := map[model.MAC]model.Device{}
	for _, d := range devices {
		byMAC[d.MAC] = d
	}
	gw := model.MustParseMAC("00:00:5E:00:53:01")
	sw := model.MustParseMAC("00:00:5E:00:53:02")
	ap := model.MustParseMAC("00:00:5E:00:53:03")

	if got := byMAC[gw].UplinkMAC; !got.IsZero() {
		t.Errorf("gateway UplinkMAC = %q, want zero", got)
	}
	if got := byMAC[sw].UplinkMAC; got != gw {
		t.Errorf("switch UplinkMAC = %q, want the gateway %q", got, gw)
	}
	if got := byMAC[ap].UplinkMAC; got != sw {
		t.Errorf("AP UplinkMAC = %q, want the switch %q", got, sw)
	}

	// The details also bring in the parts the list omits entirely.
	if len(byMAC[sw].Ports) == 0 {
		t.Error("switch has no ports — the detail response was not merged")
	}
	if len(byMAC[ap].Radios) == 0 {
		t.Error("AP has no radios — the detail response was not merged")
	}
	// ...while overview-only facts survive the merge.
	if !byMAC[sw].UpdateAvail {
		t.Error("switch lost UpdateAvail, which only the overview reports")
	}
}

// A device whose detail call fails keeps its overview data, so the
// state, firmware and update sensors survive.
func TestDeviceDetailFailureKeepsOverview(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == osPrefix+"/v1/info":
			_, _ = w.Write(fixture(t, "info.json"))
		case r.URL.Path == sitePath("/devices"):
			_, _ = w.Write(fixture(t, "devices.json"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	c := newClient(t, srv.URL, 0)
	if _, err := c.Probe(t.Context()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	devices, err := c.DevicesWithDetails(t.Context(), siteID)
	if err != nil {
		t.Fatalf("DevicesWithDetails: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("got %d devices, want 3 despite every detail call failing", len(devices))
	}
	if !devices[1].UpdateAvail || devices[1].Firmware != "7.0.25" {
		t.Error("overview data was lost when the detail call failed")
	}
}
