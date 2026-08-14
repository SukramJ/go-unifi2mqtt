// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// ErrInvalidConfig wraps every validation failure so callers can match
// a configuration problem with errors.Is without string matching.
var ErrInvalidConfig = errors.New("config: invalid")

// Validate checks the configuration for contradictions and missing
// required values.
//
// Validation is where misconfiguration has to surface — at startup,
// with a message naming the key — rather than as a silently wrong
// result hours later. The most important rules are the capability
// couplings: a filter or control that needs the classic API layer while
// CLASSIC_ENABLE is off would not merely be inert, it would invert the
// operator's intent (an SSID filter with no SSID data lets *every*
// client through). See CONCEPT.md §7.2.
//
// Warnings — conditions that are suspicious but workable — are returned
// by [Config.Warnings] instead, so the caller can log them without
// refusing to start.
func (c *Config) Validate() error {
	var errs []string

	// --- required ---
	if strings.TrimSpace(c.Host) == "" {
		errs = append(errs, "HOST is required (the UniFi console's IP or hostname)")
	}
	if !c.APIKey.IsSet() {
		errs = append(errs, "API_KEY is required (Settings → Control Plane → Integrations)")
	}
	if strings.TrimSpace(c.MQTTServer) == "" {
		errs = append(errs, "MQTT_SERVER is required")
	}
	if strings.TrimSpace(c.MQTTTopic) == "" {
		errs = append(errs, "MQTT_TOPIC must not be empty")
	}
	if strings.TrimSpace(c.Site) == "" {
		errs = append(errs, "SITE must not be empty")
	}

	// --- ranges ---
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Sprintf("PORT %d is out of range 1..65535", c.Port))
	}
	if c.MQTTPort < 1 || c.MQTTPort > 65535 {
		errs = append(errs, fmt.Sprintf("MQTT_PORT %d is out of range 1..65535", c.MQTTPort))
	}
	if c.HTTPTimeout < 1 {
		errs = append(errs, "HTTP_TIMEOUT must be at least 1 second")
	}
	if c.HTTPRetries < 0 {
		errs = append(errs, "HTTP_RETRIES must not be negative")
	}

	// Rate-limit protection: polling faster than this cannot produce
	// meaningfully fresher data but can trip the console's limiter.
	for _, r := range []struct {
		key string
		val int
	}{
		{"REFRESH_DEVICES", c.RefreshDevices},
		{"REFRESH_CLIENTS", c.RefreshClients},
		{"REFRESH_HEALTH", c.RefreshHealth},
		{"REFRESH_STATIC", c.RefreshStatic},
	} {
		if r.val < MinRefreshSeconds {
			errs = append(errs, fmt.Sprintf("%s must be at least %d seconds, got %d",
				r.key, MinRefreshSeconds, r.val))
		}
	}
	// Zero means "inherit REFRESH_DEVICES", so only a positive value
	// below the floor is wrong.
	if c.RefreshDeviceStats > 0 && c.RefreshDeviceStats < MinRefreshSeconds {
		errs = append(errs, fmt.Sprintf("REFRESH_DEVICE_STATS must be at least %d seconds, got %d",
			MinRefreshSeconds, c.RefreshDeviceStats))
	}

	// --- classic API coupling ---
	if c.ClassicEnable {
		if strings.TrimSpace(c.ClassicUsername) == "" || !c.ClassicPassword.IsSet() {
			errs = append(errs,
				"CLASSIC_ENABLE requires CLASSIC_USERNAME and CLASSIC_PASSWORD (a local UniFi admin, 2FA off)")
		}
	} else {
		if len(c.Clients.SSIDs) > 0 {
			errs = append(errs,
				"CLIENTS.SSIDS needs CLASSIC_ENABLE: the Integration API does not report a client's SSID, "+
					"so the filter would match nothing and publish every client instead")
		}
		for _, ctl := range []struct {
			key string
			on  bool
		}{
			{"CONTROLS.DEVICE_LOCATE", c.Controls.DeviceLocate},
			{"CONTROLS.CLIENT_BLOCK", c.Controls.ClientBlock},
			{"CONTROLS.WLAN_ENABLE", c.Controls.WLANEnable},
		} {
			if ctl.on && c.Controls.Enable {
				errs = append(errs, ctl.key+" needs CLASSIC_ENABLE (not exposed by the Integration API)")
			}
		}
	}

	// --- clients ---
	if c.Clients.Max < 1 {
		errs = append(errs, "CLIENTS.MAX must be at least 1")
	}
	if c.Clients.AwayTimeout < 1 {
		errs = append(errs, "CLIENTS.AWAY_TIMEOUT must be at least 1 second")
	}
	errs = append(errs, validateEnums(c)...)
	errs = append(errs, validateMACLists(c)...)

	// --- misc ---
	switch c.Language {
	case "en", "de":
	default:
		errs = append(errs, fmt.Sprintf("LANGUAGE %q is not supported (want \"en\" or \"de\")", c.Language))
	}
	if c.HASSEnable && strings.TrimSpace(c.HASSBaseTopic) == "" {
		errs = append(errs, "HASS_BASE_TOPIC must not be empty when HASS_ENABLE is true")
	}
	if c.WebEnable && strings.TrimSpace(c.WebBind) == "" {
		errs = append(errs, "WEB_BIND must not be empty when WEB_ENABLE is true")
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w:\n  - %s", ErrInvalidConfig, strings.Join(errs, "\n  - "))
	}
	return nil
}

