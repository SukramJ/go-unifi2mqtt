// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
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
//   - [discovery.RawEncoding]. The zero [discovery.Encoding] is
//     EnvelopeEncoding, which attaches
//     `value_template: {{ value_json.value }}` to every entity that
//     reads a topic. This bridge publishes bare scalars, so the
//     envelope template would break all 297 state-reading entities at
//     once — and it would look like a formatting detail in review.
//   - A zero [discovery.Origin]. [discovery.RenderComponent] stamps an
//     `origin` block whenever the origin has a name, and this bridge
//     publishes none (measurement F7). Adding one is a payload addition
//     on every config and belongs in its own step.
//   - [hamqttContext.NodeID], [hamqttContext.UniqueID] and
//     [hamqttContext.ObjectID] — see each method.
//   - [hamqttContext.Availability], which is the whole of F8: the
//     shape the library defaults to is right, the spelling is not.
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

	// availTemplate maps the second availability level's payload onto
	// online/offline. It differs per device kind — "ONLINE" for
	// infrastructure, "home" for a client — and is empty on the 128
	// bridge-only entities.
	availTemplate string

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

// hamqttLayout is this bridge's topic tree as a [hatopic.Layout].
//
// It delegates to the same [Topics] implementation the shipped builders
// point their configs at, and composes no string of its own beyond
// joining a slot's path. That is deliberate: F10 counted 22 places in
// this tree that compose an MQTT topic, 18 of which remain, and a
// Layout that re-formatted "<root>/<site>/device/<mac>/<key>" would be
// the 23rd — agreeing today and free to drift tomorrow, with the
// symptom being entities that stay "unknown" forever and nothing in any
// log.
//
// go-hamqtt's own topic.Default is unusable here and it is worth saying
// why rather than leaving it to look like an oversight. It renders
// "<root>/<scope...>/<uid>/<channel>/<bucket>/<path...>", which has a
// bucket level this tree does not have; it keys devices on the device
// UID ("unifi_<mac>") where this tree uses the bare MAC; and its
// Command is always State+"/set", where this bridge spells four of its
// six command suffixes differently ("cmd/restart", "cmd/locate/set",
// "port/<n>/cmd/power_cycle", "cmd/authorize").
type hamqttLayout struct{ topics Topics }

// Slot scopes. The first scope segment names which of the four topic
// families a slot belongs to; [Topics] owns everything below it.
const (
	scopeDevice = "device"
	scopeClient = "client"
	scopeHealth = "health"
	scopeWLAN   = "wlan"
)

// State implements [hatopic.Layout].
func (l hamqttLayout) State(s hamodel.Slot) string {
	return l.render(s, s.Path)
}

// Command implements [hatopic.Layout].
//
// The path is rendered verbatim rather than with a "/set" leaf appended,
// because this bridge's command suffixes are not uniform: three are
// under "cmd/", two are a "<value>/set" sibling of the state topic, and
// one is nested inside a port. A layout that appended "/set" would
// silently produce four topics nothing subscribes to.
func (l hamqttLayout) Command(s hamodel.Slot) string {
	return l.render(s, s.Path)
}

// Availability implements [hatopic.Layout]: the object's own state
// topic, which is where this bridge's device-level availability signal
// actually is.
//
// go-hamqtt's LevelDevice names "<root>/<uid>/availability" — a topic
// this bridge does not publish and has no separate signal to put on.
// The signal *is* the state value, so the slot resolves to the state
// topic and [hamqttContext.Availability] supplies the template that
// reads it. Returning a dedicated availability topic here would leave
// 187 entities permanently grey, with a payload that looks correct.
func (l hamqttLayout) Availability(s hamodel.Slot) string {
	return l.render(s, []string{"state"})
}

// Bridge implements [hatopic.Layout].
func (l hamqttLayout) Bridge() string { return l.topics.AvailabilityTopic() }

