// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"errors"
	"strings"
	"testing"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

func TestTopicBuilder(t *testing.T) {
	t.Parallel()

	b := newTopicBuilder("unifi", "default")
	mac := model.MustParseMAC("aa:bb:cc:dd:ee:ff")

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"connected", b.connected(), "unifi/connected"},
		{"info", b.layout.Info(), "unifi/info"},
		{"bridge error", b.bridgeError(), "unifi/status/bridge/error"},
		{"device", b.device(mac, keyState), "unifi/status/default/device/aabbccddeeff/state"},
		{"device online", b.device(mac, keyOnline), "unifi/status/default/device/aabbccddeeff/online"},
		{"device prefix", b.devicePrefix(mac), "unifi/status/default/device/aabbccddeeff/"},
		{"port", b.port(mac, 3, keyPortPoE), "unifi/status/default/device/aabbccddeeff/port/3/poe"},
		{"port power", b.port(mac, 3, keyPortPoEPower), "unifi/status/default/device/aabbccddeeff/port/3/poe/power_w"},
		{"radio 5g", b.radio(mac, 5, keyRadioChannel), "unifi/status/default/device/aabbccddeeff/radio/5g/channel"},
		{"radio 2.4g", b.radio(mac, 2.4, keyRadioChannel), "unifi/status/default/device/aabbccddeeff/radio/2g4/channel"},
		{"radio 6g", b.radio(mac, 6, keyRadioChannel), "unifi/status/default/device/aabbccddeeff/radio/6g/channel"},
		{"wlan", b.wlan("w-1", keyWLANEnabled), "unifi/status/default/wlan/w-1/enabled"},
		{"client", b.client("aabbccddeeff", keyState), "unifi/status/default/client/aabbccddeeff/state"},
		{"health", b.health(keyWANState), "unifi/status/default/health/wan/state"},
		// Commands share the item path, under set.
		{"restart", b.deviceCommand(mac, cmdRestart), "unifi/set/default/device/aabbccddeeff/cmd/restart"},
		{"locate", b.deviceCommand(mac, cmdLocate), "unifi/set/default/device/aabbccddeeff/cmd/locate"},
		{"power cycle", b.deviceCommand(mac, "port/3/"+cmdPowerCycle), "unifi/set/default/device/aabbccddeeff/port/3/cmd/power_cycle"},
		{"blocked", b.clientCommand("aabbccddeeff", cmdBlocked), "unifi/set/default/client/aabbccddeeff/blocked"},
		{"authorize", b.clientCommand("aabbccddeeff", cmdAuthorize), "unifi/set/default/client/aabbccddeeff/cmd/authorize"},
		{"wlan enable", b.wlanCommand("w-1", cmdWLANEnabled), "unifi/set/default/wlan/w-1/enabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("= %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// The MAC goes into the topic without separators: a colon is legal in
// MQTT but awkward in tooling, and the same string seeds the Home
// Assistant unique_id.
func TestDeviceTopicUsesBareMAC(t *testing.T) {
	t.Parallel()

	b := newTopicBuilder("unifi", "default")
	got := b.device(model.MustParseMAC("AA:BB:CC:DD:EE:FF"), keyState)
	if strings.Contains(got, ":") {
		t.Errorf("topic %q contains a colon", got)
	}
	if !strings.Contains(got, "aabbccddeeff") {
		t.Errorf("topic %q does not carry the normalised MAC", got)
	}
}

// A site or topic root containing a wildcard or a slash would either be
// rejected by the broker or silently create a topic level the rest of
// the code does not know about.
func TestSanitiseSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"default", "default"},
		{"My Site", "My_Site"},
		{"a/b", "a_b"},
		{"wild+card", "wild_card"},
		{"hash#tag", "hash_tag"},
		{"  padded  ", "padded"},
		{"", "_"},
		{"   ", "_"},
		{"ctrl\x01char", "ctrlchar"},
	}
	for _, tt := range tests {
		if got := sanitiseSegment(tt.in); got != tt.want {
			t.Errorf("sanitiseSegment(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuilderSanitisesRootAndSite(t *testing.T) {
	t.Parallel()

	// A site literally named "a/b" would otherwise shift every device
	// topic one level deeper.
	b := newTopicBuilder("uni/fi", "a/b")
	got := b.device(model.MustParseMAC("aabbccddeeff"), keyState)
	if got != "uni_fi/status/a_b/device/aabbccddeeff/state" {
		t.Errorf("= %q, want the separators replaced", got)
	}
	if strings.Count(got, "/") != 5 {
		t.Errorf("topic %q has %d levels, want 6 segments", got, strings.Count(got, "/")+1)
	}
}

// The reserved-name guard (openccu-loom ADR 0083): a site whose
// topic-safe form is `bridge`, `alarm`, `security`, `system` or a
// function name would share a level with a literal item or make the
// migration sweep's "second level is a function ⇒ new" rule unsound.
// It is refused at start, by the reference that goes into the topic.
func TestCheckTopicsRefusesReservedSites(t *testing.T) {
	t.Parallel()

	for _, ref := range []string{
		"bridge", "alarm", "security", "system",
		"connected", "status", "set", "get", "info", "meta", "maintenance",
		" status ", // sanitised to "status"
	} {
		err := CheckTopics("unifi", model.Site{ID: "uuid", Name: "Home", Internal: ref})
		if !errors.Is(err, ErrReservedSite) {
			t.Errorf("site %q: CheckTopics = %v, want ErrReservedSite", ref, err)
			continue
		}
		if !strings.Contains(err.Error(), "SITE") {
			t.Errorf("site %q: the refusal does not name SITE: %v", ref, err)
		}
	}

	for _, ref := range []string{"default", "Bridge", "bridges", "home_office", "a1b2c3"} {
		if err := CheckTopics("unifi", model.Site{Internal: ref}); err != nil {
			t.Errorf("site %q refused: %v", ref, err)
		}
	}

	// The display name and SITE do not decide: only the reference does.
	if err := CheckTopics("unifi", model.Site{Name: "status", Internal: "default"}); err != nil {
		t.Errorf("a site displayed as \"status\" with reference default was refused: %v", err)
	}
}

// The name is sanitised to one segment, as it always was, and then
// validated by the convention: the only thing left to refuse is a
// leading `$`, which MQTT reserves for the broker.
func TestCheckTopicsValidatesTheName(t *testing.T) {
	t.Parallel()

	site := model.Site{Internal: "default"}
	if err := CheckTopics("$SYS", site); err == nil {
		t.Error("a name starting with $ was accepted")
	}
	for _, name := range []string{"unifi", "unifi-garage", "uni/fi", "uni+fi"} {
		if err := CheckTopics(name, site); err != nil {
			t.Errorf("name %q refused: %v", name, err)
		}
	}
}
