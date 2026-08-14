// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package classic_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi/classic"
)

const (
	testUser = "admin"
	testPass = "secret"
	siteRef  = "default"
)

func newClient(t *testing.T, baseURL string) *classic.Client {
	t.Helper()

	tr, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL: baseURL,
		Timeout: 5 * time.Second,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	c, err := classic.New(classic.Config{
		Transport: tr,
		BaseURL:   baseURL,
		Username:  testUser,
		Password:  testPass,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// unifiOS serves the UniFi OS layout: /api/auth/login, /proxy/network
// prefix, a session cookie and a CSRF token.
type unifiOS struct {
	logins   atomic.Int32
	requests atomic.Int32
	// expireOnce makes the next data request report an expired session,
	// then clears itself — so the re-login that follows succeeds and
	// the retry can be observed.
	expireOnce atomic.Bool
	// body is what data endpoints return.
	body string
	// requireCSRF fails state-changing requests without the token.
	requireCSRF bool
	// lastCSRF records the token the client sent.
	lastCSRF atomic.Value
}

func (s *unifiOS) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			s.logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "session-abc", Path: "/"})
			w.Header().Set("X-CSRF-Token", "csrf-xyz")
			_, _ = io.WriteString(w, `{"meta":{"rc":"ok"},"data":[]}`)

		case "/proxy/network/api/s/default/stat/health",
			"/proxy/network/api/s/default/stat/sta",
			"/proxy/network/api/s/default/stat/device",
			"/proxy/network/api/s/default/cmd/stamgr",
			"/proxy/network/api/s/default/cmd/devmgr":
			n := s.requests.Add(1)
			if tok := r.Header.Get("X-CSRF-Token"); tok != "" {
				s.lastCSRF.Store(tok)
			}
			if s.requireCSRF && r.Method == http.MethodPost && r.Header.Get("X-CSRF-Token") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Cookie") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"meta":{"rc":"error","msg":"api.err.LoginRequired"}}`)
				return
			}
			if s.expireOnce.CompareAndSwap(true, false) {
				// The subtle case this exists for: HTTP 200 carrying the
				// error in the body. A client checking only status codes
				// sees success and decodes an empty data array.
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"meta":{"rc":"error","msg":"api.err.LoginRequired"},"data":[]}`)
				return
			}
			_ = n
			body := s.body
			if body == "" {
				body = `{"meta":{"rc":"ok"},"data":[]}`
			}
			_, _ = io.WriteString(w, body)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestLoginUniFiOSLayout(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	c := newClient(t, ts.URL)
	if err := c.Login(t.Context()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !c.LoggedIn() {
		t.Error("LoggedIn() = false after a successful login")
	}
	if got := srv.logins.Load(); got != 1 {
		t.Errorf("made %d login attempts, want 1", got)
	}
}

// A standalone software controller has no /api/auth/login and no
// /proxy/network prefix; the probe has to find that layout too.
func TestLoginStandaloneLayout(t *testing.T) {
	t.Parallel()

	var logins atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "sess", Path: "/"})
			_, _ = io.WriteString(w, `{"meta":{"rc":"ok"},"data":[]}`)
		case "/api/s/default/stat/health":
			_, _ = io.WriteString(w, `{"meta":{"rc":"ok"},"data":[{"subsystem":"wan","status":"ok"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	c := newClient(t, ts.URL)
	if err := c.Login(t.Context()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := logins.Load(); got != 1 {
		t.Errorf("standalone login attempts = %d, want 1", got)
	}

	// The data path must use the un-prefixed layout too.
	h, err := c.Health(t.Context(), siteRef)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.WAN.Status != "ok" {
		t.Errorf("WAN status = %q, want ok", h.WAN.Status)
	}
}

// Wrong credentials must be reported as such, not retried against the
// other layout until something else fails.
func TestLoginRejectedIsReportedClearly(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"meta":{"rc":"error","msg":"api.err.Invalid"}}`)
	}))
	t.Cleanup(ts.Close)

	err := newClient(t, ts.URL).Login(t.Context())
	if err == nil {
		t.Fatal("Login succeeded with bad credentials")
	}
	if !errors.Is(err, unifi.ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized", err)
	}
	// The message has to point at the actual cause, since "local admin,
	// not SSO" is the mistake people make here.
	if !containsAll(err.Error(), "CLASSIC_USERNAME", "SSO") {
		t.Errorf("error = %v, want it to name the credential keys", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts on a rejected login, want 1", got)
	}
}

// 2FA cannot be satisfied non-interactively, so it must be reported
// once with a clear message rather than retried forever.
func TestTwoFactorIsReportedNotRetried(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = io.WriteString(w, `{"meta":{"rc":"error","msg":"api.err.Ubic2faTokenRequired"}}`)
	}))
	t.Cleanup(ts.Close)

	err := newClient(t, ts.URL).Login(t.Context())
	if !errors.Is(err, classic.ErrTwoFactorRequired) {
		t.Fatalf("error = %v, want ErrTwoFactorRequired", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts on a 2FA challenge, want 1", got)
	}
}

// Controller sessions expire on their own schedule, and the expiry
// arrives as HTTP 200 with an error in the body — missing that would
// turn it into a permanent silent failure.
func TestSessionExpiryTriggersOneRelogin(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{body: `{"meta":{"rc":"ok"},"data":[{"subsystem":"wan","status":"ok"}]}`}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	c := newClient(t, ts.URL)
	if _, err := c.Health(t.Context(), siteRef); err != nil {
		t.Fatalf("first Health: %v", err)
	}
	if got := srv.logins.Load(); got != 1 {
		t.Fatalf("logins after first call = %d, want 1", got)
	}

	// The next call finds the session expired, re-logs in and retries.
	srv.expireOnce.Store(true)
	h, err := c.Health(t.Context(), siteRef)
	if err != nil {
		t.Fatalf("Health after expiry: %v", err)
	}
	if got := srv.logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2 (one re-login after expiry)", got)
	}
	// The retry must return real data, not the error response.
	if h.WAN.Status != "ok" {
		t.Errorf("WAN status after re-login = %q, want ok", h.WAN.Status)
	}
}

// The CSRF token from login must be sent back on state-changing calls;
// UniFi OS rejects them otherwise.
func TestCSRFTokenIsSentBack(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{requireCSRF: true}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	c := newClient(t, ts.URL)
	if err := c.SetClientBlocked(t.Context(), siteRef, model.MustParseMAC("00:00:5e:00:53:10"), true); err != nil {
		t.Fatalf("SetClientBlocked: %v", err)
	}
	if got, _ := srv.lastCSRF.Load().(string); got != "csrf-xyz" {
		t.Errorf("CSRF token sent = %q, want csrf-xyz", got)
	}
}

func TestHealthDecoding(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{body: `{"meta":{"rc":"ok"},"data":[
		{"subsystem":"wan","status":"ok","wan_ip":"203.0.113.7","latency":12,"uptime":864000,
		 "rx_bytes-r":2300000,"tx_bytes-r":262144,"num_gw":1},
		{"subsystem":"wlan","status":"ok","num_user":40,"num_guest":3,"num_ap":3},
		{"subsystem":"lan","status":"warning","num_user":20,"num_sw":8},
		{"subsystem":"vpn","status":"unknown"}
	]}`}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	h, err := newClient(t, ts.URL).Health(t.Context(), siteRef)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if h.WAN.Status != "ok" || h.LAN.Status != "warning" || h.VPN.Status != "unknown" {
		t.Errorf("subsystem statuses = %+v", h)
	}
	if got := h.WANIP.String(); got != "203.0.113.7" {
		t.Errorf("WANIP = %q", got)
	}
	if h.LatencyMs == nil || *h.LatencyMs != 12 {
		t.Errorf("latency = %v, want 12", h.LatencyMs)
	}
	if h.UptimeSec == nil || *h.UptimeSec != 864000 {
		t.Errorf("uptime = %v, want 864000", h.UptimeSec)
	}
	// The controller reports bytes/s; the topic layout is bits/s, to
	// match the device uplink rates.
	if got, want := h.RxBps, uint64(2300000*8); got != want {
		t.Errorf("RxBps = %d, want %d (bytes converted to bits)", got, want)
	}
	// User counts appear on several subsystems and are summed.
	if got, want := h.NumUser, 60; got != want {
		t.Errorf("NumUser = %d, want %d", got, want)
	}
	if h.NumAP != 3 || h.NumSwitch != 8 || h.NumGateway != 1 {
		t.Errorf("device counts = ap %d sw %d gw %d", h.NumAP, h.NumSwitch, h.NumGateway)
	}
}

func TestClientDetailsDecoding(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{body: `{"meta":{"rc":"ok"},"data":[
		{"mac":"00:00:5e:00:53:10","_id":"abc","hostname":"phone","essid":"HomeNet",
		 "signal":-52,"is_wired":false,"blocked":false,"last_seen":1786000000,"vlan":20,"network":"IoT"},
		{"mac":"00:00:5e:00:53:11","_id":"def","hostname":"nas","rssi":-40,"blocked":true},
		{"mac":"not-a-mac","_id":"ghi"}
	]}`}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	details, err := newClient(t, ts.URL).ClientDetails(t.Context(), siteRef)
	if err != nil {
		t.Fatalf("ClientDetails: %v", err)
	}
	if got, want := len(details), 2; got != want {
		t.Fatalf("got %d clients, want %d (the malformed MAC is skipped)", got, want)
	}

	phone := details[model.MustParseMAC("00:00:5e:00:53:10")]
	if phone.SSID != "HomeNet" || phone.SignalDBm != -52 || phone.Hostname != "phone" {
		t.Errorf("phone = %+v", phone)
	}
	if phone.VLAN != 20 || phone.Network != "IoT" {
		t.Errorf("phone network = %q/vlan %d", phone.Network, phone.VLAN)
	}
	if phone.LastSeen.IsZero() {
		t.Error("phone LastSeen was not decoded")
	}

	// Controllers report signal in one field or the other depending on
	// version and radio; both are dBm.
	nas := details[model.MustParseMAC("00:00:5e:00:53:11")]
	if nas.SignalDBm != -40 {
		t.Errorf("nas SignalDBm = %d, want -40 from the rssi field", nas.SignalDBm)
	}
	if !nas.Blocked {
		t.Error("nas Blocked = false, want true")
	}
}

func TestPortPowerDecoding(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{body: `{"meta":{"rc":"ok"},"data":[
		{"mac":"00:00:5e:00:53:02","_id":"sw","port_table":[
			{"port_idx":1,"poe_power":"7.40","poe_enable":true},
			{"port_idx":2,"poe_power":"0.00","poe_enable":false},
			{"port_idx":3},
			{"port_idx":4,"poe_power":"not-a-number"}
		]}
	]}`}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	power, err := newClient(t, ts.URL).PortPower(t.Context(), siteRef)
	if err != nil {
		t.Fatalf("PortPower: %v", err)
	}

	byPort := power[model.MustParseMAC("00:00:5e:00:53:02")]
	if got, want := byPort[1], 7.4; got != want {
		t.Errorf("port 1 = %v W, want %v", got, want)
	}
	// A port drawing nothing, one with no field, and one with garbage
	// must all be absent rather than reported as 0 W — the distinction
	// between "no power" and "no data" matters for the sensor.
	for _, idx := range []int{2, 3, 4} {
		if _, ok := byPort[idx]; ok {
			t.Errorf("port %d has a power reading, want none", idx)
		}
	}
}

func TestCommands(t *testing.T) {
	t.Parallel()

	type call struct{ path, body string }
	got := make(chan call, 2)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "s", Path: "/"})
			_, _ = io.WriteString(w, `{"meta":{"rc":"ok"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		got <- call{path: r.URL.Path, body: string(body)}
		_, _ = io.WriteString(w, `{"meta":{"rc":"ok"},"data":[]}`)
	}))
	t.Cleanup(ts.Close)

	c := newClient(t, ts.URL)
	mac := model.MustParseMAC("00:00:5e:00:53:10")

	if err := c.SetClientBlocked(t.Context(), siteRef, mac, true); err != nil {
		t.Fatalf("block: %v", err)
	}
	if v := <-got; v.body != `{"cmd":"block-sta","mac":"00:00:5e:00:53:10"}` {
		t.Errorf("block body = %s", v.body)
	}

	if err := c.SetLocate(t.Context(), siteRef, mac, false); err != nil {
		t.Fatalf("locate: %v", err)
	}
	if v := <-got; v.body != `{"cmd":"unset-locate","mac":"00:00:5e:00:53:10"}` {
		t.Errorf("locate body = %s", v.body)
	}
}

// The classic API has no schema, so a response missing fields must
// decode to zero values rather than failing the poll.
func TestPartialResponsesDoNotFail(t *testing.T) {
	t.Parallel()

	srv := &unifiOS{body: `{"meta":{"rc":"ok"},"data":[{"subsystem":"wan"}]}`}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)

	h, err := newClient(t, ts.URL).Health(t.Context(), siteRef)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	// A controller that omits latency entirely — the reference console
	// does — must leave it absent rather than reporting a measured 0.
	if h.LatencyMs != nil {
		t.Errorf("absent latency decoded to %v, want nil", *h.LatencyMs)
	}
	if h.WANIP.IsValid() {
		t.Errorf("absent WAN IP decoded to %v", h.WANIP)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return sub == "" || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = context.Background

// UniFi OS answers a bad password with 403 and
// AUTHENTICATION_FAILED_INVALID_CREDENTIALS, not 401. Treating that as
// "wrong layout" makes the client try the standalone endpoint, get a
// 401 because it does not exist there, and report *that* — pointing the
// operator at the wrong problem entirely.
func TestForbiddenIsTreatedAsBadCredentials(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path == "/api/auth/login" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w,
				`{"message":"Invalid username or password","code":"AUTHENTICATION_FAILED_INVALID_CREDENTIALS"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)

	err := newClient(t, ts.URL).Login(t.Context())
	if err == nil {
		t.Fatal("Login succeeded with a rejected password")
	}
	if !errors.Is(err, unifi.ErrForbidden) {
		t.Errorf("error = %v, want it to carry ErrForbidden", err)
	}
	if !containsAll(err.Error(), "CLASSIC_USERNAME", "SSO") {
		t.Errorf("error = %v, want it to name the credential keys", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts, want 1 — a rejected password must not retry the other layout", got)
	}
}

// When both layouts genuinely fail, the error must name what each one
// said. Reporting only the last points at the endpoint that was never
// the right one.
func TestBothLayoutsFailingNamesBoth(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	err := newClient(t, ts.URL).Login(t.Context())
	if err == nil {
		t.Fatal("Login succeeded against a broken console")
	}
	if !containsAll(err.Error(), "/api/auth/login", "/api/login") {
		t.Errorf("error = %v, want it to name both login paths", err)
	}
}
