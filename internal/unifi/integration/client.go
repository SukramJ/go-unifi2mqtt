// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package integration is the client for the official UniFi Network
// Integration API v1.
//
// It is the daemon's primary data source: API-key authenticated, served
// by the console itself, and backed by a published OpenAPI schema. What
// it cannot do — site health, per-client SSID and signal, PoE power
// draw, client blocking, WLAN toggles — is listed in CONCEPT.md §2.2
// and filled in by the optional classic client.
//
// All methods return domain types from internal/model; the wire structs
// in dto.go never escape this package.
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// API path prefixes, in probe order.
//
// A UniFi OS console (UDM, UCG, UniFi OS Server, …) proxies the Network
// application under /proxy/network; a standalone software controller
// serves it at the root. Probing beats making the operator configure
// it, since the answer is a property of the console, not a preference.
var pathPrefixes = []string{
	"/proxy/network/integration",
	"/integration",
}

// apiKeyHeader is the authentication header. The key travels here and
// never in a URL, which is what keeps it out of error strings and logs.
const apiKeyHeader = "X-API-KEY" //nolint:gosec // header name, not a credential

// pageLimit is the page size for list endpoints. The API caps it at
// 200; asking for the maximum keeps the request count down on sites
// with many clients.
const pageLimit = 200

// maxPages bounds pagination so a console that keeps reporting a
// totalCount it never delivers cannot spin forever.
const maxPages = 100

// Client talks to one console's Integration API.
type Client struct {
	tr     *unifi.Transport
	apiKey string
	log    *slog.Logger

	// prefix is resolved by Probe and read by every request afterwards.
	// Probe runs once during startup, before the poll goroutines exist,
	// so this needs no synchronisation.
	prefix string
}

// Config configures a [Client].
type Config struct {
	// Transport is the shared HTTP layer. Required.
	Transport *unifi.Transport
	// APIKey authenticates every request.
	APIKey string
	// Logger receives diagnostics; nil uses slog.Default().
	Logger *slog.Logger
}

// New builds a Client. Call [Client.Probe] before the first data call
// to resolve the console's path prefix.
func New(cfg Config) *Client {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		tr:     cfg.Transport,
		apiKey: cfg.APIKey,
		log:    log,
		// Start on the UniFi OS layout: it covers every current console,
		// so an unprobed client still works against the common case.
		prefix: pathPrefixes[0],
	}
}

// Probe determines the console's API path prefix and returns the
// application version.
//
// It tries each known prefix against `/v1/info` and keeps the first that
// answers. A 401 stops the probe immediately: the key is wrong, and
// trying the other prefix would only produce a second, more confusing
// error.
func (c *Client) Probe(ctx context.Context) (model.ControllerInfo, error) {
	var lastErr error

	for _, prefix := range pathPrefixes {
		var info applicationInfo
		err := c.do(ctx, http.MethodGet, prefix+"/v1/info", nil, nil, &info)
		if err == nil {
			c.prefix = prefix
			c.log.Debug("integration.probe_ok",
				slog.String("prefix", prefix),
				slog.String("application_version", info.ApplicationVersion))
			return model.ControllerInfo{ApplicationVersion: info.ApplicationVersion}, nil
		}
		if errors.Is(err, unifi.ErrUnauthorized) || errors.Is(err, unifi.ErrForbidden) {
			return model.ControllerInfo{}, err
		}
		lastErr = err
	}

	return model.ControllerInfo{}, fmt.Errorf(
		"integration: no Integration API found on this console "+
			"(needs UniFi Network 10.5 or newer): %w", lastErr,
	)
}

// Info returns the console's application version.
func (c *Client) Info(ctx context.Context) (model.ControllerInfo, error) {
	var info applicationInfo
	if err := c.do(ctx, http.MethodGet, c.path("/v1/info"), nil, nil, &info); err != nil {
		return model.ControllerInfo{}, err
	}
	return model.ControllerInfo{ApplicationVersion: info.ApplicationVersion}, nil
}

// Sites lists the sites on this console.
func (c *Client) Sites(ctx context.Context) ([]model.Site, error) {
	raw, err := paginate[site](ctx, c, c.path("/v1/sites"))
	if err != nil {
		return nil, err
	}
	out := make([]model.Site, 0, len(raw))
	for _, s := range raw {
		out = append(out, toSite(s))
	}
	return out, nil
}

// ResolveSite maps a site identifier — either the UUID or the
// `internalReference` an operator would recognise ("default") — onto
// the site record.
//
// Configuration names the site the way the UniFi UI does, but every
// other endpoint takes the UUID, so this translation has to happen once
// at startup.
func (c *Client) ResolveSite(ctx context.Context, name string) (model.Site, error) {
	sites, err := c.Sites(ctx)
	if err != nil {
		return model.Site{}, err
	}
	for _, s := range sites {
		if s.ID == name || s.Internal == name || s.Name == name {
			return s, nil
		}
	}

	known := make([]string, 0, len(sites))
	for _, s := range sites {
		known = append(known, s.Internal)
	}
	return model.Site{}, fmt.Errorf("integration: site %q not found (this console has: %v)", name, known)
}

// Devices lists the adopted devices of a site.
//
// The overview response already carries state, model, firmware and the
// update flag, so this alone feeds most device sensors; ports, radios
// and the uplink need [Client.Device] per device.
func (c *Client) Devices(ctx context.Context, siteID string) ([]model.Device, error) {
	raw, err := paginate[deviceOverview](ctx, c, c.path("/v1/sites/"+siteID+"/devices"))
	if err != nil {
		return nil, err
	}
	out := make([]model.Device, 0, len(raw))
	for i := range raw {
		out = append(out, toDeviceOverview(&raw[i], c.log))
	}
	return out, nil
}

