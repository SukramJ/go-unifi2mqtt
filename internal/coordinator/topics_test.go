// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
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
		{"bridge", b.bridge(statusKey), "unifi/bridge/status"},
		{"device", b.device(mac, keyState), "unifi/default/device/aabbccddeeff/state"},
		{"device prefix", b.devicePrefix(mac), "unifi/default/device/aabbccddeeff/"},
		{"port", b.port(mac, 3, keyPortPoE), "unifi/default/device/aabbccddeeff/port/3/poe"},
		{"radio 5g", b.radio(mac, 5, keyRadioChannel), "unifi/default/device/aabbccddeeff/radio/5g/channel"},
		{"radio 2.4g", b.radio(mac, 2.4, keyRadioChannel), "unifi/default/device/aabbccddeeff/radio/2g4/channel"},
		{"radio 6g", b.radio(mac, 6, keyRadioChannel), "unifi/default/device/aabbccddeeff/radio/6g/channel"},
		{"wlan", b.wlan("w-1", keyWLANEnabled), "unifi/default/wlan/w-1/enabled"},
		{"client", b.client("aabbccddeeff", keyState), "unifi/default/client/aabbccddeeff/state"},
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
	if got != "uni_fi/a_b/device/aabbccddeeff/state" {
		t.Errorf("= %q, want the separators replaced", got)
	}
	if strings.Count(got, "/") != 4 {
		t.Errorf("topic %q has %d levels, want 5 segments", got, strings.Count(got, "/")+1)
	}
}

func TestBoolPayload(t *testing.T) {
	t.Parallel()

	if got := boolPayload(true); got != "ON" {
		t.Errorf("boolPayload(true) = %q, want ON", got)
	}
	if got := boolPayload(false); got != "OFF" {
		t.Errorf("boolPayload(false) = %q, want OFF", got)
	}
}
