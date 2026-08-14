// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
)

func newServer(t *testing.T, cfg Config) (*Server, *state.Store) {
	t.Helper()

	store := state.New(time.Now().Add(-time.Hour))
	cfg.Logger = slog.New(slog.DiscardHandler)
	srv, err := New(cfg, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, store
}

// do issues a request through the full handler chain, auth included.
func do(t *testing.T, srv *Server, method, path string, auth ...string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, http.NoBody)
	if len(auth) == 2 {
		req.SetBasicAuth(auth[0], auth[1])
	}
	rec := httptest.NewRecorder()
	srv.auth(srv.mux).ServeHTTP(rec, req)
	return rec
}

func TestServesTheSPA(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, Config{})
	for _, path := range []string{"/", "/app.css", "/app.js"} {
		rec := do(t, srv, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s -> %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
}

// The Ingress proxy serves this page under a generated path prefix, so
// an absolute /app.css or /api/state escapes it and 404s.
func TestAssetReferencesAreRelative(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, Config{})
	html := do(t, srv, http.MethodGet, "/").Body.String()
	js := do(t, srv, http.MethodGet, "/app.js").Body.String()

	for _, bad := range []string{`href="/`, `src="/`} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html contains an absolute reference (%s) — it would break behind Ingress", bad)
		}
	}
	if strings.Contains(js, `fetch("/`) || strings.Contains(js, "fetch('/") {
		t.Error("app.js fetches an absolute path — it would break behind Ingress")
	}
	if !strings.Contains(js, `fetch("api/state"`) {
		t.Error("app.js does not fetch the relative api/state")
	}
}

func TestStateEndpoint(t *testing.T) {
	t.Parallel()

	srv, store := newServer(t, Config{Language: "de"})
	store.SetSite(
		model.Site{Name: "Default", Internal: "default"},
		model.ControllerInfo{ApplicationVersion: "10.5.67"},
	)
	store.SetMQTTConnected(true)
	store.SetDevices([]model.Device{{
		MAC: model.MustParseMAC("00:00:5e:00:53:02"), Name: "Switch",
		Model: "USW Pro Max 16 PoE", Type: model.DeviceSwitch,
		State: model.DeviceOnline, IP: netip.MustParseAddr("192.0.2.2"),
		Ports: []model.Port{
			{Idx: 1, PoE: &model.PoEState{Enabled: true, PowerW: 7.4}},
			{Idx: 2},
		},
	}})
	store.SetDeviceStats(model.MustParseMAC("00:00:5e:00:53:02"),
		model.DeviceStats{CPUPct: 12.5, MemoryPct: 48, Uptime: time.Hour}, time.Now())
	store.PollSucceeded("devices", time.Now())

	rec := do(t, srv, http.MethodGet, "/api/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the UI polls", got)
	}

	var resp stateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Site.Name != "Default" || resp.Site.ApplicationVersion != "10.5.67" {
		t.Errorf("site = %+v", resp.Site)
	}
	if !resp.Bridge.MQTTConnected {
		t.Error("MQTTConnected = false")
	}
	if resp.Language != "de" {
		t.Errorf("language = %q, want de", resp.Language)
	}
	if len(resp.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(resp.Devices))
	}

	d := resp.Devices[0]
	if d.CPUPct != 12.5 || !d.HasStats {
		t.Errorf("device stats = %+v", d)
	}
	if d.PoEPorts != 1 || d.PoEWatts != 7.4 {
		t.Errorf("PoE = %d ports / %v W, want 1 / 7.4", d.PoEPorts, d.PoEWatts)
	}
	if len(resp.Loops) != 1 || resp.Loops[0].Name != "devices" {
		t.Errorf("loops = %+v", resp.Loops)
	}
}

