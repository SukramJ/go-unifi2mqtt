// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	hacatalog "github.com/SukramJ/go-ha-catalog"
	"github.com/SukramJ/go-hamqtt/discovery"
	hamodel "github.com/SukramJ/go-hamqtt/model"
	hatopic "github.com/SukramJ/go-hamqtt/topic"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// The go-hamqtt rendering path — ADR 0070 phase 9, step 4.
//
// This file is a second, parallel way to produce the same discovery
// payloads [Discovery.Device], [Discovery.Client], [Discovery.Health],
// [Discovery.DeviceControls], [Discovery.ClientControls] and
// [Discovery.WLANControl] already produce: the fleet is described as
// go-hamqtt [hamodel.Device] values carrying [hamodel.Entity] values,
// and the JSON comes out of go-hamqtt's renderer rather than out of
// this package's struct literals.
//
// **Nothing here is wired.** No publish path, no coordinator call site
// and no MQTT bootstrap reaches it; the only callers are tests, and
// [Discovery] has no transport of its own to reach a broker with. It
// exists to answer one question at zero risk — can the shared model
// reproduce this bridge's published bytes? — before step 6 asks it
// irreversibly, when getting the answer wrong costs a device its whole
// entity set with one WARNING line and nothing on the wire.
//
// Four library settings would each be a silent mass diff, so each is
// stated once, here, rather than left to a zero value:
//
//   - [discovery.StatusObjectEncoding]. The zero [discovery.Encoding]
//     is EnvelopeEncoding, which attaches
//     `value_template: {{ value_json.value }}` to every entity that
//     reads a topic. This bridge publishes mqtt-smarthome status
//     objects, so the envelope template would break every
//     state-reading entity at once — and it would look like a
//     formatting detail in review.
//   - [HamqttOrigin]. [discovery.RenderComponent] stamps an `origin`
//     block whenever the origin has a name. Until measurement F7 was
//     fixed this bridge published none and the render passed a zero
//     origin to match; it now publishes one on all 315 configs, because
//     [discovery.Validate] refuses a *bundle* without `origin.name` and
//     a refused bundle publishes no entities at all.
//   - [hamqttContext.NodeID], [hamqttContext.UniqueID] and
//     [hamqttContext.ObjectID] — see each method.
//   - [hamqttContext.Availability], which is the whole of F8: the
//     shape the library defaults to is right, the device slot it
//     derives is not.
//
// What this file does NOT do: it does not classify, enrich or decide
// anything. Every unit, device class, icon, payload and topic comes
// from the same spec tables and the same [Topics] implementation the
// shipped builders use. The experiment is about rendering, and a
// re-derived catalogue would be measuring the transcription instead.

// hamqttNamespace is the unique-id namespace go-hamqtt would prefix
// with if [hamqttContext.UniqueID] were not overridden. It is stated so
// [discovery.StdContext] is fully configured and the override is the
// only reason the ids come out as they do.
const hamqttNamespace = idPrefix

// HamqttOrigin is the origin block this bridge publishes, in the form
// the library renders it. It is [OriginName] and nothing else — see
// [originInfo] for why no sw_version or support_url.
func HamqttOrigin() discovery.Origin { return discovery.Origin{Name: OriginName} }

// hamqttEntity is one entity in the shared model.
//
// It carries identity *seeds* rather than finished strings, because the
// three identity strings are composed by three different rules that the
// library resolves at different moments: the unique id at render time,
// the entity-id seed at render time through [hamqttContext.ObjectID],
// and the object-id topic segment at retraction time from
// [hamodel.Entity.Key]. A struct holding finished strings would let the
// three drift apart, which is the exact failure F5 describes.
type hamqttEntity struct {
	hamodel.Basic

	// uidBase and uidKey compose the unique id as
	// "<uidBase>_<uidKey>". They are separate because on 21 of the 315
	// published configs the unique id is NOT "<node_id>_<object_id>":
	// the client ip and signal sensors key on "client_ip"/"client_signal"
	// while their topic segment is "ip"/"signal", and an SSID switch
	// lives on the site node while its id is namespaced under
	// "unifi_wlan_<uuid>". See [LegacyConfigTopicForm].
	uidBase string
	uidKey  string

	// seedName and seedKey compose the entity-id seed exactly as
	// [entityIDSeed] does — collapseTokens(slugify(name)+"_"+slugify(key)).
	// The seed is a *composition*, not a normaliser applied to one
	// string, which is why go-hamqtt's topic.Slug cannot stand in for it
	// however it is configured (measurement F4, decided at step 2:
	// this bridge keeps its own normalisers).
	seedName string
	seedKey  string

	// fields carries the platform keys [discovery.Component] has no
	// typed home for: payload_on/off, state_on/off, payload_press,
	// source_type and the two tracker payloads.
	fields any
}

