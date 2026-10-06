// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// frozenIdentity is one discovery config's Home Assistant identity:
// where the config is retained, and every string the entity and device
// registries key on.
type frozenIdentity struct {
	Topic           string     `json:"topic"`
	UniqueID        string     `json:"unique_id"`
	DefaultEntityID string     `json:"default_entity_id"`
	Identifiers     []string   `json:"identifiers"`
	Connections     [][]string `json:"connections"`
	ViaDevice       *string    `json:"via_device"`
}

// frozenIdentities is testdata/frozen_v1.3.0_identity.json: the
// identities 1.3.0 published, extracted from its surface goldens before
// the mqtt-smarthome 2.0 move and never regenerated since.
type frozenIdentities struct {
	Scenarios []struct {
		Scenario string           `json:"scenario"`
		Configs  []frozenIdentity `json:"configs"`
	} `json:"scenarios"`
}

// TestHomeAssistantIdentitiesAreThoseOf130 is openccu-loom ADR 0083's
// one hard constraint on the Home Assistant side: the topic move
// re-points entities, it does not re-register them.
//
// Home Assistant keys the entity registry on unique_id and the device
// registry on identifiers, and has no migration for either; a config
// retained at a different topic is a second entity. So for every
// discovery config of the device, client, SSID and site planes the
// rendering must reproduce, string for string, what 1.3.0 published —
// the config topic (and with it the node id), unique_id,
// default_entity_id, device identifiers, connections and via_device —
// while every state, command and availability topic inside it moved.
//
// The reference is frozen testdata, not the regenerable goldens: a
// golden rewritten by the code it guards would agree with any identity
// change.
func TestHomeAssistantIdentitiesAreThoseOf130(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "frozen_v1.3.0_identity.json"))
	if err != nil {
		t.Fatalf("read frozen identities: %v", err)
	}
	var frozen frozenIdentities
	if err := json.Unmarshal(raw, &frozen); err != nil {
		t.Fatalf("decode frozen identities: %v", err)
	}
	if len(frozen.Scenarios) != 2 {
		t.Fatalf("frozen file has %d scenarios, want full.en and nonascii.de", len(frozen.Scenarios))
	}

	byName := map[string]surfaceScenario{}
	for _, sc := range surfaceScenarios() {
		byName[sc.name] = sc
	}

	for _, want := range frozen.Scenarios {
		sc, ok := byName[want.Scenario]
		if !ok {
			t.Fatalf("no scenario %q", want.Scenario)
		}
		got := map[string]frozenIdentity{}
		for _, m := range buildSurface(t, sc).Messages {
			if !isConfigTopic(m.Topic) || m.JSON == nil {
				continue
			}
			id := identityOf(t, m.Topic, m.JSON)
			got[m.Topic] = id

			// Not vacuous: the payload around the identity did move.
			if st, _ := m.JSON["state_topic"].(string); st != "" && !strings.HasPrefix(st, "unifi/status/") {
				t.Errorf("%s: state_topic %q is not under unifi/status/", m.Topic, st)
			}
		}

		planes := map[string]int{}
		for _, w := range want.Configs {
			g, ok := got[w.Topic]
			if !ok {
				t.Errorf("%s: 1.3.0 published %s, 2.0 does not — an orphaned entity", want.Scenario, w.Topic)
				continue
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s: %s identity moved\n  1.3.0 %+v\n  now   %+v", want.Scenario, w.Topic, w, g)
			}
			planes[plane(w)]++
		}
		if len(got) != len(want.Configs) {
			t.Errorf("%s: %d configs rendered, 1.3.0 published %d", want.Scenario, len(got), len(want.Configs))
		}
		// Every plane the requirement names is represented.
		for _, p := range []string{"device", "client", "wlan", "site"} {
			if planes[p] == 0 {
				t.Errorf("%s: the frozen identities cover no %s entity", want.Scenario, p)
			}
		}
	}
}

func identityOf(t *testing.T, topic string, cfg map[string]any) frozenIdentity {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode %s: %v", topic, err)
	}
	var doc struct {
		UniqueID        string `json:"unique_id"`
		DefaultEntityID string `json:"default_entity_id"`
		Device          struct {
			Identifiers []string   `json:"identifiers"`
			Connections [][]string `json:"connections"`
			ViaDevice   *string    `json:"via_device"`
		} `json:"device"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode %s: %v", topic, err)
	}
	return frozenIdentity{
		Topic:           topic,
		UniqueID:        doc.UniqueID,
		DefaultEntityID: doc.DefaultEntityID,
		Identifiers:     doc.Device.Identifiers,
		Connections:     doc.Device.Connections,
		ViaDevice:       doc.Device.ViaDevice,
	}
}

// plane names which of the four entity families a frozen identity
// belongs to, by its unique_id namespace.
func plane(id frozenIdentity) string {
	switch {
	case strings.HasPrefix(id.UniqueID, "unifi_client_"):
		return "client"
	case strings.HasPrefix(id.UniqueID, "unifi_wlan_"):
		return "wlan"
	case strings.HasPrefix(id.UniqueID, "unifi_site_"):
		return "site"
	default:
		return "device"
	}
}