// Device fetches one device including its ports, radios and uplink.
func (c *Client) Device(ctx context.Context, siteID, deviceID string) (model.Device, error) {
	var d deviceDetails
	if err := c.do(ctx, http.MethodGet, c.path("/v1/sites/"+siteID+"/devices/"+deviceID), nil, nil, &d); err != nil {
		return model.Device{}, err
	}
	return toDeviceDetails(&d, c.log), nil
}

// DeviceStats fetches the latest statistics sample for one device.
//
// Callers should skip devices that are not [model.DeviceState.IsOnline]:
// an offline device answers with an empty sample, so the call is pure
// overhead on a site with unplugged hardware (CONCEPT.md §8.2).
func (c *Client) DeviceStats(ctx context.Context, siteID, deviceID string) (model.DeviceStats, error) {
	var s deviceStatistics
	path := c.path("/v1/sites/" + siteID + "/devices/" + deviceID + "/statistics/latest")
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &s); err != nil {
		return model.DeviceStats{}, err
	}
	return toDeviceStats(&s, deviceID, c.log), nil
}

// Clients lists the site's connected clients.
func (c *Client) Clients(ctx context.Context, siteID string) ([]model.Client, error) {
	raw, err := paginate[clientOverview](ctx, c, c.path("/v1/sites/"+siteID+"/clients"))
	if err != nil {
		return nil, err
	}
	out := make([]model.Client, 0, len(raw))
	for i := range raw {
		out = append(out, toClient(&raw[i], c.log))
	}
	return out, nil
}

// Networks lists the site's network/VLAN catalogue. Their subnets are
// what map a client's IP onto a VLAN (CONCEPT.md §2.2).
func (c *Client) Networks(ctx context.Context, siteID string) ([]model.Network, error) {
	raw, err := paginate[networkOverview](ctx, c, c.path("/v1/sites/"+siteID+"/networks"))
	if err != nil {
		return nil, err
	}
	out := make([]model.Network, 0, len(raw))
	for i := range raw {
		out = append(out, toNetwork(&raw[i], c.log))
	}
	return out, nil
}

// WLANs lists the site's SSID catalogue.
func (c *Client) WLANs(ctx context.Context, siteID string) ([]model.WLAN, error) {
	raw, err := paginate[wifiBroadcastOverview](ctx, c, c.path("/v1/sites/"+siteID+"/wifi/broadcasts"))
	if err != nil {
		return nil, err
	}
	out := make([]model.WLAN, 0, len(raw))
	for _, w := range raw {
		out = append(out, toWLAN(w))
	}
	return out, nil
}

// --- actuators ---

// RestartDevice reboots an adopted device.
func (c *Client) RestartDevice(ctx context.Context, siteID, deviceID string) error {
	path := c.path("/v1/sites/" + siteID + "/devices/" + deviceID + "/actions")
	return c.do(ctx, http.MethodPost, path, nil, deviceActionRequest{Action: "RESTART"}, nil)
}

// PowerCyclePort cuts and restores power on a PoE port.
func (c *Client) PowerCyclePort(ctx context.Context, siteID, deviceID string, portIdx int) error {
	path := c.path("/v1/sites/" + siteID + "/devices/" + deviceID +
		"/interfaces/ports/" + strconv.Itoa(portIdx) + "/actions")
	return c.do(ctx, http.MethodPost, path, nil, portActionRequest{Action: "POWER_CYCLE"}, nil)
}

// AuthorizeGuest grants a guest client network access. A minutes value
// of zero uses the site's configured default.
func (c *Client) AuthorizeGuest(ctx context.Context, siteID, clientID string, minutes int) error {
	body := clientActionRequest{Action: "AUTHORIZE_GUEST_ACCESS"}
	if minutes > 0 {
		body.TimeLimitMinutes = &minutes
	}
	path := c.path("/v1/sites/" + siteID + "/clients/" + clientID + "/actions")
	return c.do(ctx, http.MethodPost, path, nil, body, nil)
}

// --- plumbing ---

func (c *Client) path(suffix string) string { return c.prefix + suffix }

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.tr.Do(ctx, unifi.Request{
		Method: method,
		Path:   path,
		Query:  q,
		Body:   body,
		Header: http.Header{apiKeyHeader: []string{c.apiKey}},
		Out:    out,
	})
}

// paginate walks a list endpoint to completion.
//
// The API returns `totalCount` alongside each page, so the loop knows
// when it is done without relying on a short page — which matters
// because a page can legitimately come back short of `limit` while more
// results remain.
func paginate[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var out []T

	for offset, pages := 0, 0; ; pages++ {
		if pages >= maxPages {
			c.log.Warn("integration.pagination_capped",
				slog.String("path", path),
				slog.Int("pages", pages),
				slog.Int("collected", len(out)))
			return out, nil
		}

		q := url.Values{
			"offset": []string{strconv.Itoa(offset)},
			"limit":  []string{strconv.Itoa(pageLimit)},
		}
		var p page[T]
		if err := c.do(ctx, http.MethodGet, path, q, nil, &p); err != nil {
			return nil, err
		}

		out = append(out, p.Data...)

		// Stop on an empty page even if totalCount claims more: without
		// this an inconsistent count would loop until the page cap.
		if len(p.Data) == 0 {
			return out, nil
		}
		offset += len(p.Data)
		if int64(offset) >= p.TotalCount {
			return out, nil
		}
	}
}
