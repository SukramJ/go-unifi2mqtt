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
// default_entity_id are English and stable, only the display name
// follows LANGUAGE.
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
	// HealthTopic returns the state topic for one site-health value.
	HealthTopic(key string) string
	// WLANTopic returns the state topic for one WLAN value.
	WLANTopic(id, key string) string
	// AvailabilityTopic returns the bridge's retained online/offline
	// topic.
	AvailabilityTopic() string
}

// Manufacturer is the vendor string every device is announced under.
const Manufacturer = "Ubiquiti"

// OriginName is the `origin.name` every discovery payload carries.
//
// Home Assistant shows it as the integration that announced the device.
// It keys nothing: the entity registry keys on unique_id and the device
// registry on identifiers, so adding it re-registers nothing.
//
// It is not optional. A *device bundle* — the form ADR 0070 phase 9
// step 6 switches to — is refused outright by go-hamqtt's
// discovery.Validate with `origin.name is required`, and a refused
// bundle publishes no entities at all. That is why this landed before
// the migration rather than after it (measurement F7).
const OriginName = "go-unifi2mqtt"

// originInfo is the `origin` block.
//
// Name only, deliberately, following go-zendure2mqtt's phase-5
// precedent. Home Assistant also reads `sw_version` and `support_url`,
// and this bridge's own version is the obvious candidate for the first
// — but it is stamped at link time, so every retained config's bytes
// would then depend on how the binary was linked, the pinned surface
// would depend on it too, and every release would rewrite 315 retained
// payloads for no operator-visible gain. The build banner already says
// the version, once, at boot.
type originInfo struct {
	Name string `json:"name"`
}

// origin is the block every payload in this package carries.
func origin() originInfo { return originInfo{Name: OriginName} }

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
	// site scopes the synthetic site device's identifiers. It is
	// Site.Internal — the API's addressing token — and it is *not* a
	// display string.
	site string
	// siteName is the site device's display name, from Site.Name.
	siteName string
	lang     string
	topics   Topics
}

// Config configures a [Discovery].
type Config struct {
	// BaseTopic is Home Assistant's discovery prefix (HASS_BASE_TOPIC).
	BaseTopic string
	// Topics supplies the state-topic layout. Required.
	Topics Topics
	// Site scopes the site-health entities' identifiers. It is
	// Site.Internal (e.g. "default").
	Site string
	// SiteName is the site's display name (Site.Name, e.g. "Default").
	// It is the only string the site device is named after; when empty
	// it falls back to Site.
	SiteName string
	// Language selects the display language; anything unsupported falls
	// back to English.
	Language string
}

// New builds a Discovery.
func New(cfg Config) *Discovery {
	siteName := cfg.SiteName
	if siteName == "" {
		// A console that reports no display name still needs a label,
		// and the addressing token is the only string guaranteed to be
		// there. Before F14 the SSID switches used it unconditionally.
		siteName = cfg.Site
	}
	return &Discovery{
		baseTopic: cfg.BaseTopic,
		site:      cfg.Site,
		siteName:  siteName,
		lang:      normaliseLang(cfg.Language),
		topics:    cfg.Topics,
	}
}

