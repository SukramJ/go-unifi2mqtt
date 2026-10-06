// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// The one-time re-point of 1.x discovery configs (2.0.1).
//
// 2.0.0 kept every config topic and unique_id and relied on each class
// re-announcing its entities on the first run after the upgrade. Clients
// do not: a client's configs are published on its first sighting
// ([Coordinator.trackPresent]) and nowhere else, which is also all 1.3.0
// did — 1.3.0 kept no client list across a restart and never retracted a
// client config, so an absent client lived on in Home Assistant on its
// retained config, its retained `state` and `<root>/bridge/status`.
// After 2.0.0 those three no longer agree: `bridge/status` is gone (the
// migration sweep evicts it, and nothing writes it), Home Assistant
// holds every availability topic at "not available" until a matching
// payload arrives (homeassistant/components/mqtt/entity.py, the
// `_available` dict seeded False in `_availability_prepare_subscribe_topics`
// and read by `available` under availability_mode "all"), and the old
// state topic is no longer written. A client that stayed away across
// the upgrade therefore sat `unavailable` instead of `not_home`, and one
// that never came back stayed a ghost the orphan reconcile could not
// even see, because [hass.Discovery.IsOwnConfig] requires the new
// `<name>/connected` topic.
//
// This pass reads the retained configs back once their class's source
// has reported, recognises the ones this instance wrote in the 1.x
// shape ([parseLegacyConfig]) and settles each of them:
//
//   - re-pointed, when the current configuration produces that entity:
//     the config is re-rendered by the ordinary builders from the
//     object this run polled or, for an absent client, from the
//     identity the old config and its retained 1.x items carry; an
//     absent client then gets the status items its entities read
//     (`state` not_home, `online` false, `attributes`, and `blocked`
//     when the old value is known);
//   - retracted with a coordinator.migration_retracted line, when it
//     does not: a device no longer on the console, a port or radio the
//     device no longer has, an SSID no longer in the catalogue, or an
//     entity whose option or source this installation has switched off.
//
// It lives beside the 1.x state sweep in migrate.go and goes with it.

// The 1.x templates of an entity's second availability entry, verbatim:
// the object's raw `state` payload mapped onto online/offline.
const (
	legacyDeviceAvailability = "{{ 'online' if value == 'ONLINE' else 'offline' }}"
	legacyClientAvailability = "{{ 'online' if value == 'home' else 'offline' }}"
)

// legacyAvailability is one 1.x availability entry. The two payload keys
// are pointers so a config carrying them — which no 1.x config did — is
// told apart from one that omits them.
type legacyAvailability struct {
	Topic               string  `json:"topic"`
	ValueTemplate       string  `json:"value_template"`
	PayloadAvailable    *string `json:"payload_available"`
	PayloadNotAvailable *string `json:"payload_not_available"`
}

// legacyPayload is the part of a 1.x discovery config the pass reads.
type legacyPayload struct {
	UniqueID          string               `json:"unique_id"`
	Name              string               `json:"name"`
	StateTopic        string               `json:"state_topic"`
	CommandTopic      string               `json:"command_topic"`
	AttributesTopic   string               `json:"json_attributes_topic"`
	AvailabilityTopic string               `json:"availability_topic"`
	Availability      []legacyAvailability `json:"availability"`
	Device            struct {
		Identifiers []string   `json:"identifiers"`
		Connections [][]string `json:"connections"`
		Name        string     `json:"name"`
		ViaDevice   string     `json:"via_device"`
	} `json:"device"`
}

// legacyConfig is one retained 1.x config this instance owns.
type legacyConfig struct {
	topic string
	class hass.Class
	// id is the object the config belongs to: the bare MAC of a device,
	// a client's key, an SSID's id; empty for a site-health entity.
	id string
	p  legacyPayload
}

