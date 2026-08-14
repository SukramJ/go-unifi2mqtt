// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Env abstracts environment lookup so tests do not have to mutate the
// process environment (and can therefore run in parallel).
type Env interface {
	Lookup(key string) (string, bool)
}

// OSEnv reads the real process environment.
type OSEnv struct{}

// Lookup implements Env against os.LookupEnv.
func (OSEnv) Lookup(key string) (string, bool) { return os.LookupEnv(key) }

// MapEnv is an in-memory Env for tests.
type MapEnv map[string]string

// Lookup implements Env against the map.
func (m MapEnv) Lookup(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// Locate finds the config file using the standard search order and
// reports whether one was found:
//
//  1. $XDG_CONFIG_HOME/unifi2mqtt/config.yaml (%APPDATA% on Windows)
//  2. ~/.config/unifi2mqtt/config.yaml
//
// An explicit --config path is handled by the caller and never reaches
// this function.
func Locate(env Env) (string, bool) {
	var candidates []string

	if base, ok := env.Lookup(configHomeVar()); ok && base != "" {
		candidates = append(candidates, filepath.Join(base, AppDirName, ConfigFile))
	}
	if home, ok := env.Lookup(homeVar()); ok && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", AppDirName, ConfigFile))
	}

	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

func configHomeVar() string {
	if runtime.GOOS == "windows" {
		return "APPDATA"
	}
	return "XDG_CONFIG_HOME"
}

func homeVar() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

// Option adjusts how a config is loaded and validated.
type Option func(*options)

type options struct {
	skipMQTT bool
}

// WithoutMQTT relaxes validation for callers that never open a broker
// connection — currently the `--once` inventory dump, which only talks
// to the console.
//
// Without this, a diagnostic run would demand MQTT_SERVER and push
// operators towards putting a placeholder in their real config just to
// get past the check. A placeholder that later gets forgotten is worse
// than the missing validation.
func WithoutMQTT() Option {
	return func(o *options) { o.skipMQTT = true }
}

func newOptions(opts []Option) options {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// LoadFile reads and parses the config file at path, then applies the
// env overlay and validation.
func LoadFile(path string, env Env, opts ...Option) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied config path is the point
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	cfg, err := Load(f, env, opts...)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Load parses YAML from r, overlays UNIFI_* environment variables and
// validates the result.
//
// An empty reader is valid and yields a config built from defaults and
// env alone — that is the path the Home Assistant add-on and env-only
// container deployments take.
func Load(r io.Reader, env Env, opts ...Option) (*Config, error) {
	cfg := defaults()

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if strings.TrimSpace(string(raw)) != "" {
		// Decoding into the already-defaulted struct means an absent key
		// keeps its default while a present one wins, including when it
		// sets false or 0.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
	}

	if err := applyEnv(&cfg, env); err != nil {
		return nil, err
	}

	// The MQTT default port depends on a setting that may only be known
	// after the overlay, so it cannot live in defaults().
	if cfg.MQTTSSL && cfg.MQTTPort == DefaultMQTTPort {
		cfg.MQTTPort = DefaultMQTTSSLPort
	}

	if err := cfg.validate(newOptions(opts)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnv overlays UNIFI_* variables onto cfg.
//
// The mapping is an explicit table rather than reflection over struct
// tags: nested keys need a flattened name (CLIENTS.TYPES becomes
// UNIFI_CLIENTS_TYPES) and list values need a parse rule, neither of
// which a tag can express. An explicit table also means an unknown
// variable is simply ignored instead of silently mis-assigned.
func applyEnv(cfg *Config, env Env) error {
	setters := map[string]func(string) error{
		// UniFi console
		"HOST":         str(&cfg.Host),
		"PORT":         num(&cfg.Port),
		"API_KEY":      secret(&cfg.APIKey),
		"SITE":         str(&cfg.Site),
		"VERIFY_TLS":   boolean(&cfg.VerifyTLS),
		"CA_FILE":      str(&cfg.CAFile),
		"HTTP_TIMEOUT": num(&cfg.HTTPTimeout),
		"HTTP_RETRIES": num(&cfg.HTTPRetries),

		// Classic API
		"CLASSIC_ENABLE":   boolean(&cfg.ClassicEnable),
		"CLASSIC_USERNAME": str(&cfg.ClassicUsername),
		"CLASSIC_PASSWORD": secret(&cfg.ClassicPassword),

		// MQTT
		"MQTT_SERVER":       str(&cfg.MQTTServer),
		"MQTT_PORT":         num(&cfg.MQTTPort),
		"MQTT_LOGIN":        str(&cfg.MQTTLogin),
		"MQTT_PASSWORD":     secret(&cfg.MQTTPassword),
		"MQTT_TOPIC":        str(&cfg.MQTTTopic),
		"MQTT_SSL":          boolean(&cfg.MQTTSSL),
		"MQTT_SSL_INSECURE": boolean(&cfg.MQTTSSLInsecure),

		// Home Assistant
		"HASS_ENABLE":          boolean(&cfg.HASSEnable),
		"HASS_BASE_TOPIC":      str(&cfg.HASSBaseTopic),
		"HASS_BIRTH_GRACETIME": num(&cfg.HASSBirthGracetime),

		// Polling
		"REFRESH_DEVICES":      num(&cfg.RefreshDevices),
		"REFRESH_DEVICE_STATS": num(&cfg.RefreshDeviceStats),
		"REFRESH_CLIENTS":      num(&cfg.RefreshClients),
		"REFRESH_HEALTH":       num(&cfg.RefreshHealth),
		"REFRESH_STATIC":       num(&cfg.RefreshStatic),
		"FORCE_REPUBLISH":      num(&cfg.ForceRepublish),

		// Clients
		"CLIENTS_ENABLE":         boolean(&cfg.Clients.Enable),
		"CLIENTS_TYPES":          list(&cfg.Clients.Types),
		"CLIENTS_NETWORKS":       list(&cfg.Clients.Networks),
		"CLIENTS_VLANS":          intList(&cfg.Clients.VLANs),
		"CLIENTS_SSIDS":          list(&cfg.Clients.SSIDs),
		"CLIENTS_INCLUDE_MACS":   list(&cfg.Clients.IncludeMACs),
		"CLIENTS_EXCLUDE_MACS":   list(&cfg.Clients.ExcludeMACs),
		"CLIENTS_EXCLUDE_GUESTS": boolean(&cfg.Clients.ExcludeGuests),
		"CLIENTS_MAX":            num(&cfg.Clients.Max),
		"CLIENTS_AWAY_TIMEOUT":   num(&cfg.Clients.AwayTimeout),
		"CLIENTS_SIGNAL_SENSOR":  boolean(&cfg.Clients.SignalSensor),

		// Controls
		"CONTROLS_ENABLE":           boolean(&cfg.Controls.Enable),
		"CONTROLS_DEVICE_RESTART":   boolean(&cfg.Controls.DeviceRestart),
		"CONTROLS_PORT_POWER_CYCLE": boolean(&cfg.Controls.PortPowerCycle),
		"CONTROLS_GUEST_AUTHORIZE":  boolean(&cfg.Controls.GuestAuthorize),
		"CONTROLS_DEVICE_LOCATE":    boolean(&cfg.Controls.DeviceLocate),
		"CONTROLS_CLIENT_BLOCK":     boolean(&cfg.Controls.ClientBlock),
		"CONTROLS_WLAN_ENABLE":      boolean(&cfg.Controls.WLANEnable),

		// Web UI
		"WEB_ENABLE":   boolean(&cfg.WebEnable),
		"WEB_BIND":     str(&cfg.WebBind),
		"WEB_USER":     str(&cfg.WebUser),
		"WEB_PASSWORD": secret(&cfg.WebPassword),

		// General
		"LANGUAGE": str(&cfg.Language),
		"DEBUG":    boolean(&cfg.Debug),
	}

	for key, set := range setters {
		v, ok := env.Lookup(EnvPrefix + key)
		if !ok {
			continue
		}
		if err := set(v); err != nil {
			return fmt.Errorf("config: env %s%s: %w", EnvPrefix, key, err)
		}
	}
	return nil
}

func str(dst *string) func(string) error {
	return func(v string) error {
		*dst = v
		return nil
	}
}

func secret(dst *Secret) func(string) error {
	return func(v string) error {
		*dst = Secret(v)
		return nil
	}
}

func num(dst *int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("want an integer, got %q", v)
		}
		*dst = n
		return nil
	}
}

// boolean accepts everything strconv.ParseBool does plus the words
// bashio emits for add-on toggles.
func boolean(dst *bool) func(string) error {
	return func(v string) error {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "t", "true", "yes", "y", "on":
			*dst = true
		case "0", "f", "false", "no", "n", "off":
			*dst = false
		default:
			return fmt.Errorf("want a boolean, got %q", v)
		}
		return nil
	}
}

// list parses a comma-separated value. An empty string clears the list
// rather than leaving the default in place — "UNIFI_CLIENTS_TYPES=" is
// how an operator says "no restriction on this dimension".
func list(dst *[]string) func(string) error {
	return func(v string) error {
		*dst = splitList(v)
		return nil
	}
}

func intList(dst *[]int) func(string) error {
	return func(v string) error {
		parts := splitList(v)
		out := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return fmt.Errorf("want a comma-separated integer list, got %q", v)
			}
			out = append(out, n)
		}
		*dst = out
		return nil
	}
}

func splitList(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
