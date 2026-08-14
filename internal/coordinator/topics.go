// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"strconv"
	"strings"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Topic construction.
//
// Every segment below is a stable English key, never a localised or
// operator-supplied string. Two things depend on that: the topic suffix
// doubles as the entity key in Home Assistant discovery (phase 3), and
// a renamed device must not orphan its topics. See CONCEPT.md §6.2.
//
// The tree is rooted at MQTT_TOPIC and keyed by site and MAC:
//
//	unifi/bridge/status
//	unifi/<site>/device/<mac>/state
//	unifi/<site>/device/<mac>/port/<idx>/poe
//	unifi/<site>/wlan/<id>/enabled

// Bridge-level topic suffixes.
const (
	bridgeSegment = "bridge"
	statusKey     = "status"
	infoKey       = "info"
	errorKey      = "error"
)

// Availability payloads. These are the strings Home Assistant expects
// by default for an availability topic, so discovery needs no
// payload_available override.
const (
	payloadOnline  = "online"
	payloadOffline = "offline"
)

// Binary payloads for switch- and binary-sensor-shaped values.
const (
	payloadOn  = "ON"
	payloadOff = "OFF"
)

// Device value keys. Kept as constants because phase 3 reuses them as
// the discovery entity keys and as translation-table lookups — a typo
// that only exists in one of the two places would produce an entity
// whose state never arrives.
const (
	keyState             = "state"
	keyUptime            = "uptime"
	keyCPUUtilization    = "cpu_utilization"
	keyMemoryUtilization = "memory_utilization"
	keyUplinkTxBps       = "uplink_tx_bps"
	keyUplinkRxBps       = "uplink_rx_bps"
	keyFirmware          = "firmware"
	keyUpdateAvailable   = "update_available"
	keyAttributes        = "attributes"

	keyPortState = "state"
	keyPortSpeed = "speed"
	keyPortPoE   = "poe"

	keyRadioChannel   = "channel"
	keyRadioTxRetries = "tx_retries"

	keyWLANEnabled = "enabled"
	keyWLANName    = "name"
)

// topicBuilder assembles topics under one root and site.
type topicBuilder struct {
	root string
	site string
}

func newTopicBuilder(root, site string) topicBuilder {
	return topicBuilder{root: sanitiseSegment(root), site: sanitiseSegment(site)}
}

// bridge returns a bridge-level topic, e.g. "unifi/bridge/status".
//
// It deliberately sits under our own root rather than the Home
// Assistant discovery prefix: that prefix belongs to HA, and a bridge
// availability topic has no business there (CONCEPT.md §5.1).
func (b topicBuilder) bridge(key string) string {
	return b.root + "/" + bridgeSegment + "/" + key
}

// device returns a device topic, e.g.
// "unifi/default/device/aabbccddeeff/state".
func (b topicBuilder) device(mac model.MAC, key string) string {
	return b.root + "/" + b.site + "/device/" + mac.String() + "/" + key
}

// devicePrefix returns everything up to and including the MAC, used to
// enumerate a device's topics when it disappears.
func (b topicBuilder) devicePrefix(mac model.MAC) string {
	return b.root + "/" + b.site + "/device/" + mac.String() + "/"
}

// port returns a port topic, e.g.
// "unifi/default/device/aabbccddeeff/port/3/poe".
func (b topicBuilder) port(mac model.MAC, idx int, key string) string {
	return b.device(mac, "port/"+strconv.Itoa(idx)+"/"+key)
}

// radio returns a radio topic keyed by band, e.g.
// "unifi/default/device/aabbccddeeff/radio/5g/channel".
//
// The band is used as the identifier because the API gives radios no
// index and no MAC — frequency is all there is to tell them apart.
func (b topicBuilder) radio(mac model.MAC, freqGHz float64, key string) string {
	return b.device(mac, "radio/"+bandSegment(freqGHz)+"/"+key)
}

// wlan returns a WLAN topic, e.g. "unifi/default/wlan/<id>/enabled".
//
// WLANs are keyed by the API's id rather than the SSID: an SSID can be
// renamed, and a renamed SSID must not orphan its entity.
func (b topicBuilder) wlan(id, key string) string {
	return b.root + "/" + b.site + "/wlan/" + sanitiseSegment(id) + "/" + key
}

// client returns a client topic. Phase 4 fills this in; the builder
// carries it now so the layout stays in one place.
func (b topicBuilder) client(key, valueKey string) string {
	return b.root + "/" + b.site + "/client/" + sanitiseSegment(key) + "/" + valueKey
}

// bandSegment turns a radio frequency into a topic segment: 2.4 -> "2g4",
// 5 -> "5g", 6 -> "6g". The 2.4 GHz band cannot use a bare decimal point
// because that reads badly in a topic and in an entity id derived from
// it.
func bandSegment(freqGHz float64) string {
	switch freqGHz {
	case 2.4:
		return "2g4"
	case 5:
		return "5g"
	case 6:
		return "6g"
	case 60:
		return "60g"
	default:
		// Unknown band: keep it addressable rather than dropping the
		// radio, replacing the separator so the topic stays one segment.
		return strings.ReplaceAll(strconv.FormatFloat(freqGHz, 'f', -1, 64), ".", "g")
	}
}

// sanitiseSegment makes an operator- or API-supplied string safe as a
// single topic segment.
//
// MQTT forbids the wildcards + and # in a published topic name, and a
// slash would silently create a level the rest of the code does not
// know about — a site literally named "a/b" would otherwise shift every
// device topic one level deeper. Empty input becomes "_" so a topic
// never contains an empty level.
func sanitiseSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '/', '+', '#':
			b.WriteRune('_')
		case ' ':
			b.WriteRune('_')
		default:
			if r < 0x20 || r == 0x7f {
				continue // control characters are not valid in a topic
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	return out
}

// boolPayload renders a boolean as the ON/OFF strings Home Assistant
// expects by default for switches and binary sensors.
func boolPayload(v bool) string {
	if v {
		return payloadOn
	}
	return payloadOff
}