func validateEnums(c *Config) []string {
	var errs []string

	valid := map[string]bool{
		string(model.ClientWired):    true,
		string(model.ClientWireless): true,
		string(model.ClientVPN):      true,
		string(model.ClientTeleport): true,
	}
	for _, t := range c.Clients.Types {
		if !valid[strings.ToUpper(t)] {
			errs = append(errs, fmt.Sprintf(
				"CLIENTS.TYPES contains %q (want WIRED, WIRELESS, VPN or TELEPORT)", t,
			))
		}
	}

	for _, v := range c.Clients.VLANs {
		if v < 0 || v > 4094 {
			errs = append(errs, fmt.Sprintf("CLIENTS.VLANS contains %d (out of range 0..4094)", v))
		}
	}
	return errs
}

// validateMACLists rejects malformed addresses at startup. Left
// unchecked, a typo'd MAC in EXCLUDE_MACS would simply never match and
// the client the operator meant to hide would be published — a silent
// failure of exactly the kind this validation exists to prevent.
func validateMACLists(c *Config) []string {
	var errs []string
	for _, l := range []struct {
		key   string
		items []string
	}{
		{"CLIENTS.INCLUDE_MACS", c.Clients.IncludeMACs},
		{"CLIENTS.EXCLUDE_MACS", c.Clients.ExcludeMACs},
	} {
		for _, raw := range l.items {
			m, err := model.ParseMAC(raw)
			if err != nil || m.IsZero() {
				errs = append(errs, fmt.Sprintf("%s contains %q, which is not a MAC address", l.key, raw))
			}
		}
	}
	return errs
}

// Warnings returns configuration that is workable but questionable.
// The caller logs these at startup; none of them stop the daemon.
func (c *Config) Warnings() []string {
	var w []string

	if !c.VerifyTLS {
		w = append(w, "VERIFY_TLS is off — the console's certificate is not verified. "+
			"Set CA_FILE to the console's certificate to get verification back.")
	}
	if c.MQTTSSLInsecure && !c.MQTTSSL {
		w = append(w, "MQTT_SSL_INSECURE has no effect while MQTT_SSL is false")
	}
	if c.MQTTSSL && c.MQTTSSLInsecure {
		w = append(w, "MQTT_SSL_INSECURE is on — the broker's certificate is not verified")
	}

	// Wireless clients drop out of the client list for a cycle while
	// roaming between APs; without enough grace period every presence
	// automation flaps (CONCEPT.md §6.4).
	if c.Clients.Enable && c.Clients.AwayTimeout < 2*c.RefreshClients {
		w = append(w, fmt.Sprintf(
			"CLIENTS.AWAY_TIMEOUT (%ds) is below 2 × REFRESH_CLIENTS (%ds) — "+
				"presence will flap while clients roam between access points",
			c.Clients.AwayTimeout, 2*c.RefreshClients,
		))
	}
	if c.Clients.Enable && c.Clients.Max > 500 {
		w = append(w, fmt.Sprintf(
			"CLIENTS.MAX is %d — that many client entities will make Home Assistant sluggish",
			c.Clients.Max,
		))
	}
	if c.Clients.Enable && len(c.Clients.IncludeMACs) > 0 && hasOtherFilters(&c.Clients) {
		w = append(w, "CLIENTS.INCLUDE_MACS is set, so TYPES/NETWORKS/VLANS/SSIDS are ignored "+
			"— the MAC allowlist is an either/or, not an additional filter")
	}

	if c.Controls.Enable {
		w = append(w, "CONTROLS.ENABLE is on — anyone able to publish to the broker can "+
			"restart devices. Restrict write access with a broker ACL.")
	}
	if c.WebEnable && c.WebUser == "" && !c.WebPassword.IsSet() &&
		!strings.HasPrefix(c.WebBind, "127.0.0.1") && !strings.HasPrefix(c.WebBind, "localhost") {
		w = append(w, fmt.Sprintf(
			"the web UI is bound to %s without authentication — set WEB_USER and WEB_PASSWORD",
			c.WebBind,
		))
	}

	return w
}

func hasOtherFilters(c *ClientsConfig) bool {
	return len(c.Types) > 0 || len(c.Networks) > 0 || len(c.VLANs) > 0 || len(c.SSIDs) > 0
}
