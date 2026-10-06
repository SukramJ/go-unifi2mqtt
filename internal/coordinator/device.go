// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"math"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Value publication.
//
// Every entity-shaped value gets its own status item rather than being
// packed into one JSON blob: Home Assistant then needs one state_topic
// per sensor reading `value_json.val` instead of a value_template maze
// (CONCEPT.md §5.6). What is useful to see but pointless as its own
// entity goes into the accompanying `attributes` item, whose `val` is
// the object.
//
// Each value is handed over as the Go value its `val` should be: a
// boolean as a JSON boolean, a count or a rate as a JSON number, a
// state as its English token. The publisher wraps it in the
// `{"val","ts","lc"}` status object (mqtt-smarthome 2.0 §5.2). nil
// means "no value" and clears the retained item.
//
// A publish failure on one value is returned to the caller, which logs
// it and moves on — losing one sensor for one cycle is better than
// aborting the whole poll.

// publishDevice publishes one device's state, statistics-independent
// values, ports and radios.
func (c *Coordinator) publishDevice(ctx context.Context, d *model.Device) error {
	values := []struct {
		key   string
		value any
	}{
		{keyState, string(d.State)},
		// Reachability as the boolean `online` item every non-state
		// entity of the device reads as its second availability entry.
		{keyOnline, d.State.IsOnline()},
		{keyFirmware, d.Firmware},
		{keyUpdateAvailable, d.UpdateAvail},
	}
	for _, v := range values {
		if err := c.pub.publish(ctx, c.topics.device(d.MAC, v.key), v.value); err != nil {
			return err
		}
	}

	if err := c.publishDeviceAttributes(ctx, d); err != nil {
		return err
	}
	for i := range d.Ports {
		if err := c.publishPort(ctx, d.MAC, &d.Ports[i]); err != nil {
			return err
		}
	}
	for i := range d.Radios {
		if err := c.publishRadio(ctx, d.MAC, &d.Radios[i]); err != nil {
			return err
		}
	}
	return nil
}

// deviceAttributes is the JSON object bound as json_attributes_topic.
// Fields are omitted when unset so a gateway does not advertise an
// empty uplink and a device with no IP does not advertise an empty
// string that reads like a real value.
type deviceAttributes struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Type      string `json:"type"`
	MAC       string `json:"mac"`
	IP        string `json:"ip,omitempty"`
	Uplink    string `json:"uplink_mac,omitempty"`
	AdoptedAt string `json:"adopted_at,omitempty"`
	Supported bool   `json:"supported"`
	Ports     int    `json:"ports,omitempty"`
	Radios    int    `json:"radios,omitempty"`
}

func (c *Coordinator) publishDeviceAttributes(ctx context.Context, d *model.Device) error {
	attrs := deviceAttributes{
		Name:      d.Name,
		Model:     d.Model,
		Type:      string(d.Type),
		MAC:       d.MAC.Colon(),
		Supported: d.Supported,
		Ports:     len(d.Ports),
		Radios:    len(d.Radios),
	}
	if d.IP.IsValid() {
		attrs.IP = d.IP.String()
	}
	if !d.UplinkMAC.IsZero() {
		attrs.Uplink = d.UplinkMAC.Colon()
	}
	if !d.AdoptedAt.IsZero() {
		attrs.AdoptedAt = d.AdoptedAt.UTC().Format("2006-01-02T15:04:05Z")
	}

	return c.pub.publish(ctx, c.topics.device(d.MAC, keyAttributes), attrs)
}

