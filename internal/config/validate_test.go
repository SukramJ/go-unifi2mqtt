// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRequiredFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		yaml     string
		wantHint string
	}{
		{
			"missing host",
			"API_KEY: k\nMQTT_SERVER: b\n",
			"HOST is required",
		},
		{
			"missing api key",
			"HOST: h\nMQTT_SERVER: b\n",
			"API_KEY is required",
		},
		{
			"missing broker",
			"HOST: h\nAPI_KEY: k\n",
			"MQTT_SERVER is required",
		},
		{
			"blank host is not a host",
			"HOST: \"   \"\nAPI_KEY: k\nMQTT_SERVER: b\n",
			"HOST is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadYAML(t, tt.yaml, nil)
			if err == nil {
				t.Fatal("Load succeeded, want a validation error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("error does not wrap ErrInvalidConfig: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantHint)
			}
		})
	}
}

// Every problem should be reported at once — an operator fixing a
// config one error per restart is a miserable experience.
func TestValidateReportsAllProblemsTogether(t *testing.T) {
	t.Parallel()

	_, err := loadYAML(t, "PORT: 0\nLANGUAGE: fr\n", nil)
	if err == nil {
		t.Fatal("Load succeeded, want validation errors")
	}
	msg := err.Error()
	for _, want := range []string{"HOST is required", "API_KEY is required", "MQTT_SERVER is required", "PORT 0", "LANGUAGE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q:\n%s", want, msg)
		}
	}
}

// The capability couplings from CONCEPT.md §7.2 — the reason
// validation exists at all. Each of these would otherwise fail
// silently and invert what the operator asked for.
func TestValidateClassicCapabilityCoupling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		yaml     string
		wantErr  bool
		wantHint string
	}{
		{
			name: "ssid filter without classic is rejected",
			yaml: minimal + `
CLIENTS:
  ENABLE: true
  SSIDS: ["Home"]
`,
			wantErr:  true,
			wantHint: "CLIENTS.SSIDS needs CLASSIC_ENABLE",
		},
		{
			name: "ssid filter with classic is fine",
			yaml: minimal + `
CLASSIC_ENABLE: true
CLASSIC_USERNAME: admin
CLASSIC_PASSWORD: pw
CLIENTS:
  ENABLE: true
  SSIDS: ["Home"]
`,
			wantErr: false,
		},
		{
			// VLAN and network filtering works on the official API via
			// IP-subnet mapping, so it must NOT require the classic layer.
			name: "vlan filter without classic is fine",
			yaml: minimal + `
CLIENTS:
  ENABLE: true
  VLANS: [20]
  NETWORKS: ["IoT"]
`,
			wantErr: false,
		},
		{
			name: "client blocking without classic is rejected",
			yaml: minimal + `
CONTROLS:
  ENABLE: true
  CLIENT_BLOCK: true
`,
			wantErr:  true,
			wantHint: "CONTROLS.CLIENT_BLOCK needs CLASSIC_ENABLE",
		},
		{
			name: "wlan toggle without classic is rejected",
			yaml: minimal + `
CONTROLS:
  ENABLE: true
  WLAN_ENABLE: true
`,
			wantErr:  true,
			wantHint: "CONTROLS.WLAN_ENABLE needs CLASSIC_ENABLE",
		},
		{
			// A classic-only control that is configured but never
			// exposed (CONTROLS.ENABLE off) is inert, not wrong.
			name: "classic-only control is fine while controls are off",
			yaml: minimal + `
CONTROLS:
  ENABLE: false
  CLIENT_BLOCK: true
`,
			wantErr: false,
		},
		{
			name: "classic without credentials is rejected",
			yaml: minimal + `
CLASSIC_ENABLE: true
`,
			wantErr:  true,
			wantHint: "CLASSIC_ENABLE requires CLASSIC_USERNAME and CLASSIC_PASSWORD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadYAML(t, tt.yaml, nil)
			switch {
			case tt.wantErr && err == nil:
				t.Fatal("Load succeeded, want a validation error")
			case !tt.wantErr && err != nil:
				t.Fatalf("Load failed unexpectedly: %v", err)
			case tt.wantErr && !strings.Contains(err.Error(), tt.wantHint):
				t.Errorf("error = %v, want it to mention %q", err, tt.wantHint)
			}
		})
	}
}

func TestValidateRefreshFloor(t *testing.T) {
	t.Parallel()

	if _, err := loadYAML(t, minimal+"REFRESH_CLIENTS: 1\n", nil); err == nil {
		t.Error("REFRESH_CLIENTS: 1 was accepted, want a rate-limit error")
	}
	if _, err := loadYAML(t, minimal+"REFRESH_CLIENTS: 5\n", nil); err != nil {
		t.Errorf("REFRESH_CLIENTS at the floor was rejected: %v", err)
	}
	// Zero means "inherit", not "poll continuously".
	if _, err := loadYAML(t, minimal+"REFRESH_DEVICE_STATS: 0\n", nil); err != nil {
		t.Errorf("REFRESH_DEVICE_STATS: 0 was rejected: %v", err)
	}
	if _, err := loadYAML(t, minimal+"REFRESH_DEVICE_STATS: 2\n", nil); err == nil {
		t.Error("REFRESH_DEVICE_STATS: 2 was accepted, want a rate-limit error")
	}
}