// parseLegacyConfig reports whether a retained discovery config is one
// this instance wrote in the 1.x layout, and what it belongs to.
//
// Every condition is exact; none is a prefix match on a payload:
//
//   - the topic is `<prefix>/<platform>/<node_id>/<object_id>/config` on a
//     platform this bridge publishes, and the node id is one it mints —
//     `unifi_<bare mac>`, `unifi_client_<key>` or `unifi_site_<site>`
//     with this instance's site — and equals `device.identifiers`;
//   - the unique_id is the one this bridge mints under that node id;
//   - the availability list is exactly the 1.x one: `<root>/bridge/status`
//     under this instance's root, plus for a device or client entity at
//     most its own `<root>/<site>/…/state` with the verbatim 1.x template;
//   - every state, command and attributes topic is a 1.x item of the
//     same object under this instance's root and site ([oldLayoutTopic]).
//
// Another root, another site, another integration's config and a config
// already in the 2.x layout all fail at least one of them. A second
// console bridged under the *same* name and site cannot be told apart —
// 1.x could not either, and 2.0.0 made the name the instance's identity.
func parseLegacyConfig(root, site, siteRef, topic string, payload []byte) (legacyConfig, bool) {
	levels := strings.Split(topic, "/")
	n := len(levels)
	if n < 5 || levels[n-1] != "config" {
		return legacyConfig{}, false
	}
	platform, node, object := levels[n-4], levels[n-3], levels[n-2]
	if !slices.Contains(hass.PublishedPlatforms(), platform) || object == "" {
		return legacyConfig{}, false
	}

	var p legacyPayload
	if json.Unmarshal(payload, &p) != nil {
		return legacyConfig{}, false
	}
	if len(p.Device.Identifiers) != 1 || p.Device.Identifiers[0] != node || p.AvailabilityTopic != "" {
		return legacyConfig{}, false
	}

	cfg := legacyConfig{topic: topic, p: p}
	var kind, seg string
	switch {
	case strings.HasPrefix(node, "unifi_client_"):
		cfg.class, kind = hass.ClassClient, "client"
		cfg.id = strings.TrimPrefix(node, "unifi_client_")
		seg = sanitiseSegment(cfg.id)
		if cfg.id == "" || !strings.HasPrefix(p.UniqueID, node+"_") {
			return legacyConfig{}, false
		}
	case node == "unifi_site_"+siteRef:
		if id, ok := strings.CutPrefix(object, "wlan_"); ok {
			cfg.class, kind, cfg.id = hass.ClassWLAN, "wlan", id
			seg = sanitiseSegment(id)
			if id == "" || p.UniqueID != "unifi_wlan_"+id+"_enabled" {
				return legacyConfig{}, false
			}
		} else {
			cfg.class, kind = hass.ClassSite, "health"
			if !strings.HasPrefix(p.UniqueID, node+"_") {
				return legacyConfig{}, false
			}
		}
	default:
		mac, ok := strings.CutPrefix(node, "unifi_")
		if !ok || !isBareMAC(mac) || !strings.HasPrefix(p.UniqueID, node+"_") {
			return legacyConfig{}, false
		}
		cfg.class, kind, cfg.id, seg = hass.ClassDevice, "device", mac, mac
	}
	if hass.ClassOf(p.UniqueID) != cfg.class {
		return legacyConfig{}, false
	}

	if !legacyAvailabilityOK(root, site, kind, seg, p.Availability) {
		return legacyConfig{}, false
	}

	ids := ownedIDs{}
	switch kind {
	case "device":
		ids.devices = map[string]bool{seg: true}
	case "client":
		ids.clients = map[string]bool{seg: true}
	case "wlan":
		ids.wlans = map[string]bool{seg: true}
	}
	for _, t := range []string{p.StateTopic, p.CommandTopic, p.AttributesTopic} {
		if t == "" {
			continue
		}
		if !oldLayoutTopic(root, site, ids, t) || strings.HasPrefix(t, root+"/"+bridgeSegment+"/") {
			return legacyConfig{}, false
		}
		if kind == "health" && !strings.HasPrefix(t, root+"/"+site+"/health/") {
			return legacyConfig{}, false
		}
	}
	return cfg, true
}

// legacyAvailabilityOK checks the availability list against the exact
// 1.x shape for an entity of kind on the object whose topic segment is
// seg.
func legacyAvailabilityOK(root, site, kind, seg string, avail []legacyAvailability) bool {
	if len(avail) == 0 || len(avail) > 2 {
		return false
	}
	for _, a := range avail {
		if a.PayloadAvailable != nil || a.PayloadNotAvailable != nil {
			return false
		}
	}
	if avail[0].Topic != root+"/"+bridgeSegment+"/status" || avail[0].ValueTemplate != "" {
		return false
	}
	if len(avail) == 1 {
		return true
	}
	second := avail[1]
	switch kind {
	case "device":
		return second.Topic == root+"/"+site+"/device/"+seg+"/state" &&
			second.ValueTemplate == legacyDeviceAvailability
	case "client":
		return second.Topic == root+"/"+site+"/client/"+seg+"/state" &&
			second.ValueTemplate == legacyClientAvailability
	default:
		return false
	}
}