// render resolves a slot against [Topics]. The scope names the family;
// the address is the segment that family is keyed on.
func (l hamqttLayout) render(s hamodel.Slot, path []string) string {
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
		return l.topics.DeviceTopic(mac, key)
	case scopeClient:
		return l.topics.ClientTopic(s.Address, key)
	case scopeHealth:
		return l.topics.HealthTopic(key)
	case scopeWLAN:
		return l.topics.WLANTopic(s.Address, key)
	default:
		return ""
	}
}

var _ hatopic.Layout = hamqttLayout{}

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
// per entity from [hamodel.Description.Availability], so the 128/187
// split between bridge-only and bridge+device costs nothing but a
// [hamodel.BridgeOnly] on the 128. What differs is the spelling of the
// second level: discovery.StdContext emits a dedicated availability
// topic carrying true/false, and this bridge names the object's own
// state topic with a template that maps its vocabulary onto
// online/offline.
//
// Overriding this method is the route the interface exists for, and it
// is narrow: the levels and the mode still come from the description,
// only the entry for LevelDevice is spelled differently. The
// device-level topic is resolved from the entity's own first binding
// rather than from the device, so it cannot name a topic no config of
// this entity points at.
func (c hamqttContext) Availability(dev *hamodel.Device, e hamodel.Entity) []discovery.AvailabilityEntry {
	levels, _ := e.Desc().Availability.Resolved()
	out := make([]discovery.AvailabilityEntry, 0, len(levels))
	for _, level := range levels {
		switch level {
		case hamodel.LevelBridge:
			out = append(out, discovery.AvailabilityEntry{Topic: c.layout.Bridge()})
		case hamodel.LevelDevice:
			ent, ok := e.(*hamqttEntity)
			if !ok || ent.availTemplate == "" {
				continue
			}
			binds := e.Bindings()
			if len(binds) == 0 {
				continue
			}
			out = append(out, discovery.AvailabilityEntry{
				Topic:         c.layout.Availability(binds[0].Slot),
				ValueTemplate: ent.availTemplate,
			})
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
		StdContext: discovery.StdContext{
			Layout:    layout,
			Namespace: hamqttNamespace,
			Lang:      d.lang,
			// Raw, not the zero value. See the file header.
			Enc: discovery.RawEncoding,
		},
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
	// SiteName is the site's display name, which is what the health
	// entities' device block carries.
	SiteName string
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
			comp, err := discovery.RenderComponent(ctx, g.dev, e, discovery.Origin{})
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
// There is one today, and it is a step-6 gate rather than a curiosity:
// the site device is "UniFi Site <Site.Name>" on its seven health
// entities and "UniFi Site <Site.Internal>" on every SSID switch, so
// "unifi_site_default" carries two different `name` values. Per-entity
// that is only last-write-wins in Home Assistant's device registry; a
// bundle has exactly one device block, so one of the two names has to
// win visibly.
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
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
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
		out = append(out, d.hamqttHealthGroup(f.SiteName))
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

// deviceAvailTemplate maps the ten model.DeviceState strings onto
// online/offline. It is the literal the shipped builder writes.
const deviceAvailTemplate = "{{ 'online' if value == 'ONLINE' else 'offline' }}"

// clientAvailTemplate is the client plane's equivalent.
const clientAvailTemplate = "{{ 'online' if value == 'home' else 'offline' }}"

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
		Basic: hamodel.Basic{
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
				JSONAttributesTopic: d.topics.DeviceTopic(mustMAC(mac), "attributes"),
			},
			Binds: []hamodel.Binding{{
				Role: hamodel.RoleState,
				Slot: deviceSlot(mac, s.stateSuffix),
				Mode: hamodel.Read,
			}},
		},
		uidBase:  idPrefix + "_" + mac,
		uidKey:   s.key,
		seedName: deviceName,
		seedKey:  s.key,
	}
	// The template is a property of the device kind, not of the entity:
	// it is set on every infrastructure entity, and hamodel.BridgeOnly
	// is the ONE switch that decides whether the level is rendered. Two
	// switches saying the same thing is how a setting goes inert
	// without a test noticing.
	e.availTemplate = deviceAvailTemplate
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
			Basic: hamodel.Basic{
				EntityKey:      "locate",
				EntityPlatform: hacatalog.PlatformSwitch,
				Description: hamodel.Description{
					Name:     hamodel.L(name("device_locate", d.lang)),
					Icon:     "mdi:map-marker-radius",
					Category: hacatalog.EntityCategory("config"),
				},
				Binds: []hamodel.Binding{
					{Role: hamodel.RoleState, Slot: deviceSlot(mac, "locate"), Mode: hamodel.Read},
					{Role: hamodel.RoleCommand, Slot: deviceSlot(mac, "cmd/locate/set"), Mode: hamodel.Write},
				},
			},
			uidBase:       idPrefix + "_" + mac,
			uidKey:        "locate",
			seedName:      info.Name,
			seedKey:       "locate",
			availTemplate: deviceAvailTemplate,
			fields:        &discovery.SwitchFields{StateOn: payloadON, StateOff: payloadOFF},
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
		Basic: hamodel.Basic{
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
		},
		uidBase:       idPrefix + "_" + mac,
		uidKey:        key,
		seedName:      deviceName,
		seedKey:       key,
		availTemplate: deviceAvailTemplate,
		fields:        &discovery.ButtonFields{PayloadPress: "PRESS"},
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
		Basic: hamodel.Basic{
			EntityKey:      "presence",
			EntityPlatform: hacatalog.PlatformDeviceTracker,
			Description: hamodel.Description{
				Name: hamodel.L(name("client_presence", d.lang)),
				// The tracker reports being away, so it stays available
				// while the client is not.
				Availability:        hamodel.BridgeOnly(),
				JSONAttributesTopic: d.topics.ClientTopic(key, "attributes"),
			},
			Binds: []hamodel.Binding{{
				Role: hamodel.RoleState, Slot: clientSlot(key, "state"), Mode: hamodel.Read,
			}},
		},
		uidBase:       idPrefix + "_client_" + key,
		uidKey:        "presence",
		seedName:      info.Name,
		seedKey:       "presence",
		availTemplate: clientAvailTemplate,
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
			Basic: hamodel.Basic{
				EntityKey:      "blocked",
				EntityPlatform: hacatalog.PlatformSwitch,
				Description: hamodel.Description{
					Name:         hamodel.L(name("client_blocked", d.lang)),
					Icon:         "mdi:cancel",
					Availability: hamodel.BridgeOnly(),
				},
				Binds: []hamodel.Binding{
					{Role: hamodel.RoleState, Slot: clientSlot(key, "blocked"), Mode: hamodel.Read},
					{Role: hamodel.RoleCommand, Slot: clientSlot(key, "blocked/set"), Mode: hamodel.Write},
				},
			},
			uidBase:       idPrefix + "_client_" + key,
			uidKey:        "blocked",
			seedName:      info.Name,
			seedKey:       "blocked",
			availTemplate: clientAvailTemplate,
			fields:        &discovery.SwitchFields{StateOn: payloadON, StateOff: payloadOFF},
		})
	}
	if ctl.GuestAuthorize && cl.IsGuest {
		g.entities = append(g.entities, &hamqttEntity{
			Basic: hamodel.Basic{
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
			},
			uidBase:       idPrefix + "_client_" + key,
			uidKey:        "authorize",
			seedName:      info.Name,
			seedKey:       "authorize",
			availTemplate: clientAvailTemplate,
			fields:        &discovery.ButtonFields{PayloadPress: "PRESS"},
		})
	}
	return g
}

