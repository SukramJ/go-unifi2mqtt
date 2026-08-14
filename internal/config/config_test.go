// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// minimal is the smallest config that passes validation. Tests start
// from it and change one thing at a time.
const minimal = `
HOST: 192.168.1.1
API_KEY: secret-key
MQTT_SERVER: broker.local
`

func loadYAML(t *testing.T, yaml string, env MapEnv) (*Config, error) {
	t.Helper()
	if env == nil {
		env = MapEnv{}
	}
	return Load(strings.NewReader(yaml), env)
}

func mustLoad(t *testing.T, yaml string, env MapEnv) *Config {
	t.Helper()
	cfg, err := loadYAML(t, yaml, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal, nil)

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Port", cfg.Port, DefaultPort},
		{"Site", cfg.Site, DefaultSite},
		{"HTTPTimeout", cfg.HTTPTimeout, DefaultHTTPTimeout},
		{"MQTTPort", cfg.MQTTPort, DefaultMQTTPort},
		{"MQTTTopic", cfg.MQTTTopic, DefaultMQTTTopic},
		{"RefreshDevices", cfg.RefreshDevices, DefaultRefreshDevices},
		{"RefreshClients", cfg.RefreshClients, DefaultRefreshClients},
		{"Clients.Max", cfg.Clients.Max, DefaultClientsMax},
		{"Clients.AwayTimeout", cfg.Clients.AwayTimeout, DefaultClientsAwayTimeout},
		{"Language", cfg.Language, DefaultLanguage},
		{"WebBind", cfg.WebBind, DefaultWebBind},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// Safe-by-default: nothing is published or controllable until asked.
	if cfg.Clients.Enable {
		t.Error("Clients.Enable defaults to true, want false")
	}
	if cfg.Controls.Enable {
		t.Error("Controls.Enable defaults to true, want false")
	}
	if cfg.ClassicEnable {
		t.Error("ClassicEnable defaults to true, want false")
	}
}

// A key present in the file must win over the default even when it sets
// the zero value — the classic trap of "defaulting after decode".
func TestLoadExplicitZeroBeatsDefault(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal+`
CLIENTS:
  EXCLUDE_GUESTS: false
`, nil)

	if cfg.Clients.ExcludeGuests {
		t.Error("CLIENTS.EXCLUDE_GUESTS: false was overwritten by the default true")
	}
}

