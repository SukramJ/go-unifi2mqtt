// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package config holds the daemon's runtime settings.
//
// Values flow: defaults → YAML file → UNIFI_* env overrides →
// validation. The result is a single typed [Config] the rest of the
// daemon reads from. Env always wins over the file so the Home
// Assistant add-on and env-only `docker run` deployments need no config
// file at all (see CONCEPT.md §7).
//
// Duration-valued settings are stored as plain seconds in the YAML —
// that is what operators expect in a config file — and exposed through
// `…Duration()` accessors so callers never juggle raw ints.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Daemon-wide constants.
const (
	// EnvPrefix is prepended to every config key to form its environment
	// variable name, e.g. UNIFI_HOST.
	EnvPrefix = "UNIFI_"
	// AppDirName is the directory under $XDG_CONFIG_HOME the config file
	// is looked up in.
	AppDirName = "unifi2mqtt"
	// ConfigFile is the config file's name.
	ConfigFile = "config.yaml"
	// ClientIDPrefix seeds the MQTT client identifier.
	ClientIDPrefix = "unifi2mqtt-"
)

// Secret is a configuration value that must never reach a log line, an
// error message, the diagnostic web UI or an MQTT payload.
//
// It implements [fmt.Stringer], [json.Marshaler] and [slog.LogValuer],
// which covers every accidental path a value normally leaks through —
// `slog.Any("cfg", cfg)`, a `%v` in an error, a JSON dump of the config
// in the web UI. Reading the actual value requires the deliberate,
// greppable [Secret.Reveal].
type Secret string

const redacted = "***"

// String implements fmt.Stringer with the redacted placeholder.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

// MarshalJSON implements json.Marshaler with the redacted placeholder.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// LogValue implements slog.LogValuer with the redacted placeholder.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// Reveal returns the actual secret. Every call site is a deliberate
// decision to handle plaintext — keep them few and obvious.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether the secret carries a value.
func (s Secret) IsSet() bool { return s != "" }