// BuildDiscovery implements [discovery.Builder], attaching [fields].
func (e *hamqttEntity) BuildDiscovery(_ discovery.Context, comp *discovery.Component) error {
	if e.fields != nil {
		comp.Fields = e.fields
	}
	return nil
}

// --- the topic layout ------------------------------------------------

// hamqttLayout is this bridge's topic tree as a [hatopic.SmartHomeLayout].
//
// It delegates to the same [Topics] implementation the shipped builders
// point their configs at, and composes no string of its own beyond
// joining a slot's path. That is deliberate: F10 counted 22 places in
// this tree that compose an MQTT topic, and a Layout that re-formatted
// "<name>/status/<site>/device/<mac>/<key>" would be one more —
// agreeing today and free to drift tomorrow, with the symptom being
// entities that stay "unknown" forever and nothing in any log.
//
// It is a [hatopic.SmartHomeLayout] because that capability is what
// switches go-hamqtt's runtime onto `<name>/connected`: the Last Will
// writes 0, [publisher.Runtime.AnnounceOnline] the current level, and
// the runtime refuses a layout whose Connected and Bridge disagree. The
// three extra methods forward to [Topics.SmartHome], so they are
// go-hamqtt's spelling of the grammar and not this package's.
//
// go-hamqtt's own topic.SmartHome is not used for the item paths and it
// is worth saying why rather than leaving it to look like an oversight.
// Its State renders "<name>/status/<scope...>/<uid>/<channel>/<bucket>/<path...>",
// which keys devices on the device UID ("unifi_<mac>") where this tree
// uses the bare MAC and has no site level; and its Availability is
// "<scope...>/<uid>/online" for the same reason.
type hamqttLayout struct{ topics Topics }

// Slot scopes. The first scope segment names which of the four topic
// families a slot belongs to; [Topics] owns everything below it.
const (
	scopeDevice = "device"
	scopeClient = "client"
	scopeHealth = "health"
	scopeWLAN   = "wlan"
)

// State implements [hatopic.Layout]: the slot's status item.
func (l hamqttLayout) State(s hamodel.Slot) string {
	return l.render(s, s.Path, false)
}

// Command implements [hatopic.Layout]: the slot's set item, the same
// item path under `set`.
func (l hamqttLayout) Command(s hamodel.Slot) string {
	return l.render(s, s.Path, true)
}

// Availability implements [hatopic.Layout]: the object's `online` status
// item for an infrastructure device or a client, and nothing for the
// site plane, which has no reachability of its own beyond the bridge's.
func (l hamqttLayout) Availability(s hamodel.Slot) string {
	if len(s.Scope) == 0 || (s.Scope[0] != scopeDevice && s.Scope[0] != scopeClient) {
		return ""
	}
	return l.render(s, []string{"online"}, false)
}

// Bridge implements [hatopic.Layout]: `<name>/connected`.
func (l hamqttLayout) Bridge() string { return l.topics.AvailabilityTopic() }

// Connected implements [hatopic.SmartHomeLayout].
func (l hamqttLayout) Connected() string { return l.topics.SmartHome().Connected() }

// Info implements [hatopic.SmartHomeLayout].
func (l hamqttLayout) Info() string { return l.topics.SmartHome().Info() }

// Maintenance implements [hatopic.SmartHomeLayout].
func (l hamqttLayout) Maintenance(item ...string) string {
	return l.topics.SmartHome().Maintenance(item...)
}

// render resolves a slot against [Topics]. The scope names the family;
// the address is the segment that family is keyed on.
func (l hamqttLayout) render(s hamodel.Slot, path []string, command bool) string {
	key := strings.Join(path, "/")
	family := ""
	if len(s.Scope) > 0 {
		family = s.Scope[0]
	}
	switch family {
	case scopeDevice:
		mac, err := model.ParseMAC(s.Address)
		if err != nil {
			return ""
		}
		if command {
			return l.topics.DeviceCommandTopic(mac, key)
		}
		return l.topics.DeviceTopic(mac, key)
	case scopeClient:
		if command {
			return l.topics.ClientCommandTopic(s.Address, key)
		}
		return l.topics.ClientTopic(s.Address, key)
	case scopeHealth:
		if command {
			return ""
		}
		return l.topics.HealthTopic(key)
	case scopeWLAN:
		if command {
			return l.topics.WLANCommandTopic(s.Address, key)
		}
		return l.topics.WLANTopic(s.Address, key)
	default:
		return ""
	}
}

var _ hatopic.SmartHomeLayout = hamqttLayout{}

// --- the render context ----------------------------------------------