func TestValidateEnums(t *testing.T) {
	t.Parallel()

	if _, err := loadYAML(t, minimal+"CLIENTS:\n  TYPES: [WIRELESS, BLUETOOTH]\n", nil); err == nil {
		t.Error("an unknown client type was accepted")
	}
	if _, err := loadYAML(t, minimal+"CLIENTS:\n  TYPES: [wireless]\n", nil); err != nil {
		t.Errorf("a lowercase client type was rejected: %v", err)
	}
	if _, err := loadYAML(t, minimal+"CLIENTS:\n  VLANS: [5000]\n", nil); err == nil {
		t.Error("an out-of-range VLAN was accepted")
	}
	if _, err := loadYAML(t, minimal+"LANGUAGE: fr\n", nil); err == nil {
		t.Error("an unsupported language was accepted")
	}
}

// A typo'd MAC would never match, so the client the operator meant to
// exclude would be published — exactly the silent failure validation is
// meant to catch.
func TestValidateMACLists(t *testing.T) {
	t.Parallel()

	_, err := loadYAML(t, minimal+"CLIENTS:\n  EXCLUDE_MACS: [\"aa:bb:cc:dd:ee\"]\n", nil)
	if err == nil {
		t.Fatal("a malformed MAC was accepted")
	}
	if !strings.Contains(err.Error(), "CLIENTS.EXCLUDE_MACS") {
		t.Errorf("error = %v, want it to name the offending key", err)
	}

	// Every notation the two API flavours use must be accepted.
	for _, m := range []string{"aa:bb:cc:dd:ee:ff", "AABBCCDDEEFF", "aa-bb-cc-dd-ee-ff"} {
		if _, err := loadYAML(t, minimal+"CLIENTS:\n  INCLUDE_MACS: [\""+m+"\"]\n", nil); err != nil {
			t.Errorf("MAC %q was rejected: %v", m, err)
		}
	}
}

func TestWarnings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		yaml     string
		wantHint string
		want     bool
	}{
		{
			// The default (no TLS verification) is a real risk worth
			// stating once, but not worth refusing to start over.
			name:     "unverified TLS warns",
			yaml:     minimal,
			wantHint: "VERIFY_TLS is off",
			want:     true,
		},
		{
			name:     "verified TLS does not warn",
			yaml:     minimal + "VERIFY_TLS: true\n",
			wantHint: "VERIFY_TLS is off",
			want:     false,
		},
		{
			name: "away timeout below two poll cycles warns",
			yaml: minimal + `
REFRESH_CLIENTS: 30
CLIENTS:
  ENABLE: true
  AWAY_TIMEOUT: 30
`,
			wantHint: "presence will flap",
			want:     true,
		},
		{
			name: "sufficient away timeout does not warn",
			yaml: minimal + `
REFRESH_CLIENTS: 30
CLIENTS:
  ENABLE: true
  AWAY_TIMEOUT: 300
`,
			wantHint: "presence will flap",
			want:     false,
		},
		{
			name: "MAC allowlist alongside other filters warns",
			yaml: minimal + `
CLIENTS:
  ENABLE: true
  TYPES: [WIRELESS]
  INCLUDE_MACS: ["aa:bb:cc:dd:ee:ff"]
`,
			wantHint: "either/or",
			want:     true,
		},
		{
			name:     "enabling controls warns about broker ACLs",
			yaml:     minimal + "CONTROLS:\n  ENABLE: true\n",
			wantHint: "broker ACL",
			want:     true,
		},
		{
			name: "web UI exposed without auth warns",
			yaml: minimal + `
WEB_ENABLE: true
WEB_BIND: "0.0.0.0:8080"
`,
			wantHint: "without authentication",
			want:     true,
		},
		{
			name: "localhost web UI without auth does not warn",
			yaml: minimal + `
WEB_ENABLE: true
`,
			wantHint: "without authentication",
			want:     false,
		},
		{
			name:     "insecure MQTT TLS without TLS warns it is inert",
			yaml:     minimal + "MQTT_SSL_INSECURE: true\n",
			wantHint: "has no effect",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := mustLoad(t, tt.yaml, nil)
			got := false
			for _, w := range cfg.Warnings() {
				if strings.Contains(w, tt.wantHint) {
					got = true
					break
				}
			}
			if got != tt.want {
				t.Errorf("warning containing %q present = %v, want %v\nwarnings: %v",
					tt.wantHint, got, tt.want, cfg.Warnings())
			}
		})
	}
}

// Warnings are for the log, so they must never contain a secret.
func TestWarningsCarryNoSecrets(t *testing.T) {
	t.Parallel()

	cfg := mustLoad(t, `
HOST: h
API_KEY: super-secret-key
MQTT_SERVER: b
MQTT_PASSWORD: broker-pw
WEB_ENABLE: true
WEB_BIND: "0.0.0.0:8080"
CONTROLS:
  ENABLE: true
`, nil)

	all := strings.Join(cfg.Warnings(), "\n")
	for _, secret := range []string{"super-secret-key", "broker-pw"} {
		if strings.Contains(all, secret) {
			t.Errorf("warnings leaked %q:\n%s", secret, all)
		}
	}
}