// Config is the validated daemon configuration.
//
// The YAML keys are upper-case with underscores, matching the sibling
// projects; the two nested blocks (CLIENTS, CONTROLS) group settings
// that are meaningless on their own.
type Config struct {
	// --- UniFi console (Integration API) ---

	// Host is the console's IP or hostname.
	Host string `yaml:"HOST"`
	// Port is the HTTPS port: 443 on UniFi OS, 8443 on a standalone
	// software controller, 11443 on UniFi OS Server.
	Port int `yaml:"PORT"`
	// APIKey authenticates against the Integration API. Created in
	// Settings → Control Plane → Integrations; it inherits the creating
	// admin's permissions.
	APIKey Secret `yaml:"API_KEY"`
	// Site is the site to bridge, "default" on most installs.
	Site string `yaml:"SITE"`

	// VerifyTLS enables certificate verification for the console.
	// Off by default because consoles serve a self-signed certificate on
	// their LAN address; CAFile is the better way to get verification
	// back (CONCEPT.md §10).
	VerifyTLS bool `yaml:"VERIFY_TLS"`
	// CAFile is an optional PEM bundle trusted in addition to the system
	// roots. Setting it is preferable to VerifyTLS: false.
	CAFile string `yaml:"CA_FILE"`

	// HTTPTimeout is the per-request timeout in seconds.
	HTTPTimeout int `yaml:"HTTP_TIMEOUT"`
	// HTTPRetries is the retry count for 5xx and network errors. A 429
	// always honours Retry-After instead and does not consume a retry.
	HTTPRetries int `yaml:"HTTP_RETRIES"`

	// --- Classic controller API (optional fallback) ---

	// ClassicEnable turns on the cookie-session client against the
	// classic API, which supplies site health, per-client SSID/signal,
	// PoE power draw, client blocking and WLAN toggles (CONCEPT.md §2.2).
	ClassicEnable   bool   `yaml:"CLASSIC_ENABLE"`
	ClassicUsername string `yaml:"CLASSIC_USERNAME"`
	ClassicPassword Secret `yaml:"CLASSIC_PASSWORD"`

	// --- MQTT ---

	MQTTServer   string `yaml:"MQTT_SERVER"`
	MQTTPort     int    `yaml:"MQTT_PORT"`
	MQTTLogin    string `yaml:"MQTT_LOGIN"`
	MQTTPassword Secret `yaml:"MQTT_PASSWORD"`
	// MQTTTopic is the root of the published topic tree.
	MQTTTopic string `yaml:"MQTT_TOPIC"`
	// MQTTSSL dials tls:// instead of tcp://.
	MQTTSSL bool `yaml:"MQTT_SSL"`
	// MQTTSSLInsecure disables broker certificate verification. Only
	// meaningful together with MQTTSSL, and only ever for a self-signed
	// broker the operator controls.
	MQTTSSLInsecure bool `yaml:"MQTT_SSL_INSECURE"`

	// --- Home Assistant discovery ---

	HASSEnable    bool   `yaml:"HASS_ENABLE"`
	HASSBaseTopic string `yaml:"HASS_BASE_TOPIC"`
	// HASSBirthGracetime is the delay in seconds between Home Assistant
	// announcing "online" and the daemon republishing discovery.
	HASSBirthGracetime int `yaml:"HASS_BIRTH_GRACETIME"`
	// HASSCleanup enables the orphan reconcile: on start the daemon
	// reads the retained discovery configs under HASS_BASE_TOPIC and
	// clears the ones it owns but no longer announces.
	//
	// It exists as a switch because the sweep deletes Home Assistant
	// entities, and while it only ever touches configs carrying this
	// project's unique_id prefix *and* this bridge's availability topic,
	// an operator who wants nothing removed automatically should be able
	// to say so.
	HASSCleanup bool `yaml:"HASS_CLEANUP"`

	// --- Polling cadences (seconds) ---

	RefreshDevices int `yaml:"REFRESH_DEVICES"`
	// RefreshDeviceStats decouples the expensive per-device statistics
	// calls from the cheap device list. Zero means "same as
	// RefreshDevices" (CONCEPT.md §8.2).
	RefreshDeviceStats int `yaml:"REFRESH_DEVICE_STATS"`
	RefreshClients     int `yaml:"REFRESH_CLIENTS"`
	RefreshHealth      int `yaml:"REFRESH_HEALTH"`
	RefreshStatic      int `yaml:"REFRESH_STATIC"`
	// ForceRepublish is how often every value is republished even when
	// unchanged, so a subscriber without retained support cannot drift
	// permanently stale (CONCEPT.md §8.3).
	ForceRepublish int `yaml:"FORCE_REPUBLISH"`

	// --- Nested blocks ---

	Clients  ClientsConfig  `yaml:"CLIENTS"`
	Controls ControlsConfig `yaml:"CONTROLS"`

	// --- Web UI ---

	WebEnable   bool   `yaml:"WEB_ENABLE"`
	WebBind     string `yaml:"WEB_BIND"`
	WebUser     string `yaml:"WEB_USER"`
	WebPassword Secret `yaml:"WEB_PASSWORD"`

	// --- General ---

	// Language selects the display language for Home Assistant friendly
	// names and the web UI. Entity ids and unique_ids stay English
	// regardless, so switching never re-creates entities
	// (CONCEPT.md §6.2).
	Language string `yaml:"LANGUAGE"`
	Debug    bool   `yaml:"DEBUG"`
}

// ClientsConfig governs whether and which network clients are published.
//
// Defaults are deliberately restrictive: publishing every client of a
// busy network would create hundreds of Home Assistant entities on the
// first start (CONCEPT.md §6.3).
type ClientsConfig struct {
	Enable bool `yaml:"ENABLE"`

	// Types limits publication by connection type: WIRED, WIRELESS, VPN,
	// TELEPORT. Empty means no restriction on this dimension.
	Types []string `yaml:"TYPES"`
	// Networks and VLANs filter by the network a client's IP falls into,
	// resolved against the site's network catalogue.
	Networks []string `yaml:"NETWORKS"`
	VLANs    []int    `yaml:"VLANS"`
	// SSIDs filters wireless clients by SSID. Requires ClassicEnable —
	// the Integration API does not report a client's SSID.
	SSIDs []string `yaml:"SSIDS"`

	// IncludeMACs, when non-empty, restricts publication to exactly
	// these addresses and bypasses every other filter. It is an
	// either/or, not an additive.
	IncludeMACs []string `yaml:"INCLUDE_MACS"`
	// ExcludeMACs is applied last and always wins.
	ExcludeMACs []string `yaml:"EXCLUDE_MACS"`

	ExcludeGuests bool `yaml:"EXCLUDE_GUESTS"`

	// Max caps the number of published clients. Beyond it clients are
	// skipped with a warning rather than silently dropped.
	Max int `yaml:"MAX"`
	// AwayTimeout is how long a client may be absent from the poll
	// before presence flips to not_home, in seconds. Below
	// 2×RefreshClients this causes flapping while clients roam.
	AwayTimeout int `yaml:"AWAY_TIMEOUT"`

	// SignalSensor adds a signal-strength sensor per wireless client.
	// Requires CLASSIC_ENABLE — the Integration API does not report it.
	// Off by default because it is one more entity per client, and the
	// value only means anything for wireless ones.
	SignalSensor bool `yaml:"SIGNAL_SENSOR"`
}