// hamqttContext is this bridge's [discovery.Context].
//
// Four of the eleven methods are overridden; the rest come from
// [discovery.StdContext] unchanged.
type hamqttContext struct {
	discovery.StdContext
	layout hamqttLayout
}

var _ discovery.Context = hamqttContext{}

// NodeID implements [discovery.Context]: the device identifier
// verbatim.
//
// It matters for step 6 and for nothing else today.
// publisher.SupersededTopics keys the retraction of the per-entity
// configs on discovery.Bundle.NodeID, and this bridge publishes
// <prefix>/<platform>/<device.identifiers[0]>/<object_id>/config on
// 315 of 315 configs — so a node id that is anything other than the
// device identifier retracts nothing at all.
//
// go-hamqtt's own discovery.NodeID is topic.Slug(dev.UID()), which
// reproduces the identifier for every device in this fleet, because
// every identifier this bridge composes is already lower-case ASCII
// with "_" and "-". The override is therefore a guard rather than a
// correction, and TestHamqttNodeIDIsTheDeviceIdentifier says so in both
// directions: it asserts the default agrees on all nine devices *and*
// that the two differ on an identifier that is not already slug-shaped,
// so the premise cannot rot silently.
func (c hamqttContext) NodeID(dev *hamodel.Device) string {
	if dev == nil {
		return ""
	}
	return dev.UID()
}

// UniqueID implements [discovery.Context]: "<uidBase>_<uidKey>".
//
// Home Assistant keys the entity registry on (domain, platform,
// unique_id) and has no migration path, so this is the one string that
// may never move. The library's own UniqueID would compose
// "<namespace>_<slug(uid)>_<slug(key)>", which is right for the 294
// configs where the unique id is "<node_id>_<object_id>" and wrong for
// the 21 where it is not.
func (c hamqttContext) UniqueID(_ *hamodel.Device, e hamodel.Entity) string {
	ent, ok := e.(*hamqttEntity)
	if !ok {
		return ""
	}
	return ent.uidBase + "_" + ent.uidKey
}

// ObjectID implements [discovery.Context]: the entity-id seed, through
// this package's own [entityIDSeed].
//
// This is measurement F4, decided in writing at step 2: the bridge
// keeps slugify and collapseTokens. Two properties make the library's
// topic.Slug unusable however it is wired in. It transliterates "ä" as
// "ae" where Home Assistant's own slugify — and therefore this package
// — strips the diaeresis, and it preserves a hyphen this package folds;
// 9 of 13 measured device names diverge. And the seed is a
// *composition*: collapseTokens runs over the joined string and drops a
// token repeating the one before it, so no per-string normaliser
// reproduces it, for ASCII names either.
//
// It is called with the display-name seed and the key seed the shipped
// builder uses, which for an SSID switch is the literal "unifi_wlan"
// and the SSID — the one entity whose seed is not derived from its
// device's name.
func (c hamqttContext) ObjectID(_ *hamodel.Device, e hamodel.Entity) string {
	ent, ok := e.(*hamqttEntity)
	if !ok {
		return ""
	}
	return entityIDSeed(ent.seedName, ent.seedKey)
}

// Availability implements [discovery.Context], and is the whole of
// measurement F8.
//
// The library's shape is already right — a list, mode "all", resolved
// per entity from [hamodel.Description.Availability], and under a
// [hatopic.SmartHomeLayout] the two entries it spells are exactly this
// bridge's: [discovery.ConnectedAvailability] on `<name>/connected` and
// [discovery.OnlineAvailability] on the object's `online` item. What
// differs is the slot the device level is resolved from:
// discovery.StdContext derives it from the device's UID
// ("unifi_<mac>"), and this tree keys the item on the bare MAC or the
// client key, which only the entity's own first binding carries. So the
// device-level topic is resolved from that binding rather than from the
// device, and it cannot name a topic no config of this entity points at.
func (c hamqttContext) Availability(_ *hamodel.Device, e hamodel.Entity) []discovery.AvailabilityEntry {
	levels, _ := e.Desc().Availability.Resolved()
	out := make([]discovery.AvailabilityEntry, 0, len(levels))
	for _, level := range levels {
		switch level {
		case hamodel.LevelBridge:
			out = append(out, discovery.ConnectedAvailability(c.layout.Bridge(), discovery.ConnectedOperational))
		case hamodel.LevelDevice:
			binds := e.Bindings()
			if len(binds) == 0 {
				continue
			}
			topic := c.layout.Availability(binds[0].Slot)
			if topic == "" {
				continue
			}
			out = append(out, discovery.OnlineAvailability(topic, c.Encoding()))
		case hamodel.LevelParent, hamodel.LevelSelf, hamodel.LevelNone:
			// Not used by this bridge: there is no per-parent
			// availability signal, no RoleAvailability binding, and
			// every entity carries a list.
			continue
		}
	}
	return out
}