// publishDeviceStats publishes one statistics sample.
//
// Uptime is published in seconds because that is what Home Assistant's
// duration device_class expects, and radio retry rates are keyed by
// band since the API gives radios no other identifier.
func (c *Coordinator) publishDeviceStats(ctx context.Context, mac model.MAC, s *model.DeviceStats) error {
	values := []struct {
		key   string
		value any
	}{
		{keyUptime, int64(s.Uptime.Seconds())},
		{keyCPUUtilization, roundPct(s.CPUPct)},
		{keyMemoryUtilization, roundPct(s.MemoryPct)},
		{keyUplinkTxBps, s.UplinkTxBps},
		{keyUplinkRxBps, s.UplinkRxBps},
	}
	for _, v := range values {
		if err := c.pub.publish(ctx, c.topics.device(mac, v.key), v.value); err != nil {
			return err
		}
	}

	for freq, pct := range s.RadioTxRetry {
		topic := c.topics.radio(mac, freq, keyRadioTxRetries)
		if err := c.pub.publish(ctx, topic, roundPct(pct)); err != nil {
			return err
		}
	}
	return nil
}

// publishPort publishes one port's link state, speed and PoE state.
func (c *Coordinator) publishPort(ctx context.Context, mac model.MAC, p *model.Port) error {
	if err := c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortState), string(p.State)); err != nil {
		return err
	}
	if err := c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortSpeed), p.SpeedMbps); err != nil {
		return err
	}
	// A port without PoE hardware gets no PoE topic at all — publishing
	// false there would create a Home Assistant entity for a capability
	// the port does not have.
	if p.PoE == nil {
		return nil
	}
	if err := c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortPoE), p.PoE.Enabled); err != nil {
		return err
	}

	// Wattage exists only with the classic layer, and only for ports
	// actually delivering power. Publishing 0 W for the rest would fill
	// Home Assistant with flat-zero sensors that look like real
	// readings (CONCEPT.md §2.2).
	if p.PoE.PowerW <= 0 {
		return nil
	}
	return c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortPoEPower), roundOne(p.PoE.PowerW))
}

// publishLocate publishes the locate LED's read-back.
//
// It is written exactly where the locate switch is announced, and
// nowhere else: the value comes from the classic API, so without the
// classic layer model.Device.Locating is simply false and publishing it
// would be inventing a reading. The same condition keeps the topic out
// of an installation that has the capability but has not enabled the
// control, where it would be a state topic no entity reads.
//
// It belongs to the static loop rather than the device loop because
// that is the loop that reads the value — the device list the fast loop
// polls has no locate field at all. A press nudges the static loop for
// the same reason.
func (c *Coordinator) publishLocate(ctx context.Context, d *model.Device) error {
	if !c.controlOptions().DeviceLocate {
		return nil
	}
	return c.pub.publish(ctx, c.topics.device(d.MAC, keyLocate), d.Locating)
}

// publishRadio publishes one radio's channel.
func (c *Coordinator) publishRadio(ctx context.Context, mac model.MAC, r *model.Radio) error {
	return c.pub.publish(ctx, c.topics.radio(mac, r.FrequencyGHz, keyRadioChannel), r.Channel)
}

// publishWLANs publishes the SSID catalogue and, when the toggle is
// enabled, announces a switch per SSID.
func (c *Coordinator) publishWLANs(ctx context.Context, wlans []model.WLAN) error {
	announce := c.hass != nil && c.controlOptions().WLANEnable

	for i := range wlans {
		w := &wlans[i]
		if err := c.pub.publish(ctx, c.topics.wlan(w.ID, keyWLANEnabled), w.Enabled); err != nil {
			return err
		}
		if err := c.pub.publish(ctx, c.topics.wlan(w.ID, keyWLANName), w.Name); err != nil {
			return err
		}
		if !announce {
			continue
		}
		entry, err := c.hass.WLANControl(w)
		if err != nil {
			return err
		}
		if err := c.pub.publishConfig(ctx, entry.ConfigTopic, entry.Payload); err != nil {
			return err
		}
	}
	return nil
}

// roundPct rounds a percentage to one decimal.
//
// Fixed precision matters for change detection: the console reports
// values like 12.500000001, and the gate compares the rendered `val`,
// so an unrounded reading would look different on every poll and
// republish forever.
func roundPct(v float64) float64 { return roundOne(v) }

// roundOne rounds to one decimal, the precision every fractional value
// of this bridge is published with.
func roundOne(v float64) float64 { return math.Round(v*10) / 10 }