// ControlsConfig selects which write-back entities are exposed. With
// Enable false the daemon is strictly read-only, which pairs with a
// read-only UniFi admin for the API key.
type ControlsConfig struct {
	Enable bool `yaml:"ENABLE"`

	DeviceRestart  bool `yaml:"DEVICE_RESTART"`
	PortPowerCycle bool `yaml:"PORT_POWER_CYCLE"`
	GuestAuthorize bool `yaml:"GUEST_AUTHORIZE"`

	// These three need the classic API layer.
	DeviceLocate bool `yaml:"DEVICE_LOCATE"`
	ClientBlock  bool `yaml:"CLIENT_BLOCK"`
	WLANEnable   bool `yaml:"WLAN_ENABLE"`
}

// BaseURL returns the console's origin, e.g. "https://192.168.1.1:443".
func (c *Config) BaseURL() string {
	return fmt.Sprintf("https://%s:%d", c.Host, c.Port)
}

// MQTTBrokerURL returns the broker URL in the scheme go-mqtt expects.
func (c *Config) MQTTBrokerURL() string {
	scheme := "tcp"
	if c.MQTTSSL {
		scheme = "tls"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, c.MQTTServer, c.MQTTPort)
}

// ClientID is the MQTT client identifier, derived from the topic root
// so two instances bridging different sites do not collide.
func (c *Config) ClientID() string { return ClientIDPrefix + c.MQTTTopic }

// Duration accessors. The YAML stores plain seconds; these are what the
// rest of the daemon consumes.

// HTTPTimeoutDuration is the per-request timeout.
func (c *Config) HTTPTimeoutDuration() time.Duration { return secs(c.HTTPTimeout) }

// RefreshDevicesDuration is the device-list poll interval.
func (c *Config) RefreshDevicesDuration() time.Duration { return secs(c.RefreshDevices) }

// RefreshDeviceStatsDuration is the per-device statistics poll
// interval, falling back to the device-list cadence when unset.
func (c *Config) RefreshDeviceStatsDuration() time.Duration {
	if c.RefreshDeviceStats <= 0 {
		return secs(c.RefreshDevices)
	}
	return secs(c.RefreshDeviceStats)
}

// RefreshClientsDuration is the client-list poll interval.
func (c *Config) RefreshClientsDuration() time.Duration { return secs(c.RefreshClients) }

// RefreshHealthDuration is the site-health poll interval.
func (c *Config) RefreshHealthDuration() time.Duration { return secs(c.RefreshHealth) }

// RefreshStaticDuration is the catalogue poll interval.
func (c *Config) RefreshStaticDuration() time.Duration { return secs(c.RefreshStatic) }

// ForceRepublishDuration is the unconditional republish interval.
func (c *Config) ForceRepublishDuration() time.Duration { return secs(c.ForceRepublish) }

// HASSBirthGracetimeDuration is the delay after Home Assistant's birth
// message.
func (c *Config) HASSBirthGracetimeDuration() time.Duration { return secs(c.HASSBirthGracetime) }

// AwayTimeoutDuration is the presence grace period.
func (c *ClientsConfig) AwayTimeoutDuration() time.Duration { return secs(c.AwayTimeout) }

func secs(n int) time.Duration { return time.Duration(n) * time.Second }