func (d *Discovery) hamqttClientSensor(key, suffix, deviceName string, s spec) *hamqttEntity {
	return &hamqttEntity{
		Basic: hamodel.Basic{
			// The component key is the topic's object-id segment, which
			// for these two sensors is NOT the unique id's suffix —
			// "ip" against "client_ip". That is 2 of the 21 configs F5
			// names, and getting it wrong retracts nothing at step 6.
			EntityKey:      suffix,
			EntityPlatform: hacatalog.Platform(s.platform),
			Description: hamodel.Description{
				Name:                hamodel.L(name(s.nameKey, d.lang)),
				DeviceClass:         hamodel.DeviceClass(s.deviceClass),
				StateClass:          hacatalog.StateClass(s.stateClass),
				Unit:                hamodel.Unit(s.unit),
				Icon:                s.icon,
				Category:            hacatalog.EntityCategory(s.category),
				JSONAttributesTopic: d.topics.ClientTopic(key, "attributes"),
			},
			Binds: []hamodel.Binding{{
				Role: hamodel.RoleState, Slot: clientSlot(key, s.stateSuffix), Mode: hamodel.Read,
			}},
		},
		uidBase:       idPrefix + "_client_" + key,
		uidKey:        s.key,
		seedName:      deviceName,
		seedKey:       s.key,
		availTemplate: clientAvailTemplate,
	}
}

