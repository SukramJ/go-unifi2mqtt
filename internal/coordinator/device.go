// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Value publication.
//
// Every entity-shaped value gets its own scalar topic rather than being
// packed into one JSON blob: Home Assistant then needs a plain
// state_topic per sensor instead of a value_template maze
// (CONCEPT.md §5.6). What is useful to see but pointless as its own
// entity goes into the accompanying `attributes` object.
//
// A publish failure on one value is returned to the caller, which logs
// it and moves on — losing one sensor for one cycle is better than
// aborting the whole poll.

// publishDevice publishes one device's state, statistics-independent
// values, ports and radios.
func (c *Coordinator) publishDevice(ctx context.Context, d *model.Device) error {
	values := []struct {
		key     string
		payload string
	}{
		{keyState, string(d.State)},
		{keyFirmware, d.Firmware},
		{keyUpdateAvailable, boolPayload(d.UpdateAvail)},
	}
	for _, v := range values {
		if err := c.pub.publish(ctx, c.topics.device(d.MAC, v.key), v.payload); err != nil {
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

	payload, err := json.Marshal(attrs)
	if err != nil {
		return err
	}
	return c.pub.publish(ctx, c.topics.device(d.MAC, keyAttributes), string(payload))
}

// publishDeviceStats publishes one statistics sample.
//
// Uptime is published in seconds because that is what Home Assistant's
// duration device_class expects, and radio retry rates are keyed by
// band since the API gives radios no other identifier.
func (c *Coordinator) publishDeviceStats(ctx context.Context, mac model.MAC, s *model.DeviceStats) error {
	values := []struct {
		key     string
		payload string
	}{
		{keyUptime, strconv.FormatInt(int64(s.Uptime.Seconds()), 10)},
		{keyCPUUtilization, formatPct(s.CPUPct)},
		{keyMemoryUtilization, formatPct(s.MemoryPct)},
		{keyUplinkTxBps, strconv.FormatUint(s.UplinkTxBps, 10)},
		{keyUplinkRxBps, strconv.FormatUint(s.UplinkRxBps, 10)},
	}
	for _, v := range values {
		if err := c.pub.publish(ctx, c.topics.device(mac, v.key), v.payload); err != nil {
			return err
		}
	}

	for freq, pct := range s.RadioTxRetry {
		topic := c.topics.radio(mac, freq, keyRadioTxRetries)
		if err := c.pub.publish(ctx, topic, formatPct(pct)); err != nil {
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
	if err := c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortSpeed), itoa(p.SpeedMbps)); err != nil {
		return err
	}
	// A port without PoE hardware gets no PoE topic at all — publishing
	// OFF there would create a Home Assistant entity for a capability
	// the port does not have.
	if p.PoE == nil {
		return nil
	}
	if err := c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortPoE), boolPayload(p.PoE.Enabled)); err != nil {
		return err
	}

	// Wattage exists only with the classic layer, and only for ports
	// actually delivering power. Publishing 0 W for the rest would fill
	// Home Assistant with flat-zero sensors that look like real
	// readings (CONCEPT.md §2.2).
	if p.PoE.PowerW <= 0 {
		return nil
	}
	return c.pub.publish(ctx, c.topics.port(mac, p.Idx, keyPortPoEPower),
		strconv.FormatFloat(p.PoE.PowerW, 'f', 1, 64))
}

// publishRadio publishes one radio's channel.
func (c *Coordinator) publishRadio(ctx context.Context, mac model.MAC, r *model.Radio) error {
	return c.pub.publish(ctx, c.topics.radio(mac, r.FrequencyGHz, keyRadioChannel), itoa(r.Channel))
}

// publishWLANs publishes the SSID catalogue.
func (c *Coordinator) publishWLANs(ctx context.Context, wlans []model.WLAN) error {
	for i := range wlans {
		w := &wlans[i]
		if err := c.pub.publish(ctx, c.topics.wlan(w.ID, keyWLANEnabled), boolPayload(w.Enabled)); err != nil {
			return err
		}
		if err := c.pub.publish(ctx, c.topics.wlan(w.ID, keyWLANName), w.Name); err != nil {
			return err
		}
	}
	return nil
}

// formatPct renders a percentage with one decimal.
//
// Fixed precision matters for change detection: the console reports
// values like 12.500000001, and formatting with %v would make an
// identical reading look different on every poll and republish forever.
func formatPct(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}