// isBareMAC reports whether s is a MAC in the form [model.MAC.String]
// produces: twelve lowercase hex digits.
func isBareMAC(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// repointResult is what one pass did: how many configs it re-pointed and
// retracted, and which objects' 1.x status items are now this
// instance's to evict.
type repointResult struct {
	repointed, retracted int
	ids                  ownedIDs
}

// repointLegacyConfigs settles every 1.x config of a class in classes
// found in retained, the configs a discovery window read back. oldItems
// is what the 1.x state window read: an absent client's last `blocked`
// value and attributes come from there.
func (c *Coordinator) repointLegacyConfigs(
	ctx context.Context,
	retained, oldItems map[string][]byte,
	classes map[hass.Class]bool,
) repointResult {
	res := repointResult{ids: ownedIDs{
		devices: map[string]bool{}, clients: map[string]bool{}, wlans: map[string]bool{},
	}}
	if c.hass == nil {
		return res
	}
	root, site := c.topics.name(), c.topics.site

	groups := map[hass.Class]map[string][]legacyConfig{}
	for topic, payload := range retained {
		cfg, ok := parseLegacyConfig(root, site, c.site.Internal, topic, payload)
		if !ok || !classes[cfg.class] {
			continue
		}
		if groups[cfg.class] == nil {
			groups[cfg.class] = map[string][]legacyConfig{}
		}
		groups[cfg.class][cfg.id] = append(groups[cfg.class][cfg.id], cfg)
	}

	for _, id := range sortedIDs(groups[hass.ClassClient]) {
		c.repointClient(ctx, id, groups[hass.ClassClient][id], oldItems, &res)
	}
	for _, id := range sortedIDs(groups[hass.ClassDevice]) {
		c.repointDevice(ctx, id, groups[hass.ClassDevice][id], &res)
	}
	for _, id := range sortedIDs(groups[hass.ClassWLAN]) {
		c.repointWLAN(ctx, id, groups[hass.ClassWLAN][id], &res)
	}
	if cfgs := groups[hass.ClassSite][""]; len(cfgs) > 0 {
		c.repointHealth(ctx, cfgs, &res)
	}

	if res.repointed > 0 {
		c.log.Info("coordinator.migration_repointed",
			slog.Int("configs", res.repointed),
			slog.String("note", "Home Assistant entities of the 1.x layout moved to the 2.x topics"))
	}
	return res
}

// settle re-points every config of one object that rendered contains
// and retracts the rest, logging the retraction with reason. It returns
// how many it re-pointed.
func (c *Coordinator) settle(
	ctx context.Context,
	object string,
	cfgs []legacyConfig,
	rendered []hass.Entry,
	reason string,
	res *repointResult,
) int {
	byTopic := make(map[string][]byte, len(rendered))
	for _, e := range rendered {
		byTopic[e.ConfigTopic] = e.Payload
	}
	var retracted []string
	repointed := 0
	for i := range cfgs {
		cfg := &cfgs[i]
		// Published by this process since the window read it: a live
		// entity of this run, which the pass must never overwrite.
		if c.pub.wasPublished(cfg.topic) {
			continue
		}
		if payload, ok := byTopic[cfg.topic]; ok {
			if err := c.pub.publishConfig(ctx, cfg.topic, payload); err != nil {
				c.log.Warn("coordinator.migration_repoint_failed",
					slog.String("topic", cfg.topic), slog.String("err", err.Error()))
				continue
			}
			repointed++
			continue
		}
		if err := c.clearConfig(ctx, cfg.topic); err != nil {
			c.log.Warn("coordinator.migration_retract_failed",
				slog.String("topic", cfg.topic), slog.String("err", err.Error()))
			continue
		}
		retracted = append(retracted, cfg.topic)
	}
	if len(retracted) > 0 {
		slices.Sort(retracted)
		c.log.Info("coordinator.migration_retracted",
			slog.String("object", object),
			slog.String("reason", reason),
			slog.Any("topics", retracted))
	}
	res.repointed += repointed
	res.retracted += len(retracted)
	return repointed
}

// configuredControls is [Coordinator.controlOptions] without the live
// capability check: the controls this installation is configured to
// offer when its classic layer answers. A capability that is down for
// the moment must not cost an entity its registry entry.
func (c *Coordinator) configuredControls() hass.ControlOptions {
	if !c.cfg.Controls.Enable {
		return hass.ControlOptions{}
	}
	ctl, classic := c.cfg.Controls, c.cfg.ClassicEnable
	return hass.ControlOptions{
		DeviceRestart:  ctl.DeviceRestart,
		PortPowerCycle: ctl.PortPowerCycle,
		GuestAuthorize: ctl.GuestAuthorize,
		DeviceLocate:   ctl.DeviceLocate && classic,
		ClientBlock:    ctl.ClientBlock && classic,
		WLANEnable:     ctl.WLANEnable && classic,
	}
}

// repointClient settles one client's 1.x configs.
//
// A client this run has seen is rendered from its live record. An absent
// one is rebuilt from what its configs and retained 1.x items carry, and
// keeps every entity the configuration still produces: 1.3.0 never
// retracted a client, and an absent client is "away", not "gone".
func (c *Coordinator) repointClient(
	ctx context.Context,
	key string,
	cfgs []legacyConfig,
	oldItems map[string][]byte,
	res *repointResult,
) {
	c.mu.RLock()
	st, present := c.clients[key]
	c.mu.RUnlock()

	cl, blocked := st.client, (*bool)(nil)
	if !present {
		cl, blocked = c.legacyClient(key, cfgs, oldItems)
	}

	var rendered []hass.Entry
	if c.cfg.Clients.Enable {
		entries, err := c.hass.Client(&cl, hass.ClientOptions{
			Signal: c.cfg.Clients.SignalSensor && c.cfg.ClassicEnable,
		})
		if err != nil {
			c.log.Warn("coordinator.migration_repoint_failed",
				slog.String("client", key), slog.String("err", err.Error()))
			return
		}
		controls, err := c.hass.ClientControls(&cl, c.configuredControls())
		if err != nil {
			c.log.Warn("coordinator.migration_repoint_failed",
				slog.String("client", key), slog.String("err", err.Error()))
			return
		}
		rendered = slices.Concat(entries, controls)
	}

	repointed := c.settle(ctx, "client "+key, cfgs, rendered,
		"the configuration no longer produces this client entity", res)
	res.ids.clients[sanitiseSegment(key)] = true
	if present || repointed == 0 {
		return
	}
	if err := c.publishAwayClient(ctx, &cl, blocked); err != nil {
		c.log.Warn("coordinator.client_publish_failed",
			slog.String("client", cl.Name), slog.String("err", err.Error()))
	}
}

// legacyClient rebuilds an absent client from its 1.x configs and
// retained 1.x items: the name, MAC and uplink from the `device` block,
// the rest from the attributes document. blocked is the last `blocked`
// value, or nil when none is retained.
func (c *Coordinator) legacyClient(
	key string,
	cfgs []legacyConfig,
	oldItems map[string][]byte,
) (cl model.Client, blocked *bool) {
	cl = model.Client{ID: key}
	if mac, err := model.ParseMAC(key); err == nil && isBareMAC(key) {
		cl.MAC = mac
	}
	for i := range cfgs {
		cfg := &cfgs[i]
		dev := cfg.p.Device
		if dev.Name != "" && !strings.HasPrefix(dev.Name, "UniFi client ") {
			cl.Name = dev.Name
		}
		for _, conn := range dev.Connections {
			if len(conn) == 2 && conn[0] == "mac" && cl.MAC.IsZero() {
				if mac, err := model.ParseMAC(conn[1]); err == nil {
					cl.MAC = mac
				}
			}
		}
		if via, ok := strings.CutPrefix(dev.ViaDevice, "unifi_"); ok && isBareMAC(via) {
			cl.UplinkMAC = model.MAC(via)
		}
		// The two entities only a wireless client and a guest get.
		switch {
		case strings.HasSuffix(cfg.p.UniqueID, "_client_signal"):
			cl.Type = model.ClientWireless
		case strings.HasSuffix(cfg.p.UniqueID, "_authorize"):
			cl.IsGuest = true
		}
	}

	root, site, seg := c.topics.name(), c.topics.site, sanitiseSegment(key)
	item := func(name string) []byte { return oldItems[root+"/"+site+"/client/"+seg+"/"+name] }

	var attrs clientAttrs
	if json.Unmarshal(item(keyAttributes), &attrs) == nil {
		if cl.Name == "" {
			cl.Name = attrs.Name
		}
		if cl.Type == "" {
			cl.Type = model.ClientType(attrs.Type)
		}
		cl.IsGuest = cl.IsGuest || attrs.Guest
		cl.Network, cl.VLAN, cl.SSID = attrs.Network, attrs.VLAN, attrs.SSID
	}

	switch string(item(keyClientBlocked)) {
	case legacyON:
		b := true
		blocked = &b
	case legacyOFF:
		b := false
		blocked = &b
	}
	if blocked != nil {
		cl.Blocked = *blocked
	}
	return cl, blocked
}

// The 1.x binary payloads, as the old `blocked` item carried them.
const (
	legacyON  = "ON"
	legacyOFF = "OFF"
)

// publishAwayClient writes the status items a re-pointed absent
// client's entities read: `state` not_home and `online` false, which is
// what its tracker and sensors showed under 1.3.0 for a client that was
// away; no IP and no signal, since a stale reading would suggest it is
// still reachable; its attributes; and `blocked` when the old value is
// known — guessing "not blocked" for a switch would be inventing state.
func (c *Coordinator) publishAwayClient(ctx context.Context, cl *model.Client, blocked *bool) error {
	key := cl.Key()
	values := []struct {
		key   string
		value any
	}{
		{keyState, payloadNotHome},
		{keyOnline, false},
		{keyClientIP, nil},
	}
	if blocked != nil {
		values = append(values, struct {
			key   string
			value any
		}{keyClientBlocked, *blocked})
	}
	if cl.Type == model.ClientWireless {
		values = append(values, struct {
			key   string
			value any
		}{keyClientSignal, nil})
	}
	for _, v := range values {
		if err := c.pub.publish(ctx, c.topics.client(key, v.key), v.value); err != nil {
			return err
		}
	}
	return c.pub.publish(ctx, c.topics.client(key, keyAttributes), clientAttributes(cl, false))
}

// repointDevice settles one infrastructure device's 1.x configs: a
// device the console still lists is rendered from this run's detail
// snapshot, and a device it no longer lists is retracted — 1.3.0's own
// rule for a device that disappears from the poll
// ([Coordinator.sweepDevices]).
func (c *Coordinator) repointDevice(ctx context.Context, mac string, cfgs []legacyConfig, res *repointResult) {
	c.mu.RLock()
	dev, listed := c.details[model.MAC(mac)]
	_, seen := c.seen[model.MAC(mac)]
	c.mu.RUnlock()

	switch {
	case !listed && !seen:
		c.settle(ctx, "device "+mac, cfgs, nil, "the device is no longer on the console", res)
		res.ids.devices[mac] = true
		return
	case !listed:
		// Listed by the device poll but not in the detail snapshot: the
		// two are a poll apart, and nothing here is certain enough to act.
		return
	}

	entries, err := c.hass.Device(&dev)
	if err != nil {
		c.log.Warn("coordinator.migration_repoint_failed",
			slog.String("device", mac), slog.String("err", err.Error()))
		return
	}
	controls, err := c.hass.DeviceControls(&dev, c.configuredControls())
	if err != nil {
		c.log.Warn("coordinator.migration_repoint_failed",
			slog.String("device", mac), slog.String("err", err.Error()))
		return
	}
	c.settle(ctx, "device "+mac, cfgs, append(entries, controls...),
		"the device no longer has this port or radio, or the configuration no longer produces the entity", res)
}

// repointWLAN settles one SSID switch.
func (c *Coordinator) repointWLAN(ctx context.Context, id string, cfgs []legacyConfig, res *repointResult) {
	c.mu.RLock()
	w, listed := c.wlanByID[id]
	c.mu.RUnlock()

	if !listed {
		c.settle(ctx, "wlan "+id, cfgs, nil, "the SSID is no longer in the console's WLAN catalogue", res)
		res.ids.wlans[sanitiseSegment(id)] = true
		return
	}
	var rendered []hass.Entry
	if c.configuredControls().WLANEnable {
		e, err := c.hass.WLANControl(&w)
		if err != nil {
			c.log.Warn("coordinator.migration_repoint_failed",
				slog.String("wlan", id), slog.String("err", err.Error()))
			return
		}
		rendered = []hass.Entry{e}
	}
	c.settle(ctx, "wlan "+id, cfgs, rendered, "the SSID switch is not enabled in this configuration", res)
}

// repointHealth settles the site-health configs. Their class is ready
// only once the classic layer has answered, or when it is switched off —
// so a config left here is either one the classic layer no longer has a
// value for or one CLASSIC_ENABLE has switched off.
func (c *Coordinator) repointHealth(ctx context.Context, cfgs []legacyConfig, res *repointResult) {
	var rendered []hass.Entry
	reason := "site health needs the classic layer, which this configuration does not enable"
	if c.cfg.ClassicEnable {
		entries, err := c.hass.Health()
		if err != nil {
			c.log.Warn("coordinator.migration_repoint_failed",
				slog.String("site", c.site.Internal), slog.String("err", err.Error()))
			return
		}
		rendered = entries
		reason = "this version has no such site-health entity"
	}
	c.settle(ctx, "site "+c.site.Internal, cfgs, rendered, reason, res)
}

// sortedIDs returns m's keys in order, so the pass and its log lines
// are deterministic.
func sortedIDs[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
