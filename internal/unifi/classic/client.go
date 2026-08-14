// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package classic is the client for the legacy UniFi controller API.
//
// It exists only to fill the gaps in the official Integration API
// (CONCEPT.md §2.2): site health, per-client SSID and signal, PoE power
// draw, client blocking and WLAN toggles. Everything it powers is
// optional, and the daemon runs entirely without it — that isolation is
// deliberate, because this interface is undocumented and Ubiquiti has
// reshaped it before (UniFi OS path prefix, mandatory CSRF, /v2/api
// endpoints).
//
// The practical consequence for callers: a failure here must degrade,
// never propagate. The facade disables the affected capabilities and
// the official path keeps running.
package classic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// Console layouts. A UniFi OS console proxies the Network application
// and authenticates centrally; a standalone software controller does
// neither.
type layout struct {
	// loginPath is where credentials are posted.
	loginPath string
	// apiPrefix precedes every data path.
	apiPrefix string
}

var layouts = []layout{
	// UniFi OS (UDM, UCG, UniFi OS Server, …) — by far the common case.
	{loginPath: "/api/auth/login", apiPrefix: "/proxy/network"},
	// Standalone software controller on :8443.
	{loginPath: "/api/login", apiPrefix: ""},
}

// csrfHeader carries the anti-CSRF token UniFi OS requires on every
// state-changing request.
const csrfHeader = "X-CSRF-Token"

// Error identifiers the controller returns in `meta.msg`.
const (
	errLoginRequired = "api.err.LoginRequired"
	err2FARequired   = "api.err.Ubic2faTokenRequired"
)

// ErrTwoFactorRequired reports an account with 2FA enabled. There is no
// way around it non-interactively, so it is reported once with a clear
// message instead of retried forever.
var ErrTwoFactorRequired = errors.New("classic: the account requires 2FA, which this daemon cannot satisfy")

// Client talks to one console's classic API.
//
// Safe for concurrent use: the health and client-enrichment polls run
// from different loops and share the session.
type Client struct {
	tr       *unifi.Transport
	username string
	password string
	log      *slog.Logger

	// mu guards the session state below. Login is serialised through it
	// so a burst of concurrent 401s produces one re-login rather than
	// one per caller — which on some controller versions trips a
	// brute-force lockout.
	mu        sync.Mutex
	jar       http.CookieJar
	csrf      string
	layout    layout
	loggedIn  bool
	lastLogin time.Time
}

// Config configures a [Client].
type Config struct {
	// Transport is the shared HTTP layer. Required.
	Transport *unifi.Transport
	// BaseURL is the console origin, used to scope the cookie jar.
	BaseURL string
	// Username and Password are a LOCAL UniFi admin account. A Ubiquiti
	// SSO login does not work here, and 2FA cannot be satisfied.
	Username string
	Password string
	// Logger receives diagnostics; nil uses slog.Default().
	Logger *slog.Logger
}

// New builds a Client. Call [Client.Login] before the first data call.
func New(cfg Config) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("classic: cookie jar: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		tr:       cfg.Transport,
		username: cfg.Username,
		password: cfg.Password,
		log:      log,
		jar:      jar,
		layout:   layouts[0],
	}, nil
}

// loginRequest is the credential payload.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

// Login authenticates and stores the session cookie and CSRF token.
//
// It probes the two console layouts, because which one applies is a
// property of the hardware rather than a preference — the same reason
// the Integration API client probes its path prefix.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	var lastErr error

	for _, l := range layouts {
		err := c.attemptLogin(ctx, l)
		switch {
		case err == nil:
			c.layout = l
			c.loggedIn = true
			c.lastLogin = time.Now()
			c.log.Info("classic.logged_in",
				slog.String("login_path", l.loginPath),
				slog.String("api_prefix", l.apiPrefix))
			return nil
		case errors.Is(err, ErrTwoFactorRequired):
			// Trying the other layout would produce the same answer and
			// bury the real cause.
			return err
		case errors.Is(err, unifi.ErrUnauthorized):
			// Wrong credentials, not the wrong layout: a 401 means the
			// endpoint exists and rejected us.
			return fmt.Errorf("classic: login rejected — check CLASSIC_USERNAME/CLASSIC_PASSWORD "+
				"(a local admin, not a Ubiquiti SSO account): %w", err)
		default:
			lastErr = err
		}
	}

	c.loggedIn = false
	return fmt.Errorf("classic: no login endpoint responded: %w", lastErr)
}