// --- the site device --------------------------------------------------

func (d *Discovery) hamqttHealthGroup(siteName string) *hamqttGroup {
	info := deviceInfo{
		Identifiers:  []string{siteDeviceID(d.site)},
		Name:         "UniFi Site " + siteName,
		Manufacturer: Manufacturer,
		Model:        "Site",
	}
	g := &hamqttGroup{dev: hamqttDevice(info)}
	specs := healthSpecs()
	for i := range specs {
		s := &specs[i]
		e := &hamqttEntity{
			Basic: hamodel.Basic{
				EntityKey:      s.key,
				EntityPlatform: hacatalog.Platform(s.platform),
				Description: hamodel.Description{
					Name:                hamodel.L(name(s.nameKey, d.lang)),
					DeviceClass:         hamodel.DeviceClass(s.deviceClass),
					StateClass:          hacatalog.StateClass(s.stateClass),
					Unit:                hamodel.Unit(s.unit),
					Icon:                s.icon,
					Category:            hacatalog.EntityCategory(s.category),
					ValueTemplate:       s.valueTemplate,
					Availability:        hamodel.BridgeOnly(),
					JSONAttributesTopic: d.topics.HealthTopic("attributes"),
				},
				Binds: []hamodel.Binding{{
					Role: hamodel.RoleState, Slot: healthSlot(s.stateSuffix), Mode: hamodel.Read,
				}},
			},
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
	info := deviceInfo{
		Identifiers:  []string{siteDeviceID(d.site)},
		Name:         "UniFi Site " + d.site,
		Manufacturer: Manufacturer,
		Model:        "Site",
	}
	e := &hamqttEntity{
		Basic: hamodel.Basic{
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
				{Role: hamodel.RoleState, Slot: wlanSlot(w.ID, "enabled"), Mode: hamodel.Read},
				{Role: hamodel.RoleCommand, Slot: wlanSlot(w.ID, "enabled/set"), Mode: hamodel.Write},
			},
		},
		uidBase: idPrefix + "_wlan_" + w.ID,
		uidKey:  "enabled",
		// The one entity whose entity-id seed is not derived from its
		// device's name: the shipped builder seeds it from the literal
		// "unifi_wlan" and the SSID.
		seedName: "unifi_wlan",
		seedKey:  w.Name,
		fields:   &discovery.SwitchFields{StateOn: payloadON, StateOff: payloadOFF},
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

func wlanSlot(id, suffix string) hamodel.Slot {
	return hamodel.Slot{Scope: []string{scopeWLAN}, Address: id, Path: splitSuffix(suffix)}
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