// A device whose statistics never arrived must be distinguishable from
// one that is genuinely idle at 0%.
func TestMissingStatisticsAreMarked(t *testing.T) {
	t.Parallel()

	srv, store := newServer(t, Config{})
	store.SetDevices([]model.Device{{
		MAC: model.MustParseMAC("00:00:5e:00:53:03"), Name: "AP", State: model.DeviceOffline,
	}})

	var resp stateResponse
	if err := json.Unmarshal(do(t, srv, http.MethodGet, "/api/state").Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Devices[0].HasStats {
		t.Error("HasStats = true for a device with no statistics")
	}
}

// A loop that failed before it ever succeeded has no timestamp.
// Omitting it would hide exactly the thing worth seeing.
func TestLoopThatNeverSucceededIsStillListed(t *testing.T) {
	t.Parallel()

	srv, store := newServer(t, Config{})
	store.PollFailed("health", errors.New("unreachable"), time.Now())

	var resp stateResponse
	if err := json.Unmarshal(do(t, srv, http.MethodGet, "/api/state").Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Loops) != 1 {
		t.Fatalf("loops = %+v, want the failed one listed", resp.Loops)
	}
	if !resp.Loops[0].Failed || resp.Loops[0].AgeS != -1 {
		t.Errorf("loop = %+v, want failed with age -1", resp.Loops[0])
	}
	if len(resp.Errors) != 1 {
		t.Errorf("errors = %+v, want the failure reported", resp.Errors)
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, Config{User: "admin", Password: "secret"})

	if got := do(t, srv, http.MethodGet, "/api/state").Code; got != http.StatusUnauthorized {
		t.Errorf("without credentials -> %d, want 401", got)
	}
	if got := do(t, srv, http.MethodGet, "/api/state", "admin", "wrong").Code; got != http.StatusUnauthorized {
		t.Errorf("with a wrong password -> %d, want 401", got)
	}
	if got := do(t, srv, http.MethodGet, "/api/state", "admin", "secret").Code; got != http.StatusOK {
		t.Errorf("with correct credentials -> %d, want 200", got)
	}

	// The static assets must be behind the same gate — an unauthenticated
	// index page that then fails to load data is a confusing half-open
	// door.
	if got := do(t, srv, http.MethodGet, "/").Code; got != http.StatusUnauthorized {
		t.Errorf("the SPA is served without auth -> %d, want 401", got)
	}

	if hdr := do(t, srv, http.MethodGet, "/").Header().Get("WWW-Authenticate"); !strings.Contains(hdr, "Basic") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge", hdr)
	}
}

// Only a password set makes auth active too: a config with a password
// and no user must not silently serve everything.
func TestAuthEnabledByEitherField(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, Config{Password: "secret"})
	if got := do(t, srv, http.MethodGet, "/api/state").Code; got != http.StatusUnauthorized {
		t.Errorf("password-only config served without auth -> %d, want 401", got)
	}
}

func TestNoAuthWhenUnconfigured(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, Config{})
	if got := do(t, srv, http.MethodGet, "/api/state").Code; got != http.StatusOK {
		t.Errorf("status = %d with auth disabled, want 200", got)
	}
}

// The liveness probe deliberately reports only that the process serves.
// Failing it when the console is unreachable would produce a crash loop
// that cannot fix the console.
func TestHealthEndpointIsProcessLevel(t *testing.T) {
	t.Parallel()

	srv, store := newServer(t, Config{})
	store.PollFailed("devices", errors.New("console unreachable"), time.Now())
	store.SetMQTTConnected(false)

	rec := do(t, srv, http.MethodGet, "/api/health")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d with everything broken, want 200", rec.Code)
	}
}

// The UI shows raw values from the console; a device named with markup
// is a legal UniFi name and must not become executable.
func TestValuesAreNotInterpolatedAsHTML(t *testing.T) {
	t.Parallel()

	js := string(mustAsset(t, "static/app.js"))
	if strings.Contains(js, ".innerHTML") {
		t.Error("app.js uses innerHTML; device names from the console would be interpreted as markup")
	}
	if !strings.Contains(js, "textContent") {
		t.Error("app.js does not use textContent")
	}
}

func mustAsset(t *testing.T, name string) []byte {
	t.Helper()
	b, err := assets.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// An API that returns null for "no errors" invites a null dereference
// in every future consumer. Empty collections serialise as [].
func TestEmptyCollectionsAreArrays(t *testing.T) {
	t.Parallel()

	srv, _ := newServer(t, Config{})
	body := do(t, srv, http.MethodGet, "/api/state").Body.String()

	for _, field := range []string{`"devices":null`, `"clients":null`, `"wlans":null`, `"loops":null`, `"errors":null`} {
		if strings.Contains(body, field) {
			t.Errorf("response contains %s, want an empty array", field)
		}
	}
}
