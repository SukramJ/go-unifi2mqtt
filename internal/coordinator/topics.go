// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Topic construction.
//
// Every segment below is a stable English key, never a localised or
// operator-supplied string. Two things depend on that: the item path
// doubles as the entity key in Home Assistant discovery, and a renamed
// device must not orphan its topics. See CONCEPT.md §6.2.
//
// The tree follows mqtt-smarthome 2.0 (openccu-loom ADR 0083):
// `<name>/<function>/<item...>`, with the instance name MQTT_TOPIC,
// the function on the second level and the item path keyed by site and
// MAC:
//
//	unifi/connected                                   0/1/2, the Last Will
//	unifi/info                                        instance introspection
//	unifi/status/<site>/device/<mac>/state            a status item
//	unifi/status/<site>/device/<mac>/port/<idx>/poe
//	unifi/set/<site>/device/<mac>/cmd/restart         a command, same item path
//	unifi/status/<site>/wlan/<id>/enabled
//	unifi/set/<site>/wlan/<id>/enabled
//	unifi/status/bridge/error                         loop errors, not retained
//
// `bridge` is a literal first-level item beside `<site>`, which is why
// [CheckTopics] refuses a site spelled like it.

// Bridge-level item names.
const (
	bridgeSegment = "bridge"
	errorKey      = "error"
)

// reservedSites are the first-level items a site must not be spelled
// like: this bridge's own `bridge`, and the literal trees openccu-loom
// keeps beside its centrals, which ADR 0083 reserves for every project
// of the family. A function name is refused as well, through
// [hatopic.IsFunction].
var reservedSites = []string{bridgeSegment, "alarm", "security", "system"}

// Device value keys. Kept as constants because they double as the
// discovery entity keys and as translation-table lookups — a typo that
// only exists in one of the two places would produce an entity whose
// state never arrives.
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
	// keyLocate is the read-back of the locate LED, published only
	// where the locate switch is announced — the control's state topic
	// and nothing else reads it.
	keyLocate = "locate"
	// keyOnline is the object's reachability as a boolean status item
	// (spec §3.1: "the reachability of a single device behind it goes
	// into a status item"). Home Assistant reads it as the second
	// availability entry, beside `<name>/connected`.
	keyOnline = "online"

	keyPortState    = "state"
	keyPortSpeed    = "speed"
	keyPortPoE      = "poe"
	keyPortPoEPower = "poe/power_w"

	keyRadioChannel   = "channel"
	keyRadioTxRetries = "tx_retries"

	keyWLANEnabled = "enabled"
	keyWLANName    = "name"

	keyClientIP      = "ip"
	keyClientSignal  = "signal"
	keyClientBlocked = "blocked"
)

// topicBuilder assembles topics under one instance name and site.
//
// The grammar itself — which function sits where, how an item path is
// joined — is go-hamqtt's [hatopic.SmartHome]; this type only knows the
// item paths.
type topicBuilder struct {
	layout hatopic.SmartHome
	site   string
}

// newTopicBuilder builds the layout for an instance name and a site.
//
// The name is sanitised to one segment, as it always was here, and then
// validated by [hatopic.NewSmartHome]. The only name that survives the
// sanitising and is still refused is one starting with `$`; [CheckTopics]
// turns that, and a reserved site, into a start-up refusal, and the
// zero layout this falls back to renders no topic at all rather than a
// wrong one.
func newTopicBuilder(root, site string) topicBuilder {
	layout, _ := hatopic.NewSmartHome(sanitiseSegment(root))
	return topicBuilder{layout: layout, site: sanitiseSegment(site)}
}

// CheckTopics refuses an instance name and a site the topic tree cannot
// carry: a name mqtt-smarthome 2.0 §3 does not allow once sanitised, and
// a site whose topic-safe form is a reserved first-level item —
// `bridge`, `alarm`, `security`, `system` — or a function name
// (`status`, `set`, `connected`, `info`, `meta`, `maintenance`, `get`).
//
// The site check is what keeps `unifi/status/bridge/error` from sharing
// a level with a site's devices, and what makes the migration sweep's
// rule "a topic whose second level is a function name is new" sound: a
// pre-2.0 topic is `<name>/<site>/…`, and no site can spell a function.
//
// It is checked against the resolved site's reference, which is the
// string that goes into the topic — not against SITE, which may name the
// site by its display name or id.
func CheckTopics(name string, site model.Site) error {
	if _, err := hatopic.NewSmartHome(sanitiseSegment(name)); err != nil {
		return fmt.Errorf("coordinator: MQTT_TOPIC %q: %w", name, err)
	}
	seg := sanitiseSegment(site.Internal)
	if hatopic.IsFunction(seg) || slices.Contains(reservedSites, seg) {
		return fmt.Errorf("%w: SITE %q (reference %q) would publish under %q, "+
			"which is reserved beside the sites (bridge, alarm, security, system "+
			"and the mqtt-smarthome function names)",
			ErrReservedSite, site.Name, site.Internal, seg)
	}
	return nil
}

// ErrReservedSite is returned by [CheckTopics] for a site the topic tree
// cannot carry.
var ErrReservedSite = errors.New("coordinator: reserved site name")