func TestLoadEmptyReaderIsEnvOnly(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, "", MapEnv{
		"UNIFI_HOST":        "10.0.0.1",
		"UNIFI_API_KEY":     "k",
		"UNIFI_MQTT_SERVER": "broker",
	})

	if cfg.Host != "10.0.0.1" {
		t.Errorf("Host = %q, want 10.0.0.1", cfg.Host)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want the default %d", cfg.Port, DefaultPort)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal+`
PORT: 8443
SITE: office
CLIENTS:
  ENABLE: false
  TYPES: [WIRED]
  VLANS: [1]
`, MapEnv{
		"UNIFI_PORT":           "11443",
		"UNIFI_SITE":           "home",
		"UNIFI_CLIENTS_ENABLE": "true",
		"UNIFI_CLIENTS_TYPES":  "WIRELESS,VPN",
		"UNIFI_CLIENTS_VLANS":  "10, 20 ,30",
	})

	if cfg.Port != 11443 {
		t.Errorf("Port = %d, want 11443 (env must win)", cfg.Port)
	}
	if cfg.Site != "home" {
		t.Errorf("Site = %q, want home", cfg.Site)
	}
	if !cfg.Clients.Enable {
		t.Error("Clients.Enable = false, want true from env")
	}
	if got, want := strings.Join(cfg.Clients.Types, ","), "WIRELESS,VPN"; got != want {
		t.Errorf("Clients.Types = %q, want %q", got, want)
	}
	if got, want := len(cfg.Clients.VLANs), 3; got != want {
		t.Fatalf("Clients.VLANs has %d entries, want %d", got, want)
	}
	if cfg.Clients.VLANs[1] != 20 {
		t.Errorf("Clients.VLANs[1] = %d, want 20 (whitespace must be trimmed)", cfg.Clients.VLANs[1])
	}
}

// An empty list variable is how an operator says "no restriction on
// this dimension" — it has to clear the default, not be ignored.
func TestEnvEmptyListClearsDefault(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal, MapEnv{"UNIFI_CLIENTS_TYPES": ""})
	if len(cfg.Clients.Types) != 0 {
		t.Errorf("Clients.Types = %v, want empty", cfg.Clients.Types)
	}
}

func TestEnvBooleanForms(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"true", "TRUE", "1", "yes", "on", "y", "t"} {
		cfg := mustLoad(t, minimal, MapEnv{"UNIFI_DEBUG": v})
		if !cfg.Debug {
			t.Errorf("UNIFI_DEBUG=%q did not parse as true", v)
		}
	}
	for _, v := range []string{"false", "FALSE", "0", "no", "off", "n", "f"} {
		cfg := mustLoad(t, minimal, MapEnv{"UNIFI_DEBUG": v})
		if cfg.Debug {
			t.Errorf("UNIFI_DEBUG=%q did not parse as false", v)
		}
	}
}

func TestEnvInvalidValueIsAnError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ key, val string }{
		{"UNIFI_PORT", "not-a-number"},
		{"UNIFI_DEBUG", "maybe"},
		{"UNIFI_CLIENTS_VLANS", "10,twenty"},
	} {
		if _, err := loadYAML(t, minimal, MapEnv{tc.key: tc.val}); err == nil {
			t.Errorf("%s=%q was accepted, want an error", tc.key, tc.val)
		}
	}
}

// Enabling TLS without also naming a port should land on 8883 rather
// than trying TLS against the plaintext port.
func TestMQTTSSLPicksTheTLSPort(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal+"MQTT_SSL: true\n", nil)
	if cfg.MQTTPort != DefaultMQTTSSLPort {
		t.Errorf("MQTTPort = %d, want %d", cfg.MQTTPort, DefaultMQTTSSLPort)
	}
	if got, want := cfg.MQTTBrokerURL(), "tls://broker.local:8883"; got != want {
		t.Errorf("MQTTBrokerURL() = %q, want %q", got, want)
	}

	// An explicit port must survive.
	cfg = mustLoad(t, minimal+"MQTT_SSL: true\nMQTT_PORT: 9999\n", nil)
	if cfg.MQTTPort != 9999 {
		t.Errorf("MQTTPort = %d, want the explicit 9999", cfg.MQTTPort)
	}
}

func TestDerivedValues(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal+"PORT: 8443\nMQTT_TOPIC: unifi-home\n", nil)

	if got, want := cfg.BaseURL(), "https://192.168.1.1:8443"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got, want := cfg.MQTTBrokerURL(), "tcp://broker.local:1883"; got != want {
		t.Errorf("MQTTBrokerURL() = %q, want %q", got, want)
	}
	if got, want := cfg.ClientID(), "unifi2mqtt-unifi-home"; got != want {
		t.Errorf("ClientID() = %q, want %q", got, want)
	}
}

// REFRESH_DEVICE_STATS is opt-in: unset it must follow REFRESH_DEVICES
// rather than collapsing to a zero interval.
func TestRefreshDeviceStatsFallsBack(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal+"REFRESH_DEVICES: 90\n", nil)
	if got, want := cfg.RefreshDeviceStatsDuration(), cfg.RefreshDevicesDuration(); got != want {
		t.Errorf("RefreshDeviceStatsDuration() = %v, want %v", got, want)
	}

	cfg = mustLoad(t, minimal+"REFRESH_DEVICES: 90\nREFRESH_DEVICE_STATS: 300\n", nil)
	if got, want := cfg.RefreshDeviceStatsDuration().Seconds(), 300.0; got != want {
		t.Errorf("RefreshDeviceStatsDuration() = %v s, want %v s", got, want)
	}
}

func TestSecretRedaction(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, minimal, nil)

	if got := cfg.APIKey.Reveal(); got != "secret-key" {
		t.Fatalf("Reveal() = %q, want the plaintext", got)
	}

	// The three paths a secret normally leaks through.
	if got := cfg.APIKey.String(); got != redacted {
		t.Errorf("String() = %q, want %q", got, redacted)
	}
	if s := fmt.Sprintf("%v/%s", cfg.APIKey, cfg.APIKey); strings.Contains(s, "secret-key") {
		t.Errorf("fmt formatting leaked the secret: %s", s)
	}
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(blob, []byte("secret-key")) {
		t.Errorf("JSON encoding leaked the secret: %s", blob)
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("test", slog.Any("cfg", cfg))
	if strings.Contains(buf.String(), "secret-key") {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
}

// An unset secret must stay empty rather than logging as "***", which
// would make "no password configured" look like "password configured".
func TestEmptySecretStaysEmpty(t *testing.T) {
	t.Parallel()

	var s Secret
	if got := s.String(); got != "" {
		t.Errorf("String() = %q for an unset secret, want empty", got)
	}
	if s.IsSet() {
		t.Error("IsSet() = true for an unset secret")
	}
}
