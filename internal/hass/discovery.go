// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package hass builds Home Assistant MQTT auto-discovery payloads.
//
// It owns no I/O: [Discovery.Device] turns a domain device into a slice
// of [Entry] values that the coordinator publishes and, when the device
// disappears, clears. Keeping it I/O-free is what makes the payloads
// snapshot-testable — a discovery payload is an interface with a year
// of recorded history behind it, and an accidental change to a
// unique_id is not the kind of thing to notice in production.
//
// The naming rules it implements are in CONCEPT.md §6.2: unique_id and
// object_id are English and stable, only the display name follows
// LANGUAGE.
package hass

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Topics is the state-topic layout this package points discovery
// configs at.
//
// It is an interface rather than a second copy of the topic-building
// logic: a discovery config names a topic the coordinator publishes to,
// so two independent implementations drifting apart would produce
// entities that stay "unavailable" forever, with nothing visibly wrong
// in either package. The coordinator owns the layout and passes itself
// in.
type Topics interface {
	// DeviceTopic returns the state topic for one device value, e.g.
	// key "state" or "port/3/poe".
	DeviceTopic(mac model.MAC, key string) string
	// ClientTopic returns the state topic for one client value.
	ClientTopic(key, valueKey string) string
	// AvailabilityTopic returns the bridge's retained online/offline
	// topic.
	AvailabilityTopic() string
}

// Manufacturer is the vendor string every device is announced under.
const Manufacturer = "Ubiquiti"

// idPrefix namespaces every identifier this project creates, so a
// unique_id cannot collide with another integration's.
const idPrefix = "unifi"

// Platform is the Home Assistant platform, used as the second segment
// of the discovery topic.
type Platform string

// Platforms used by this project.
const (
	PlatformSensor        Platform = "sensor"
	PlatformBinarySensor  Platform = "binary_sensor"
	PlatformButton        Platform = "button"
	PlatformSwitch        Platform = "switch"
	PlatformDeviceTracker Platform = "device_tracker"
)

// Entry is one discovery payload ready for publication.
type Entry struct {
	// ConfigTopic is the retained topic the payload goes to. Publishing
	// an empty payload here removes the entity from Home Assistant.
	ConfigTopic string
	// Payload is the JSON discovery config.
	Payload []byte
}

// Discovery builds discovery payloads for one site.
type Discovery struct {
	// baseTopic is Home Assistant's discovery prefix, e.g.
	// "homeassistant".
	baseTopic string
	lang      string
	topics    Topics
}

// Config configures a [Discovery].
type Config struct {
	// BaseTopic is Home Assistant's discovery prefix (HASS_BASE_TOPIC).
	BaseTopic string
	// Topics supplies the state-topic layout. Required.
	Topics Topics
	// Language selects the display language; anything unsupported falls
	// back to English.
	Language string
}

// New builds a Discovery.
func New(cfg Config) *Discovery {
	return &Discovery{
		baseTopic: cfg.BaseTopic,
		lang:      normaliseLang(cfg.Language),
		topics:    cfg.Topics,
	}
}

// deviceInfo is the `device` block that groups entities in Home
// Assistant's registry.
type deviceInfo struct {
	Identifiers  []string   `json:"identifiers"`
	Connections  [][]string `json:"connections,omitempty"`
	Name         string     `json:"name"`
	Manufacturer string     `json:"manufacturer"`
	Model        string     `json:"model,omitempty"`
	SWVersion    string     `json:"sw_version,omitempty"`
	// ViaDevice reproduces the network hierarchy (client → AP → switch
	// → gateway) in Home Assistant's device page. It is why the model
	// carries UplinkMAC at all.
	ViaDevice string `json:"via_device,omitempty"`
}

// availabilityEntry is one source in a multi-source availability list.
type availabilityEntry struct {
	Topic string `json:"topic"`
	// ValueTemplate maps a topic's payload onto online/offline when it
	// does not already use those words.
	ValueTemplate string `json:"value_template,omitempty"`
}

// entity is the discovery payload shared by every platform. Fields are
// omitted when empty so a payload carries only what applies.
type entity struct {
	Name     string `json:"name"`
	UniqueID string `json:"unique_id"`
	// ObjectID seeds the entity_id. Setting it explicitly is what keeps
	// entity_ids language-independent: without it Home Assistant would
	// derive them from the localised name, and switching LANGUAGE would
	// create a second set of entities (CONCEPT.md §6.2).
	ObjectID string `json:"object_id"`

	StateTopic          string `json:"state_topic"`
	UnitOfMeasurement   string `json:"unit_of_measurement,omitempty"`
	DeviceClass         string `json:"device_class,omitempty"`
	StateClass          string `json:"state_class,omitempty"`
	EntityCategory      string `json:"entity_category,omitempty"`
	Icon                string `json:"icon,omitempty"`
	PayloadOn           string `json:"payload_on,omitempty"`
	PayloadOff          string `json:"payload_off,omitempty"`
	ValueTemplate       string `json:"value_template,omitempty"`
	JSONAttributesTopic string `json:"json_attributes_topic,omitempty"`

	Availability     []availabilityEntry `json:"availability,omitempty"`
	AvailabilityMode string              `json:"availability_mode,omitempty"`

	Device deviceInfo `json:"device"`
}

