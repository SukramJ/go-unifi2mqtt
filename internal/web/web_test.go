// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
)

func newServer(t *testing.T, cfg *config.Config) (h http.Handler, store *state.Store) {
	t.Helper()
	store = state.New(time.Now().Add(-time.Hour))
	return New(cfg, store, slog.New(slog.DiscardHandler)).Handler(), store
}

// do issues a request through the full handler chain, auth included.
func do(t *testing.T, h http.Handler, path string, auth ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	if len(auth) == 2 {
		req.SetBasicAuth(auth[0], auth[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func table(t *testing.T, d *bridgeui.Device, id string) *bridgeui.Table {
	t.Helper()
	for i := range d.Tables {
		if d.Tables[i].ID == id {
			return &d.Tables[i]
		}
	}
	t.Fatalf("no table %q in %+v", id, d.Tables)
	return nil
}

func TestServesTheDashboard(t *testing.T) {
	t.Parallel()

	h, _ := newServer(t, &config.Config{})
	for _, path := range []string{"/", "/app.css", "/app.js", "/api/ui", "/i18n/de.json"} {
		if rec := do(t, h, path); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s -> %d", path, rec.Code)
		}
	}
}

// Every label the bridge adds exists in both languages.
func TestBundlesMatch(t *testing.T) {
	t.Parallel()

	tr, err := bridgeui.LoadTranslations(i18nFS, "i18n")
	if err != nil {
		t.Fatal(err)
	}
	for k := range tr["de"] {
		if _, ok := tr["en"][k]; !ok {
			t.Errorf("en lacks %s", k)
		}
	}
	for k := range tr["en"] {
		if _, ok := tr["de"][k]; !ok {
			t.Errorf("de lacks %s", k)
		}
	}
	if _, err := fs.Stat(i18nFS, "i18n/de.json"); err != nil {
		t.Error(err)
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()

	h, store := newServer(t, &config.Config{Language: "de", Clients: config.ClientsConfig{Enable: true}})
	store.SetSite(
		model.Site{Name: "Default", Internal: "default"},
		model.ControllerInfo{ApplicationVersion: "10.5.67"},
	)
	store.SetMQTTConnected(true)
	sw := model.MustParseMAC("00:00:5e:00:53:02")
	store.SetDevices([]model.Device{
		{
			MAC: sw, Name: "Switch", Model: "USW Pro Max 16 PoE", Type: model.DeviceSwitch,
			State: model.DeviceOnline, IP: netip.MustParseAddr("192.0.2.2"),
			Ports: []model.Port{{Idx: 1, PoE: &model.PoEState{Enabled: true, PowerW: 7.4}}, {Idx: 2}},
		},
		{MAC: model.MustParseMAC("00:00:5e:00:53:03"), Name: "AP", State: model.DeviceOffline},
	})
	store.SetDeviceStats(sw, model.DeviceStats{CPUPct: 12.5, MemoryPct: 48, Uptime: time.Hour}, time.Now())
	store.SetClient(model.Client{MAC: model.MustParseMAC("00:00:5e:00:53:10"), Name: "Phone", Type: model.ClientWireless, SSID: "home", SignalDBm: -55}, true, time.Now())
	store.SetClient(model.Client{MAC: model.MustParseMAC("00:00:5e:00:53:11"), Name: "Laptop", Type: model.ClientWired}, false, time.Now().Add(-time.Hour))
	store.SetWLANs([]model.WLAN{{ID: "w1", Name: "home", Enabled: true}})
	store.PollSucceeded("devices", time.Now())

	snap := decode[bridgeui.Snapshot](t, do(t, h, "/api/snapshot"))
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %+v, want the site", snap.Devices)
	}
	site := &snap.Devices[0]
	if site.ID != "default" || site.Title != "Default" || site.Subtitle != "UniFi Network 10.5.67" {
		t.Errorf("site = %q %q %q", site.ID, site.Title, site.Subtitle)
	}

	devs := table(t, site, tableDevices)
	if len(devs.Rows) != 2 || len(devs.Rows[0].Cells) != len(deviceColumns) {
		t.Fatalf("device rows = %+v", devs.Rows)
	}
	// Sorted: the switch first. PoE is summed over the PoE ports.
	if c := devs.Rows[0].Cells; c[0] != "Switch" || c[2] != "state.ONLINE" || c[6] != 12.5 || c[10] != 7.4 || devs.Rows[0].Tone != "ok" {
		t.Errorf("switch row = %+v", devs.Rows[0])
	}
	// A device whose statistics never arrived shows none, rather than
	// reading as idle at 0 %.
	if c := devs.Rows[1].Cells; c[5] != nil || c[6] != nil || devs.Rows[1].Tone != "err" {
		t.Errorf("AP row = %+v", devs.Rows[1])
	}

	clients := table(t, site, tableClients)
	if len(clients.Rows) != 2 {
		t.Fatalf("client rows = %+v", clients.Rows)
	}
	away := clients.Rows[0] // Laptop sorts first
	if away.Cells[2] != "val.away" || away.Tone != "muted" || away.Cells[3].(float64) < 3500 {
		t.Errorf("away client = %+v", away)
	}
	if home := clients.Rows[1]; home.Cells[2] != "val.home" || home.Cells[3] != nil || home.Cells[7] != float64(-55) {
		t.Errorf("home client = %+v", home)
	}

	var wlan bool
	for _, e := range site.Entries {
		if e.Group == groupWLANs && e.Name == "home" && e.Value == true {
			wlan = true
		}
	}
	if !wlan {
		t.Errorf("WLAN entry missing: %+v", site.Entries)
	}
}

// With client publication off there is no client table, and the
// optional section hides.
func TestClientsOff(t *testing.T) {
	t.Parallel()

	h, _ := newServer(t, &config.Config{})
	snap := decode[bridgeui.Snapshot](t, do(t, h, "/api/snapshot"))
	for _, tb := range snap.Devices[0].Tables {
		if tb.ID == tableClients {
			t.Error("client table served with CLIENTS.ENABLE off")
		}
	}
}

// A loop that failed before it ever succeeded has no timestamp.
// Omitting it would hide exactly the thing worth seeing.
func TestLoopThatNeverSucceededIsStillListed(t *testing.T) {
	t.Parallel()

	h, store := newServer(t, &config.Config{})
	store.SetMQTTConnected(true)
	store.PollFailed("health", errors.New("unreachable"), time.Now())
	store.SetCapability("client_block", false)

	health := decode[bridgeui.Health](t, do(t, h, "/api/health"))
	if health.Status != "degraded" {
		t.Errorf("status = %q, want degraded", health.Status)
	}
	var loop *bridgeui.Tile
	for i := range health.Tiles {
		if health.Tiles[i].Label == "loop.health" {
			loop = &health.Tiles[i]
		}
	}
	if loop == nil || loop.Text != "val.never" || loop.Tone != "err" {
		t.Errorf("loop tile = %+v in %+v", loop, health.Tiles)
	}
	if len(health.Problems) != 2 || health.Problems[0].Detail != "unreachable" || health.Problems[1].Detail != "client_block" {
		t.Errorf("problems = %+v", health.Problems)
	}
}

// The read-only UI writes nothing.
func TestReadOnly(t *testing.T) {
	t.Parallel()

	h, _ := newServer(t, &config.Config{})
	if body := do(t, h, "/api/items").Body.String(); strings.TrimSpace(body) != "[]" {
		t.Errorf("items = %s", body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/write", strings.NewReader(`{"key":"x","value":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("write -> %d, want 400", rec.Code)
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()

	h, _ := newServer(t, &config.Config{WebUser: "admin", WebPassword: "secret"})
	if got := do(t, h, "/api/status").Code; got != http.StatusUnauthorized {
		t.Errorf("without credentials -> %d, want 401", got)
	}
	if got := do(t, h, "/api/status", "admin", "wrong").Code; got != http.StatusUnauthorized {
		t.Errorf("with a wrong password -> %d, want 401", got)
	}
	if got := do(t, h, "/api/status", "admin", "secret").Code; got != http.StatusOK {
		t.Errorf("with correct credentials -> %d, want 200", got)
	}
	// The page itself is behind the same gate.
	if got := do(t, h, "/").Code; got != http.StatusUnauthorized {
		t.Errorf("the page is served without auth -> %d, want 401", got)
	}
}

// A password alone makes auth active too: a config with a password and
// no user must not silently serve everything.
func TestAuthEnabledByEitherField(t *testing.T) {
	t.Parallel()

	h, _ := newServer(t, &config.Config{WebPassword: "secret"})
	if got := do(t, h, "/api/status").Code; got != http.StatusUnauthorized {
		t.Errorf("password-only config served without auth -> %d, want 401", got)
	}
}

// The liveness probe deliberately reports only that the process serves.
// Failing it when the console is unreachable would produce a crash loop
// that cannot fix the console.
func TestHealthEndpointIsProcessLevel(t *testing.T) {
	t.Parallel()

	h, store := newServer(t, &config.Config{})
	store.PollFailed("devices", errors.New("console unreachable"), time.Now())
	store.SetMQTTConnected(false)
	if rec := do(t, h, "/api/health"); rec.Code != http.StatusOK {
		t.Errorf("status = %d with everything broken, want 200", rec.Code)
	}
}

// The configuration shown never carries a secret.
func TestConfigHidesSecrets(t *testing.T) {
	t.Parallel()

	h, _ := newServer(t, &config.Config{
		APIKey: "api-key-secret", ClassicPassword: "classic-secret", MQTTPassword: "mqtt-secret",
		WebUser: "u", WebPassword: "web-secret",
	})
	body := do(t, h, "/api/config", "u", "web-secret").Body.String()
	for _, secret := range []string{"api-key-secret", "classic-secret", "mqtt-secret", "web-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("config leaks %s: %s", secret, body)
		}
	}
}