// newHamqttContext builds the context for this Discovery's language and
// topic layout.
func (d *Discovery) newHamqttContext() hamqttContext {
	layout := hamqttLayout{topics: d.topics}
	return hamqttContext{
		Layout:    layout,
		Namespace: hamqttNamespace,
		Lang:      d.lang,
		// The status object, not the zero value. See the file header.
		Enc:    discovery.StatusObjectEncoding,
		layout: layout,
	}
}

// --- the fleet -------------------------------------------------------

// HamqttFleet is everything the shipped builders are called with over
// one announce pass, in one value.
//
// It is a struct rather than six arguments so a test can re-derive the
// inputs once and drive both rendering paths from the same value —
// which is what makes the byte comparison a statement about the
// renderer rather than about two different entity sets.
type HamqttFleet struct {
	// Devices are the infrastructure devices, with ports, radios and
	// the resolved uplink MAC — the snapshot the static poll announces
	// from.
	Devices []model.Device
	// Clients are the clients that passed the filter.
	Clients []model.Client
	// WLANs is the SSID catalogue.
	WLANs []model.WLAN
	// ClientOpts and ControlOpts gate the optional entities exactly as
	// the coordinator gates them.
	ClientOpts  ClientOptions
	ControlOpts ControlOptions
	// AnnounceHealth reports whether the site-health plane is announced,
	// which needs the classic layer to have answered.
	AnnounceHealth bool
}

// HamqttComponent is one rendered discovery config: where it goes, what
// platform Home Assistant reads it as, and the bytes.
type HamqttComponent struct {
	// Topic is the retained per-entity config topic, composed by the
	// same [Discovery.configTopic] the shipped path uses.
	Topic string
	// Platform is the component's platform, carried so a validator does
	// not have to recover it from the topic.
	Platform hacatalog.Platform
	// NodeID is the device identifier this component's bundle would be
	// published under.
	NodeID string
	// Key is the component key inside a bundle — and, by
	// publisher.SupersededTopics, the object-id segment of [Topic].
	Key string
	// Body is the discovery payload.
	Body []byte
}

// RenderHamqtt renders the whole fleet through go-hamqtt and returns
// the per-entity configs. It publishes nothing: [Discovery] owns no
// transport, and the result is handed back to the caller.
func (d *Discovery) RenderHamqtt(f HamqttFleet) ([]HamqttComponent, error) {
	ctx := d.newHamqttContext()
	groups := d.hamqttGroups(f)

	out := make([]HamqttComponent, 0, 128)
	seen := make(map[string]bool, 128)
	for _, g := range groups {
		for _, e := range g.entities {
			comp, err := discovery.RenderComponent(ctx, g.dev, e, HamqttOrigin())
			if err != nil {
				return nil, fmt.Errorf("hass: render %q: %w", e.Key(), err)
			}
			body, err := comp.EntityJSON()
			if err != nil {
				return nil, fmt.Errorf("hass: encode %q: %w", e.Key(), err)
			}
			topic := d.configTopic(Platform(comp.Platform), ctx.NodeID(g.dev), e.Key())
			if seen[topic] {
				return nil, fmt.Errorf("hass: duplicate config topic %q", topic)
			}
			seen[topic] = true
			out = append(out, HamqttComponent{
				Topic:    topic,
				Platform: comp.Platform,
				NodeID:   ctx.NodeID(g.dev),
				Key:      e.Key(),
				Body:     body,
			})
		}
	}
	return out, nil
}