func (c *Client) attemptLogin(ctx context.Context, l layout) error {
	var out envelope[json.RawMessage]

	resp, err := c.tr.DoWithResponse(ctx, unifi.Request{
		Method: http.MethodPost,
		Path:   l.loginPath,
		Body:   loginRequest{Username: c.username, Password: c.password, Remember: true},
		Out:    &out,
	})
	if err != nil {
		// A 2FA challenge arrives as HTTP 499 with a specific message,
		// which is worth naming rather than reporting as a generic
		// rejection.
		var apiErr *unifi.APIError
		if errors.As(err, &apiErr) && apiErr.Code == err2FARequired {
			return ErrTwoFactorRequired
		}
		return err
	}
	switch {
	case out.Meta.Msg == err2FARequired:
		return ErrTwoFactorRequired
	case out.Meta.RC == "error":
		return fmt.Errorf("classic: login rejected: %s", out.Meta.Msg)
	}

	c.storeSession(resp)
	return nil
}

// storeSession records the cookies and CSRF token from a response.
func (c *Client) storeSession(resp *unifi.ResponseMeta) {
	if resp == nil {
		return
	}
	if resp.URL != nil {
		c.jar.SetCookies(resp.URL, resp.Cookies)
	}
	// UniFi OS returns the token in a header on login and expects it
	// back on every state-changing call.
	if token := resp.Header.Get(csrfHeader); token != "" {
		c.csrf = token
	}
	// Some versions ship it as a TOKEN cookie instead.
	for _, ck := range resp.Cookies {
		if strings.EqualFold(ck.Name, "TOKEN") && c.csrf == "" {
			c.csrf = ck.Value
		}
	}
}

// get performs an authenticated GET against a site path.
func (c *Client) get(ctx context.Context, siteRef, path string, out any) error {
	return c.do(ctx, http.MethodGet, c.sitePath(siteRef, path), nil, out)
}

// post performs an authenticated POST against a site path.
func (c *Client) post(ctx context.Context, siteRef, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, c.sitePath(siteRef, path), body, out)
}

// do issues a request, logging in first when needed and retrying once
// after a session expiry.
//
// The single retry is the important part: controller sessions expire on
// their own schedule, and an expired one is indistinguishable from a
// wrong password until you try. Retrying more than once on a genuinely
// wrong password would hammer the login endpoint.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if err := c.ensureSession(ctx); err != nil {
		return err
	}

	err := c.request(ctx, method, path, body, out)
	if !isSessionExpired(err) {
		return err
	}

	c.log.Debug("classic.session_expired", slog.String("path", path))
	c.mu.Lock()
	c.loggedIn = false
	relogErr := c.loginLocked(ctx)
	c.mu.Unlock()
	if relogErr != nil {
		return relogErr
	}
	return c.request(ctx, method, path, body, out)
}

func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	return c.loginLocked(ctx)
}

func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	c.mu.Lock()
	header := http.Header{}
	if c.csrf != "" {
		header.Set(csrfHeader, c.csrf)
	}
	cookies := c.cookieHeaderLocked(path)
	c.mu.Unlock()

	if cookies != "" {
		header.Set("Cookie", cookies)
	}

	resp, err := c.tr.DoWithResponse(ctx, unifi.Request{
		Method: method,
		Path:   path,
		Body:   body,
		Header: header,
		Out:    out,
	})
	if err != nil {
		return err
	}

	// A refreshed CSRF token can arrive on any response.
	c.mu.Lock()
	c.storeSession(resp)
	c.mu.Unlock()

	// The status code alone is not enough here: the controller reports
	// an expired session as HTTP 200 with meta.rc = "error".
	if carrier, ok := out.(metaCarrier); ok {
		return carrier.apiError()
	}
	return nil
}

// cookieHeaderLocked renders the stored cookies. Must be called with
// the mutex held.
func (c *Client) cookieHeaderLocked(path string) string {
	u, err := url.Parse(c.tr.BaseURL() + path)
	if err != nil {
		return ""
	}
	cookies := c.jar.Cookies(u)
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

// isSessionExpired reports whether an error means "log in again".
//
// The controller signals this two ways: a plain 401, and a 200 whose
// body carries meta.msg = api.err.LoginRequired. Missing the second
// would turn an expired session into a permanent, silent failure.
func isSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, unifi.ErrUnauthorized) {
		return true
	}
	var apiErr *unifi.APIError
	return errors.As(err, &apiErr) && apiErr.Code == errLoginRequired
}

// sitePath builds a site-scoped API path. siteRef is the controller's
// own site identifier ("default"), not the Integration API's UUID.
func (c *Client) sitePath(siteRef, path string) string {
	c.mu.Lock()
	prefix := c.layout.apiPrefix
	c.mu.Unlock()
	return prefix + "/api/s/" + siteRef + path
}

// LoggedIn reports whether a session is currently established.
func (c *Client) LoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedIn
}
