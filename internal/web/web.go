// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package web serves the optional diagnostic UI.
//
// It answers one question: is the bridge doing what it should? What the
// console reports, what reached the broker, which poll last succeeded,
// and what failed. It is deliberately read-only — the write path is
// MQTT, and a second one would be a second thing to secure.
//
// The dashboard — server, hardening, SSE stream and single-page app — is
// go-bridgeui, the one every go-*2mqtt bridge embeds; this package
// presents the state store as go-bridgeui's contract and names the page's
// layout and labels. go-bridgeui's references are relative, which is what
// lets the page work unchanged behind the Ingress proxy's path prefix.
package web

import (
	"embed"
	"log/slog"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
	"github.com/SukramJ/go-unifi2mqtt/internal/version"
)

// i18nFS holds this bridge's labels — sections, groups, columns, status
// and configuration tiles — merged over go-bridgeui's built-in bundles.
//
//go:embed i18n
var i18nFS embed.FS

// layout is the UniFi page: the site's health and WLANs, its devices and
// clients as tables.
var layout = bridgeui.Layout{
	Brand:   "unifi2mqtt",
	Product: "UniFi Network",
	Icon:    "📶",
	Sections: []bridgeui.Section{
		{ID: "site", Title: "section.site", Nav: "nav.site", Groups: []string{groupHealth, groupWLANs}},
		{ID: "devices", Title: "section.devices", Nav: "nav.devices", Tables: []string{tableDevices}},
		{ID: "clients", Title: "section.clients", Nav: "nav.clients", Tables: []string{tableClients}, Optional: true},
	},
}

// New builds the dashboard over the store. It does not bind a socket —
// call Run on the result. app is the daemon's configuration, shown
// sanitised under System.
func New(app *config.Config, store *state.Store, logger *slog.Logger) *bridgeui.Server {
	if logger == nil {
		logger = slog.Default()
	}
	if app == nil {
		app = &config.Config{}
	}
	tr, err := bridgeui.LoadTranslations(i18nFS, "i18n")
	if err != nil {
		// The bundles are embedded and covered by a test.
		panic("web: " + err.Error())
	}
	return bridgeui.New(bridgeui.Options{
		Bind:         app.WebBind,
		User:         app.WebUser,
		Password:     app.WebPassword.Reveal(),
		Realm:        "go-unifi2mqtt",
		Logger:       logger,
		Language:     app.Language,
		Layout:       layout,
		Translations: tr,
		// /api/health stays a liveness probe: a restart would not fix an
		// unreachable console, so failing it would only produce a crash
		// loop.
	}, &adapter{store: store, cfg: app, version: version.Version})
}