// HamqttBundles renders the fleet as device bundles — the form step 6
// would publish — so [discovery.Validate] has something to judge.
// Nothing publishes the result.
//
// Components of one device identity that arrive with two different
// device blocks are merged onto the first one seen, and the conflict is
// reported rather than hidden: see [HamqttDeviceBlockConflicts].
func (d *Discovery) HamqttBundles(f HamqttFleet, origin discovery.Origin) ([]*discovery.Bundle, error) {
	ctx := d.newHamqttContext()
	groups := d.hamqttGroups(f)

	order := make([]string, 0, len(groups))
	merged := map[string]*hamqttGroup{}
	for i := range groups {
		g := groups[i]
		uid := g.dev.UID()
		if prev, ok := merged[uid]; ok {
			prev.entities = append(prev.entities, g.entities...)
			continue
		}
		clone := &hamqttGroup{dev: g.dev, entities: append([]hamodel.Entity(nil), g.entities...)}
		merged[uid] = clone
		order = append(order, uid)
	}

	out := make([]*discovery.Bundle, 0, len(order))
	for _, uid := range order {
		g := merged[uid]
		b, err := discovery.Render(ctx, g.dev, g.entities, origin)
		if err != nil {
			return nil, fmt.Errorf("hass: bundle %q: %w", uid, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// HamqttDeviceBlockConflicts reports device identities that this bridge
// announces under more than one device block.
//
// It must return nothing. There used to be one — the site device was
// "UniFi Site <Site.Name>" on its seven health entities and "UniFi Site
// <Site.Internal>" on every SSID switch, so "unifi_site_default"
// carried two different `name` values. Per-entity that is only
// last-write-wins in Home Assistant's device registry; a bundle has
// exactly one device block, so one of the two would have won by
// collection order. F14 made both read [Discovery.siteDeviceInfo]; this
// is the assertion that they still do.
func (d *Discovery) HamqttDeviceBlockConflicts(f HamqttFleet) map[string][]string {
	groups := d.hamqttGroups(f)
	names := map[string][]string{}
	for _, g := range groups {
		uid := g.dev.UID()
		name := g.dev.Name.Default
		if !contains(names[uid], name) {
			names[uid] = append(names[uid], name)
		}
	}
	out := map[string][]string{}
	for uid, n := range names {
		if len(n) > 1 {
			out[uid] = n
		}
	}
	return out
}

func contains(s []string, v string) bool {
	return slices.Contains(s, v)
}

// hamqttGroup is one device and the entities announced on it.
type hamqttGroup struct {
	dev      *hamodel.Device
	entities []hamodel.Entity
}

// hamqttGroups projects the fleet onto the shared model, in the order
// the coordinator announces it: devices (values then controls), the
// SSID switches, the clients, and the site health plane.
func (d *Discovery) hamqttGroups(f HamqttFleet) []*hamqttGroup {
	var out []*hamqttGroup

	for i := range f.Devices {
		dev := &f.Devices[i]
		if dev.MAC.IsZero() {
			continue
		}
		out = append(out, d.hamqttDeviceGroup(dev, f.ControlOpts))
	}

	if f.ControlOpts.WLANEnable {
		for i := range f.WLANs {
			out = append(out, d.hamqttWLANGroup(&f.WLANs[i]))
		}
	}

	for i := range f.Clients {
		cl := &f.Clients[i]
		if cl.Key() == "" {
			continue
		}
		out = append(out, d.hamqttClientGroup(cl, f.ClientOpts, f.ControlOpts))
	}

	if f.AnnounceHealth {
		out = append(out, d.hamqttHealthGroup())
	}
	return out
}

// --- infrastructure devices ------------------------------------------

// hamqttDevice projects a [deviceInfo] onto the shared model. The block
// is built by the same [Discovery.deviceInfo] the shipped path uses, so
// a name, model or uplink change moves both paths together.
func hamqttDevice(info deviceInfo) *hamodel.Device {
	dev := &hamodel.Device{
		Name:         hamodel.L(info.Name),
		Manufacturer: info.Manufacturer,
		Model:        info.Model,
		SWVersion:    info.SWVersion,
	}
	for _, id := range info.Identifiers {
		// No namespace: hamodel.Identifier.String renders
		// "<ns>:<value>" when one is set, and this fleet's identifiers
		// are already whole. A namespace here would re-key every device
		// in Home Assistant's device registry.
		dev.Identity.IDs = append(dev.Identity.IDs, hamodel.Identifier{Value: id})
	}
	for _, c := range info.Connections {
		if len(c) == 2 {
			dev.Identity.Connections = append(dev.Identity.Connections,
				hamodel.Connection{Type: c[0], Value: c[1]})
		}
	}
	if info.ViaDevice != "" {
		dev.Via = &hamodel.Identity{IDs: []hamodel.Identifier{{Value: info.ViaDevice}}}
	}
	return dev
}

func (d *Discovery) hamqttDeviceGroup(dev *model.Device, opts ControlOptions) *hamqttGroup {
	info := d.deviceInfo(dev)
	g := &hamqttGroup{dev: hamqttDevice(info)}

	specs := deviceSpecs()
	for i := range dev.Ports {
		specs = append(specs, portSpecs(&dev.Ports[i])...)
	}
	for i := range dev.Radios {
		specs = append(specs, radioSpecs(&dev.Radios[i])...)
	}
	mac := dev.MAC.String()
	for i := range specs {
		g.entities = append(g.entities, d.hamqttDeviceEntity(&specs[i], mac, info.Name))
	}

	g.entities = append(g.entities, d.hamqttDeviceControls(dev, info, opts)...)
	return g
}

func (d *Discovery) hamqttDeviceEntity(s *spec, mac, deviceName string) *hamqttEntity {
	nameKey := s.nameKey
	if nameKey == "" {
		nameKey = s.key
	}
	label := name(nameKey, d.lang)
	if s.nameArg != "" {
		label = nameWith(nameKey, d.lang, s.nameArg)
	}

	e := &hamqttEntity{
		EntityKey:      s.key,
		EntityPlatform: hacatalog.Platform(s.platform),
		Description: hamodel.Description{
			Name:          hamodel.L(label),
			DeviceClass:   hamodel.DeviceClass(s.deviceClass),
			StateClass:    hacatalog.StateClass(s.stateClass),
			Unit:          hamodel.Unit(s.unit),
			Icon:          s.icon,
			Category:      hacatalog.EntityCategory(s.category),
			ValueTemplate: s.valueTemplate,
			// A sibling of the state topic, composed through the
			// same Topics implementation rather than by string
			// surgery on the slot.
			JSONAttributesTopic:    d.topics.DeviceTopic(mustMAC(mac), "attributes"),
			JSONAttributesTemplate: attributesTemplate,
		},
		Binds: []hamodel.Binding{{
			Role: hamodel.RoleState,
			Slot: deviceSlot(mac, s.stateSuffix),
			Mode: hamodel.Read,
		}},
		uidBase:  idPrefix + "_" + mac,
		uidKey:   s.key,
		seedName: deviceName,
		seedKey:  s.key,
	}
	// hamodel.BridgeOnly is the ONE switch that decides whether the
	// device level is rendered.
	if s.bridgeAvailOnly {
		e.Description.Availability = hamodel.BridgeOnly()
	}
	if s.platform == PlatformBinarySensor {
		e.fields = &discovery.BinarySensorFields{PayloadOn: s.payloadOn, PayloadOff: s.payloadOff}
	}
	return e
}

func (d *Discovery) hamqttDeviceControls(
	dev *model.Device, info deviceInfo, opts ControlOptions,
) []hamodel.Entity {
	mac := dev.MAC.String()
	var out []hamodel.Entity

	if opts.DeviceRestart {
		out = append(out, d.hamqttDeviceButton(mac, info.Name, "restart",
			name("device_restart", d.lang), []string{"cmd", "restart"}, "mdi:restart"))
	}
	if opts.DeviceLocate {
		e := &hamqttEntity{
			EntityKey:      "locate",
			EntityPlatform: hacatalog.PlatformSwitch,
			Description: hamodel.Description{
				Name:     hamodel.L(name("device_locate", d.lang)),
				Icon:     "mdi:map-marker-radius",
				Category: hacatalog.EntityCategory("config"),
			},
			Binds: []hamodel.Binding{
				{Role: hamodel.RoleState, Slot: deviceSlot(mac, "locate"), Mode: hamodel.Read},
				{Role: hamodel.RoleCommand, Slot: deviceSlot(mac, "cmd/locate"), Mode: hamodel.Write},
			},
			uidBase:  idPrefix + "_" + mac,
			uidKey:   "locate",
			seedName: info.Name,
			seedKey:  "locate",
			fields:   &discovery.SwitchFields{StateOn: discovery.PayloadTrue, StateOff: discovery.PayloadFalse},
		}
		out = append(out, e)
	}
	if opts.PortPowerCycle {
		for i := range dev.Ports {
			p := &dev.Ports[i]
			if p.PoE == nil {
				continue
			}
			idx := strconv.Itoa(p.Idx)
			e := d.hamqttDeviceButton(mac, info.Name, "port_"+idx+"_power_cycle",
				nameWith("port_power_cycle", d.lang, idx),
				[]string{"port", idx, "cmd", "power_cycle"}, "mdi:power-cycle")
			out = append(out, e)
		}
	}
	return out
}

func (d *Discovery) hamqttDeviceButton(
	mac, deviceName, key, label string, cmdPath []string, icon string,
) *hamqttEntity {
	return &hamqttEntity{
		EntityKey:      key,
		EntityPlatform: hacatalog.PlatformButton,
		Description: hamodel.Description{
			Name:     hamodel.L(label),
			Icon:     icon,
			Category: hacatalog.EntityCategory("config"),
		},
		Binds: []hamodel.Binding{{
			Role: hamodel.RoleCommand,
			Slot: hamodel.Slot{Scope: []string{scopeDevice}, Address: mac, Path: cmdPath},
			Mode: hamodel.Write,
		}},
		uidBase:  idPrefix + "_" + mac,
		uidKey:   key,
		seedName: deviceName,
		seedKey:  key,
		fields:   &discovery.ButtonFields{PayloadPress: "PRESS"},
	}
}

// --- clients ----------------------------------------------------------

func (d *Discovery) hamqttClientGroup(
	cl *model.Client, copts ClientOptions, ctl ControlOptions,
) *hamqttGroup {
	info := d.clientDeviceInfo(cl)
	key := cl.Key()
	g := &hamqttGroup{dev: hamqttDevice(info)}

	tracker := &hamqttEntity{
		EntityKey:      "presence",
		EntityPlatform: hacatalog.PlatformDeviceTracker,
		Description: hamodel.Description{
			Name: hamodel.L(name("client_presence", d.lang)),
			// The tracker reports being away, so it stays available
			// while the client is not.
			Availability:           hamodel.BridgeOnly(),
			JSONAttributesTopic:    d.topics.ClientTopic(key, "attributes"),
			JSONAttributesTemplate: attributesTemplate,
		},
		Binds: []hamodel.Binding{{
			Role: hamodel.RoleState, Slot: clientSlot(key, "state"), Mode: hamodel.Read,
		}},
		uidBase:  idPrefix + "_client_" + key,
		uidKey:   "presence",
		seedName: info.Name,
		seedKey:  "presence",
		fields: &discovery.DeviceTrackerFields{
			SourceType: "router", PayloadHome: "home", PayloadNotHome: "not_home",
		},
	}
	g.entities = append(g.entities, tracker, d.hamqttClientSensor(key, "ip", info.Name, spec{
		platform: PlatformSensor, key: "client_ip", nameKey: "client_ip",
		stateSuffix: "ip", category: "diagnostic", icon: "mdi:ip-network",
	}))

	if copts.Signal && cl.Type == model.ClientWireless {
		g.entities = append(g.entities, d.hamqttClientSensor(key, "signal", info.Name, spec{
			platform: PlatformSensor, key: "client_signal", nameKey: "client_signal",
			stateSuffix: "signal", unit: "dBm", deviceClass: "signal_strength",
			stateClass: "measurement", category: "diagnostic",
		}))
	}

	if ctl.ClientBlock && !cl.MAC.IsZero() {
		g.entities = append(g.entities, &hamqttEntity{
			EntityKey:      "blocked",
			EntityPlatform: hacatalog.PlatformSwitch,
			Description: hamodel.Description{
				Name:         hamodel.L(name("client_blocked", d.lang)),
				Icon:         "mdi:cancel",
				Availability: hamodel.BridgeOnly(),
			},
			Binds: []hamodel.Binding{
				{Role: hamodel.RoleState, Slot: clientSlot(key, "blocked"), Mode: hamodel.Read},
				{Role: hamodel.RoleCommand, Slot: clientSlot(key, "blocked"), Mode: hamodel.Write},
			},
			uidBase:  idPrefix + "_client_" + key,
			uidKey:   "blocked",
			seedName: info.Name,
			seedKey:  "blocked",
			fields:   &discovery.SwitchFields{StateOn: discovery.PayloadTrue, StateOff: discovery.PayloadFalse},
		})
	}
	if ctl.GuestAuthorize && cl.IsGuest {
		g.entities = append(g.entities, &hamqttEntity{
			EntityKey:      "authorize",
			EntityPlatform: hacatalog.PlatformButton,
			Description: hamodel.Description{
				Name:         hamodel.L(name("client_authorize", d.lang)),
				Icon:         "mdi:account-check",
				Availability: hamodel.BridgeOnly(),
			},
			Binds: []hamodel.Binding{{
				Role: hamodel.RoleCommand,
				Slot: clientSlot(key, "cmd/authorize"),
				Mode: hamodel.Write,
			}},
			uidBase:  idPrefix + "_client_" + key,
			uidKey:   "authorize",
			seedName: info.Name,
			seedKey:  "authorize",
			fields:   &discovery.ButtonFields{PayloadPress: "PRESS"},
		})
	}
	return g
}

func (d *Discovery) hamqttClientSensor(key, suffix, deviceName string, s spec) *hamqttEntity {
	return &hamqttEntity{
		// The component key is the topic's object-id segment, which
		// for these two sensors is NOT the unique id's suffix —
		// "ip" against "client_ip". That is 2 of the 21 configs F5
		// names, and getting it wrong retracts nothing at step 6.
		EntityKey:      suffix,
		EntityPlatform: hacatalog.Platform(s.platform),
		Description: hamodel.Description{
			Name:                   hamodel.L(name(s.nameKey, d.lang)),
			DeviceClass:            hamodel.DeviceClass(s.deviceClass),
			StateClass:             hacatalog.StateClass(s.stateClass),
			Unit:                   hamodel.Unit(s.unit),
			Icon:                   s.icon,
			Category:               hacatalog.EntityCategory(s.category),
			JSONAttributesTopic:    d.topics.ClientTopic(key, "attributes"),
			JSONAttributesTemplate: attributesTemplate,
		},
		Binds: []hamodel.Binding{{
			Role: hamodel.RoleState, Slot: clientSlot(key, s.stateSuffix), Mode: hamodel.Read,
		}},
		uidBase:  idPrefix + "_client_" + key,
		uidKey:   s.key,
		seedName: deviceName,
		seedKey:  s.key,
	}
}

// --- the site device --------------------------------------------------

func (d *Discovery) hamqttHealthGroup() *hamqttGroup {
	info := d.siteDeviceInfo()
	g := &hamqttGroup{dev: hamqttDevice(info)}
	specs := healthSpecs()
	for i := range specs {
		s := &specs[i]
		e := &hamqttEntity{
			EntityKey:      s.key,
			EntityPlatform: hacatalog.Platform(s.platform),
			Description: hamodel.Description{
				Name:                   hamodel.L(name(s.nameKey, d.lang)),
				DeviceClass:            hamodel.DeviceClass(s.deviceClass),
				StateClass:             hacatalog.StateClass(s.stateClass),
				Unit:                   hamodel.Unit(s.unit),
				Icon:                   s.icon,
				Category:               hacatalog.EntityCategory(s.category),
				ValueTemplate:          s.valueTemplate,
				Availability:           hamodel.BridgeOnly(),
				JSONAttributesTopic:    d.topics.HealthTopic("attributes"),
				JSONAttributesTemplate: attributesTemplate,
			},
			Binds: []hamodel.Binding{{
				Role: hamodel.RoleState, Slot: healthSlot(s.stateSuffix), Mode: hamodel.Read,
			}},
			uidBase:  idPrefix + "_site_" + d.site,
			uidKey:   s.key,
			seedName: info.Name,
			seedKey:  s.key,
		}
		if s.platform == PlatformBinarySensor {
			e.fields = &discovery.BinarySensorFields{PayloadOn: s.payloadOn, PayloadOff: s.payloadOff}
		}
		g.entities = append(g.entities, e)
	}
	return g
}

func (d *Discovery) hamqttWLANGroup(w *model.WLAN) *hamqttGroup {
	// The same block the health plane uses. Before F14 these two
	// composed the site's name from two different fields.
	info := d.siteDeviceInfo()
	e := &hamqttEntity{
		// The SSID switch lives on the *site* node with a
		// "wlan_<id>" object-id segment while its unique id is
		// namespaced under "unifi_wlan_<id>". Those are 2 of the 21
		// configs where unique_id != "<node_id>_<object_id>".
		EntityKey:      "wlan_" + w.ID,
		EntityPlatform: hacatalog.PlatformSwitch,
		Description: hamodel.Description{
			// The SSID is the label; a generic "Enabled" would fill
			// the device page with identical names.
			Name:         hamodel.L(w.Name),
			Icon:         "mdi:wifi",
			Availability: hamodel.BridgeOnly(),
		},
		Binds: []hamodel.Binding{
			{Role: hamodel.RoleState, Slot: wlanSlot(w.ID), Mode: hamodel.Read},
			{Role: hamodel.RoleCommand, Slot: wlanSlot(w.ID), Mode: hamodel.Write},
		},
		uidBase: idPrefix + "_wlan_" + w.ID,
		uidKey:  "enabled",
		// The one entity whose entity-id seed is not derived from its
		// device's name: the shipped builder seeds it from the literal
		// "unifi_wlan" and the SSID.
		seedName: "unifi_wlan",
		seedKey:  w.Name,
		fields:   &discovery.SwitchFields{StateOn: discovery.PayloadTrue, StateOff: discovery.PayloadFalse},
	}
	return &hamqttGroup{dev: hamqttDevice(info), entities: []hamodel.Entity{e}}
}

// --- slots ------------------------------------------------------------

func deviceSlot(mac, suffix string) hamodel.Slot {
	return hamodel.Slot{Scope: []string{scopeDevice}, Address: mac, Path: splitSuffix(suffix)}
}

func clientSlot(key, suffix string) hamodel.Slot {
	return hamodel.Slot{Scope: []string{scopeClient}, Address: key, Path: splitSuffix(suffix)}
}

func healthSlot(suffix string) hamodel.Slot {
	return hamodel.Slot{Scope: []string{scopeHealth}, Path: splitSuffix(suffix)}
}

// wlanSlot is an SSID's `enabled` item, the one item its switch reads
// and writes: under mqtt-smarthome state and command share the path.
func wlanSlot(id string) hamodel.Slot {
	return hamodel.Slot{Scope: []string{scopeWLAN}, Address: id, Path: []string{"enabled"}}
}

func splitSuffix(suffix string) []string {
	if suffix == "" {
		return nil
	}
	return strings.Split(suffix, "/")
}

// mustMAC re-parses a MAC this package itself rendered. The input is
// always [model.MAC.String]'s output, so a failure is a programming
// error rather than input this code can meet.
func mustMAC(s string) model.MAC {
	mac, err := model.ParseMAC(s)
	if err != nil {
		return ""
	}
	return mac
}