// name is the instance name, the first level of every topic.
func (b topicBuilder) name() string { return b.layout.Name() }

// status returns `<name>/status/<item...>`; segments may carry `/`.
func (b topicBuilder) status(item ...string) string { return b.layout.Status(splitItem(item)...) }

// set returns `<name>/set/<item...>`, the command twin of [topicBuilder.status].
func (b topicBuilder) set(item ...string) string { return b.layout.Set(splitItem(item)...) }

// splitItem flattens keys that carry their own levels ("port/3/poe")
// into the one segment per level [hatopic.SmartHome] expects.
func splitItem(item []string) []string {
	out := make([]string, 0, len(item)+2)
	for _, s := range item {
		out = append(out, strings.Split(s, "/")...)
	}
	return out
}

// connected is `<name>/connected`, the Last Will and the 0/1/2 marker.
func (b topicBuilder) connected() string { return b.layout.Connected() }

// bridgeError is `<name>/status/bridge/error`, the non-retained loop
// error pulse.
func (b topicBuilder) bridgeError() string { return b.status(bridgeSegment, errorKey) }

// DeviceTopic returns a device status item, e.g.
// "unifi/status/default/device/aabbccddeeff/state".
//
// Exported so the discovery builder can point its configs at the exact
// topics this package publishes to, instead of rebuilding the layout
// from the same config values and drifting apart later.
func (c *Coordinator) DeviceTopic(mac model.MAC, key string) string {
	return c.topics.device(mac, key)
}

// DeviceCommandTopic returns a device command item, e.g.
// "unifi/set/default/device/aabbccddeeff/cmd/restart". Part of the
// hass.Topics contract.
func (c *Coordinator) DeviceCommandTopic(mac model.MAC, key string) string {
	return c.topics.deviceCommand(mac, key)
}

// device returns a device status item.
func (b topicBuilder) device(mac model.MAC, key string) string {
	return b.status(b.site, "device", mac.String(), key)
}

// deviceCommand returns a device command item.
func (b topicBuilder) deviceCommand(mac model.MAC, key string) string {
	return b.set(b.site, "device", mac.String(), key)
}

// devicePrefix returns everything up to and including the MAC, used to
// enumerate a device's topics when it disappears.
func (b topicBuilder) devicePrefix(mac model.MAC) string {
	return b.status(b.site, "device", mac.String()) + "/"
}

// port returns a port status item, e.g.
// "unifi/status/default/device/aabbccddeeff/port/3/poe".
func (b topicBuilder) port(mac model.MAC, idx int, key string) string {
	return b.device(mac, "port/"+strconv.Itoa(idx)+"/"+key)
}

// radio returns a radio status item keyed by band, e.g.
// "unifi/status/default/device/aabbccddeeff/radio/5g/channel".
//
// The band is used as the identifier because the API gives radios no
// index and no MAC — frequency is all there is to tell them apart.
func (b topicBuilder) radio(mac model.MAC, freqGHz float64, key string) string {
	return b.device(mac, "radio/"+model.BandSegment(freqGHz)+"/"+key)
}

// wlan returns a WLAN status item, e.g.
// "unifi/status/default/wlan/<id>/enabled".
//
// WLANs are keyed by the API's id rather than the SSID: an SSID can be
// renamed, and a renamed SSID must not orphan its entity.
func (b topicBuilder) wlan(id, key string) string {
	return b.status(b.site, "wlan", sanitiseSegment(id), key)
}

// wlanCommand returns a WLAN command item.
func (b topicBuilder) wlanCommand(id, key string) string {
	return b.set(b.site, "wlan", sanitiseSegment(id), key)
}

// health returns a site-health status item, e.g.
// "unifi/status/default/health/wan/state".
func (b topicBuilder) health(key string) string {
	return b.status(b.site, "health", key)
}

// HealthTopic returns a site-health topic. Part of the hass.Topics
// contract.
func (c *Coordinator) HealthTopic(key string) string {
	return c.topics.health(key)
}

// WLANTopic returns a WLAN topic. Part of the hass.Topics contract.
func (c *Coordinator) WLANTopic(id, key string) string {
	return c.topics.wlan(id, key)
}

// WLANCommandTopic returns a WLAN command item. Part of the hass.Topics
// contract.
func (c *Coordinator) WLANCommandTopic(id, key string) string {
	return c.topics.wlanCommand(id, key)
}

// client returns a client status item.
func (b topicBuilder) client(key, valueKey string) string {
	return b.status(b.site, "client", sanitiseSegment(key), valueKey)
}

// clientCommand returns a client command item.
func (b topicBuilder) clientCommand(key, valueKey string) string {
	return b.set(b.site, "client", sanitiseSegment(key), valueKey)
}

// ClientCommandTopic returns a client command item. Part of the
// hass.Topics contract.
func (c *Coordinator) ClientCommandTopic(key, valueKey string) string {
	return c.topics.clientCommand(key, valueKey)
}

// SmartHome is the instance's mqtt-smarthome layout. Part of the
// hass.Topics contract: the discovery runtime's layout forwards
// `connected`, `info` and `maintenance` to it.
func (c *Coordinator) SmartHome() hatopic.SmartHome { return c.topics.layout }

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