// deviceInfo is the `device` block that groups entities in Home
// Assistant's registry.
type deviceInfo struct {
	Identifiers []string   `json:"identifiers"`
	Connections [][]string `json:"connections,omitempty"`
	Name        string     `json:"name"`
	// Manufacturer is omitted when empty rather than published as "".
	//
	// A network client has no manufacturer this bridge can know, and
	// the two spellings are not equally correct: Home Assistant reads
	// an absent key and an empty string identically, and absent is the
	// one that says "unknown" rather than "the empty string" (F15).
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	SWVersion    string `json:"sw_version,omitempty"`
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
	// DefaultEntityID seeds the entity_id as "<platform>.<seed>".
	//
	// Setting a seed at all is what keeps entity_ids
	// language-independent: without one Home Assistant derives them from
	// the localised name, so switching LANGUAGE creates a second set of
	// entities and leaves the first holding all the history
	// (CONCEPT.md §6.2).
	//
	// The older object_id key is deliberately not published: Home
	// Assistant's MQTT discovery schemas are extra=REMOVE_EXTRA, and
	// object_id is accepted by 0 of the 32 MQTT platforms as of 2026.9,
	// so it is silently dropped on arrival. default_entity_id replaced
	// it and is accepted by 28 of them.
	//
	// It never renames an existing entity: Home Assistant tracks
	// entities by unique_id, so a seed only shapes the id an entity gets
	// when it is first created.
	DefaultEntityID string `json:"default_entity_id,omitempty"`

	// StateTopic is omitted when empty because a button has none, and
	// "state_topic" is not a key the MQTT button schema declares. Home
	// Assistant's discovery schemas are extra=REMOVE_EXTRA, so an empty
	// one is dropped on arrival today — but a validator reading the
	// payload as a document refuses it, and once these entities are
	// published as one device bundle a refusal costs the device its
	// whole entity set rather than the one key.
	StateTopic          string `json:"state_topic,omitempty"`
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
	// Origin is on every payload, not just the ones that need it: the
	// step-6 bundle form requires it, and a key that is present on some
	// configs and absent on others is the shape that makes a migration
	// diff unreadable.
	Origin originInfo `json:"origin"`
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
	// valueTemplate maps the published value onto payloadOn/payloadOff
	// for a binary sensor whose state topic carries something richer
	// than two strings.
	//
	// Without one, a binary sensor can only match the values it names:
	// a device state topic carrying ten model.DeviceState strings
	// matches payload_on on exactly one of them and *nothing* on the
	// other nine, so the entity can turn on and never off. A template
	// that renders every input as one of the two payloads is the only
	// shape that has no third outcome.
	valueTemplate string
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
			deviceClass: "connectivity",
			// Anything that is not exactly ONLINE counts as unreachable,
			// which is what an automation wants: ADOPTING and UPDATING
			// are not states you can route traffic through.
			//
			// That has to be said with a template. The state topic
			// carries ten model.DeviceState strings and not one of them
			// is "OFF", so a bare payload_on: "ONLINE" matched the on
			// side and nothing at all on the other nine values: the
			// sensor turned on at the first ONLINE and could never
			// report the device going away. It stayed *available* while
			// doing it, because this entity is bridge-scoped on
			// purpose, so nothing anywhere said the reading was stale.
			valueTemplate: "{{ 'ON' if value == 'ONLINE' else 'OFF' }}",
			payloadOn:     payloadON,
			payloadOff:    payloadOFF,

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
			deviceClass: "connectivity",
			// model.PortState is UP, DOWN *or* UNKNOWN, so payload_on
			// "UP" / payload_off "DOWN" left the third value matching
			// neither and the entity unable to report it — the same
			// defect as the device "reachable" sensor, on a third
			// surface. A port whose link state the console cannot
			// report is not carrying traffic, so it reads as off.
			valueTemplate: "{{ 'ON' if value == 'UP' else 'OFF' }}",
			payloadOn:     payloadON,
			payloadOff:    payloadOFF,
			category:      "diagnostic",
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
	seed := entityIDSeed(info.Name, s.key)
	e := entity{
		Name:                label,
		UniqueID:            uid,
		DefaultEntityID:     string(s.platform) + "." + seed,
		StateTopic:          d.stateTopic(mac, s.stateSuffix),
		UnitOfMeasurement:   s.unit,
		DeviceClass:         s.deviceClass,
		StateClass:          s.stateClass,
		EntityCategory:      s.category,
		Icon:                s.icon,
		PayloadOn:           s.payloadOn,
		PayloadOff:          s.payloadOff,
		ValueTemplate:       s.valueTemplate,
		JSONAttributesTopic: d.stateTopic(mac, "attributes"),
		Availability:        d.availabilityFor(mac, s.bridgeAvailOnly),
		AvailabilityMode:    "all",
		Device:              info,
		Origin:              origin(),
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.configTopic(s.platform, deviceID(mac), s.key),
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
// siteDeviceInfo is the one `device` block the synthetic site device is
// announced under.
//
// It exists because there used to be two. The seven health entities
// named the site "UniFi Site <Site.Name>" and every SSID switch named
// it "UniFi Site <Site.Internal>" — the same identity, two spellings.
// Per entity that is invisible last-write-wins in Home Assistant's
// device registry; a device bundle carries exactly one device block, so
// at step 6 one of the two would have won by collection order, which
// nothing stated (measurement F14).
//
// Site.Name wins because Site.Internal is the API's addressing token
// and was never a display string. Identity is untouched either way:
// both paths already keyed on siteDeviceID(Site.Internal), and the
// health plane's default_entity_id seeds from this name but only ever
// shapes an entity_id at first creation.
func (d *Discovery) siteDeviceInfo() deviceInfo {
	return deviceInfo{
		Identifiers:  []string{siteDeviceID(d.site)},
		Name:         "UniFi Site " + d.siteName,
		Manufacturer: Manufacturer,
		Model:        "Site",
	}
}

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

// entityIDSeed builds the language-independent entity_id seed:
// "<device>_<key>", both slugified.
//
// It deliberately uses the device's name rather than its MAC: an
// automation referencing sensor.unifi_sw_har_cpu_utilization is
// readable, while sensor.unifi_00005e005302_cpu_utilization is not.
// Renaming a device in UniFi changes the seed, but not any entity that
// already exists — Home Assistant tracks those by unique_id, which is
// MAC-based and never moves.
func entityIDSeed(deviceName, key string) string {
	base := slugify(deviceName)
	if base == "" {
		return slugify(key)
	}
	return collapseTokens(base + "_" + slugify(key))
}

// umlautReplacer transliterates German umlauts the way Home Assistant's
// own slugify does.
//
// Note "u", not "ue": HA runs the text through unidecode, which strips
// the diaeresis rather than expanding it, so "Süd" becomes "sud". The
// German-typographic expansion would be defensible in isolation, but it
// would disagree with the id HA derives for the same name — and with
// the sibling bridges, which all match HA here.
var umlautReplacer = strings.NewReplacer("ä", "a", "ö", "o", "ü", "u", "ß", "ss")

// slugify reduces a string to the lowercase alphanumerics and
// underscores Home Assistant accepts in an entity_id.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := true // suppress a leading underscore
	for _, r := range umlautReplacer.Replace(strings.ToLower(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteRune('_')
			lastUnderscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// collapseTokens drops a token that merely repeats the one before it.
//
// Device names carry their role often enough that joining them to a key
// stutters: a switch named "Switch Garage" with the "switch_state" key
// would seed "switch_garage_switch_state".
func collapseTokens(s string) string {
	parts := strings.Split(s, "_")
	out := parts[:0]
	for _, p := range parts {
		if p == "" || (len(out) > 0 && out[len(out)-1] == p) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "_")
}

// deviceID is the Home Assistant device registry identifier. Keyed on
// the MAC because API UUIDs change on re-adopt and would orphan every
// entity along with its history (CONCEPT.md §3.4).
func deviceID(mac model.MAC) string { return idPrefix + "_" + mac.String() }

// LegacyConfigTopicForm records the retained per-entity config topic
// form this bridge publishes, as the ADR 0070 migration will have to
// state it. Nothing reads it today; step 6 is what reads it.
//
// It matters because retracting the per-entity configs is what makes
// room for a device bundle, and a retraction that renders a form this
// fleet is not on retracts nothing: the bundle lands while every
// per-entity config is still retained, Home Assistant refuses it with
// one "Received a conflicting MQTT discovery message" warning, and the
// result is no entities and no error anywhere on the wire.
//
// Three things have to hold, and only the first is the form:
//
//  1. Five segments, <prefix>/<platform>/<node_id>/<object_id>/config.
//     This is publisher.SupersededTopics' *default*
//     (LegacyTopicWithNodeID), which inverts three of the four earlier
//     phases: here the default is right and the escape hatch is wrong.
//     publisher.LegacyTopicByUniqueID would retract nothing at all and
//     must not be stated — Config.LegacyEntityTopics *replaces* the
//     default rather than extending it, so naming it would turn a
//     working retraction into none.
//
//  2. The node id is byte-equal to the payload's own
//     device.identifiers[0] — unifi_<mac>, unifi_client_<key> or
//     unifi_site_<site> — on every config this daemon writes. A bundle
//     keyed on anything else, a slug of the device name above all,
//     retracts nothing.
//
//  3. The component key is the **object-id segment**, which is not
//     derivable from the unique_id: on the client ip and signal sensors
//     and on every SSID switch the two differ. Deriving the component
//     key from a unique_id suffix silently retracts nothing for those.
//
// TestConfigTopicFormIsTheFiveSegmentNodeIDForm renders both candidate
// forms over the real builder output and requires that one reproduces
// the published topic and the other does not.
const LegacyConfigTopicForm = "<discovery_prefix>/<platform>/<node_id>/<object_id>/config"

// configTopic is where a discovery payload is published. It is the one
// composer for [LegacyConfigTopicForm]; every entity kind — device,
// port, radio, control, client and site health — goes through it, so
// there is exactly one place the form can move from.
func (d *Discovery) configTopic(p Platform, nodeID, objectID string) string {
	return strings.Join([]string{d.baseTopic, string(p), nodeID, objectID, "config"}, "/")
}

func (d *Discovery) stateTopic(mac model.MAC, suffix string) string {
	return d.topics.DeviceTopic(mac, suffix)
}