// spec describes one entity before it is rendered, so the per-platform
// tables below stay readable.
type spec struct {
	platform Platform
	// key is the stable English identifier: topic suffix, unique_id
	// component and translation lookup in one.
	key string
	// nameKey selects the translation; empty means use key.
	nameKey string
	// nameArg parameterises names like "Port 3 link".
	nameArg string
	// stateSuffix is appended to the device's state topic root.
	stateSuffix string

	unit        string
	deviceClass string
	stateClass  string
	category    string
	icon        string
	payloadOn   string
	payloadOff  string
	// deviceScoped marks entities whose availability must not depend on
	// the device being online — the state sensor itself, above all.
	bridgeAvailOnly bool
}

// Device returns the discovery entries for one device: the device-level
// sensors plus one set per port and radio.
func (d *Discovery) Device(dev *model.Device) ([]Entry, error) {
	if dev.MAC.IsZero() {
		return nil, nil
	}

	info := d.deviceInfo(dev)
	specs := deviceSpecs()
	for i := range dev.Ports {
		specs = append(specs, portSpecs(&dev.Ports[i])...)
	}
	for i := range dev.Radios {
		specs = append(specs, radioSpecs(&dev.Radios[i])...)
	}

	entries := make([]Entry, 0, len(specs))
	for i := range specs {
		e, err := d.render(&specs[i], dev.MAC, info)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// deviceSpecs lists the entities every device gets.
//
// Only read-only entities live here: buttons and switches need command
// topics, which arrive with the actuators in phase 6.
func deviceSpecs() []spec {
	return []spec{
		{
			platform: PlatformSensor, key: "state", stateSuffix: "state",
			// The state sensor must stay available when the device goes
			// offline — it is the entity that reports being offline.
			bridgeAvailOnly: true,
			icon:            "mdi:lan-connect",
		},
		{
			platform: PlatformBinarySensor, key: "reachable", stateSuffix: "state",
			deviceClass: "connectivity", payloadOn: "ONLINE",
			// Anything that is not exactly ONLINE counts as unreachable,
			// which is what an automation wants: ADOPTING and UPDATING
			// are not states you can route traffic through.
			payloadOff:      "",
			bridgeAvailOnly: true,
		},
		{
			platform: PlatformSensor, key: "uptime", stateSuffix: "uptime",
			unit: "s", deviceClass: "duration", stateClass: "measurement",
			category: "diagnostic",
		},
		{
			platform: PlatformSensor, key: "cpu_utilization", stateSuffix: "cpu_utilization",
			unit: "%", stateClass: "measurement", category: "diagnostic",
			icon: "mdi:cpu-64-bit",
		},
		{
			platform: PlatformSensor, key: "memory_utilization", stateSuffix: "memory_utilization",
			unit: "%", stateClass: "measurement", category: "diagnostic",
			icon: "mdi:memory",
		},
		{
			platform: PlatformSensor, key: "uplink_tx_bps", stateSuffix: "uplink_tx_bps",
			unit: "bit/s", deviceClass: "data_rate", stateClass: "measurement",
			category: "diagnostic",
		},
		{
			platform: PlatformSensor, key: "uplink_rx_bps", stateSuffix: "uplink_rx_bps",
			unit: "bit/s", deviceClass: "data_rate", stateClass: "measurement",
			category: "diagnostic",
		},
		{
			platform: PlatformSensor, key: "firmware", stateSuffix: "firmware",
			category: "diagnostic", icon: "mdi:chip",
			bridgeAvailOnly: true,
		},
		{
			platform: PlatformBinarySensor, key: "update_available", stateSuffix: "update_available",
			deviceClass: "update", payloadOn: "ON", payloadOff: "OFF",
			category:        "diagnostic",
			bridgeAvailOnly: true,
		},
	}
}

func portSpecs(p *model.Port) []spec {
	idx := strconv.Itoa(p.Idx)
	prefix := "port_" + idx + "_"
	out := []spec{
		{
			platform: PlatformBinarySensor, key: prefix + "link",
			nameKey: "port_link", nameArg: idx,
			stateSuffix: "port/" + idx + "/state",
			deviceClass: "connectivity", payloadOn: "UP", payloadOff: "DOWN",
			category: "diagnostic",
		},
		{
			platform: PlatformSensor, key: prefix + "speed",
			nameKey: "port_speed", nameArg: idx,
			stateSuffix: "port/" + idx + "/speed",
			unit:        "Mbit/s", deviceClass: "data_rate", stateClass: "measurement",
			category: "diagnostic",
		},
	}
	// A port without PoE hardware gets no PoE entity: the coordinator
	// publishes no such topic, and an entity pointing at a topic that
	// never receives a value shows as unavailable forever.
	if p.PoE != nil {
		out = append(out, spec{
			platform: PlatformBinarySensor, key: prefix + "poe",
			nameKey: "port_poe", nameArg: idx,
			stateSuffix: "port/" + idx + "/poe",
			deviceClass: "power", payloadOn: "ON", payloadOff: "OFF",
			category: "diagnostic",
		})
	}
	return out
}

func radioSpecs(r *model.Radio) []spec {
	band := model.BandSegment(r.FrequencyGHz)
	label := model.BandLabel(r.FrequencyGHz)
	//nolint:prealloc // fixed-size literal, not an accumulating append
	return []spec{
		{
			platform: PlatformSensor, key: "radio_" + band + "_channel",
			nameKey: "radio_channel", nameArg: label,
			stateSuffix: "radio/" + band + "/channel",
			category:    "diagnostic", icon: "mdi:wifi",
		},
		{
			platform: PlatformSensor, key: "radio_" + band + "_tx_retries",
			nameKey: "radio_tx_retries", nameArg: label,
			stateSuffix: "radio/" + band + "/tx_retries",
			unit:        "%", stateClass: "measurement",
			category: "diagnostic",
		},
	}
}

func (d *Discovery) render(s *spec, mac model.MAC, info deviceInfo) (Entry, error) {
	nameKey := s.nameKey
	if nameKey == "" {
		nameKey = s.key
	}
	label := name(nameKey, d.lang)
	if s.nameArg != "" {
		label = nameWith(nameKey, d.lang, s.nameArg)
	}

	uid := idPrefix + "_" + mac.String() + "_" + s.key
	e := entity{
		Name:     label,
		UniqueID: uid,
		// object_id is the unique_id: both are English and stable, and
		// using the same string makes the relationship obvious when
		// debugging a mis-created entity.
		ObjectID:            uid,
		StateTopic:          d.stateTopic(mac, s.stateSuffix),
		UnitOfMeasurement:   s.unit,
		DeviceClass:         s.deviceClass,
		StateClass:          s.stateClass,
		EntityCategory:      s.category,
		Icon:                s.icon,
		PayloadOn:           s.payloadOn,
		PayloadOff:          s.payloadOff,
		JSONAttributesTopic: d.stateTopic(mac, "attributes"),
		Availability:        d.availabilityFor(mac, s.bridgeAvailOnly),
		AvailabilityMode:    "all",
		Device:              info,
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.configTopic(s.platform, mac, s.key),
		Payload:     payload,
	}, nil
}

// availabilityFor builds the availability sources for an entity.
//
// Two stages (CONCEPT.md §6.5): the bridge topic covers the daemon
// being gone, and the device's own state topic covers the device being
// gone — without the second one a switch that went offline would sit in
// Home Assistant showing its last CPU reading as though it were current.
//
// Entities that report the offline condition itself (state, reachable,
// firmware, update) opt out of the second stage: making them unavailable
// when the device is offline would hide exactly the information the
// user needs.
func (d *Discovery) availabilityFor(mac model.MAC, bridgeOnly bool) []availabilityEntry {
	out := make([]availabilityEntry, 0, 2)
	out = append(out, availabilityEntry{Topic: d.topics.AvailabilityTopic()})
	if bridgeOnly {
		return out
	}
	return append(out, availabilityEntry{
		Topic:         d.stateTopic(mac, "state"),
		ValueTemplate: "{{ 'online' if value == 'ONLINE' else 'offline' }}",
	})
}

func (d *Discovery) deviceInfo(dev *model.Device) deviceInfo {
	info := deviceInfo{
		Identifiers:  []string{deviceID(dev.MAC)},
		Connections:  [][]string{{"mac", dev.MAC.Colon()}},
		Name:         dev.Name,
		Manufacturer: Manufacturer,
		Model:        dev.Model,
		SWVersion:    dev.Firmware,
	}
	if !dev.UplinkMAC.IsZero() {
		info.ViaDevice = deviceID(dev.UplinkMAC)
	}
	if info.Name == "" {
		// A device the operator never named still needs a label; the MAC
		// is the only thing guaranteed to be there.
		info.Name = "UniFi " + dev.MAC.Colon()
	}
	return info
}

// deviceID is the Home Assistant device registry identifier. Keyed on
// the MAC because API UUIDs change on re-adopt and would orphan every
// entity along with its history (CONCEPT.md §3.4).
func deviceID(mac model.MAC) string { return idPrefix + "_" + mac.String() }

// configTopic is where a discovery payload is published:
// <prefix>/<platform>/unifi_<mac>/<key>/config
func (d *Discovery) configTopic(p Platform, mac model.MAC, key string) string {
	return strings.Join([]string{
		d.baseTopic, string(p), deviceID(mac), key, "config",
	}, "/")
}

func (d *Discovery) stateTopic(mac model.MAC, suffix string) string {
	return d.topics.DeviceTopic(mac, suffix)
}
