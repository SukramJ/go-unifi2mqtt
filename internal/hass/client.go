// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Client discovery.
//
// A network client becomes its own Home Assistant device — a phone, a
// NAS, a TV — carrying a device_tracker plus whatever metadata the API
// surface provides. via_device points at the access point or switch it
// is connected through, which is what continues the topology started by
// the infrastructure devices: gateway → switch → AP → phone.

// clientEntity extends the shared entity shape with the fields only
// device_tracker uses.
type clientEntity struct {
	entity
	SourceType     string `json:"source_type,omitempty"`
	PayloadHome    string `json:"payload_home,omitempty"`
	PayloadNotHome string `json:"payload_not_home,omitempty"`
}

// Client returns the discovery entries for one network client.
//
// Only entities backed by data the Integration API actually returns are
// created. Signal strength and the blocked switch need the classic
// layer; announcing them regardless would produce entities that stay
// unavailable forever (CONCEPT.md §2.2).
func (d *Discovery) Client(cl *model.Client, opts ClientOptions) ([]Entry, error) {
	key := cl.Key()
	if key == "" {
		return nil, nil
	}

	info := d.clientDeviceInfo(cl)
	tracker, err := d.renderTracker(key, info)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, 3)
	entries = append(entries, tracker)

	// The IP sensor is diagnostic: useful on the device page, noise in
	// the main dashboard.
	ip, err := d.renderClientSensor(key, "ip", info, spec{
		platform: PlatformSensor, key: "client_ip", nameKey: "client_ip",
		stateSuffix: "ip", category: "diagnostic", icon: "mdi:ip-network",
	})
	if err != nil {
		return nil, err
	}
	entries = append(entries, ip)

	// Signal strength needs the classic layer and only means anything
	// for a wireless client. Announcing it otherwise would create an
	// entity whose topic never receives a value.
	if opts.Signal && cl.Type == model.ClientWireless {
		signal, err := d.renderClientSensor(key, "signal", info, spec{
			platform: PlatformSensor, key: "client_signal", nameKey: "client_signal",
			stateSuffix: "signal", unit: "dBm", deviceClass: "signal_strength",
			stateClass: "measurement", category: "diagnostic",
		})
		if err != nil {
			return nil, err
		}
		entries = append(entries, signal)
	}

	return entries, nil
}

// ClientOptions selects the optional per-client entities, which depend
// on capabilities the classic layer provides.
type ClientOptions struct {
	// Signal adds a signal-strength sensor for wireless clients.
	Signal bool
}

func (d *Discovery) renderTracker(key string, info deviceInfo) (Entry, error) {
	uid := idPrefix + "_client_" + key + "_presence"
	e := clientEntity{
		entity: entity{
			Name:     name("client_presence", d.lang),
			UniqueID: uid,
			ObjectID: uid,
			// The tracker is the entity that reports being away, so its
			// availability must not depend on the client being present —
			// only on the bridge running.
			StateTopic:          d.topics.ClientTopic(key, "state"),
			JSONAttributesTopic: d.topics.ClientTopic(key, "attributes"),
			Availability: []availabilityEntry{
				{Topic: d.topics.AvailabilityTopic()},
			},
			AvailabilityMode: "all",
			Device:           info,
		},
		// "router" tells Home Assistant this is network-presence rather
		// than GPS, which is what makes it usable for the person
		// integration without a location.
		SourceType:     "router",
		PayloadHome:    "home",
		PayloadNotHome: "not_home",
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.clientConfigTopic(PlatformDeviceTracker, key, "presence"),
		Payload:     payload,
	}, nil
}

func (d *Discovery) renderClientSensor(key, suffix string, info deviceInfo, s spec) (Entry, error) {
	uid := idPrefix + "_client_" + key + "_" + s.key
	e := entity{
		Name:                name(s.nameKey, d.lang),
		UniqueID:            uid,
		ObjectID:            uid,
		StateTopic:          d.topics.ClientTopic(key, s.stateSuffix),
		UnitOfMeasurement:   s.unit,
		DeviceClass:         s.deviceClass,
		StateClass:          s.stateClass,
		EntityCategory:      s.category,
		Icon:                s.icon,
		JSONAttributesTopic: d.topics.ClientTopic(key, "attributes"),
		Availability: []availabilityEntry{
			{Topic: d.topics.AvailabilityTopic()},
			// A client's values are stale once it is away, so these go
			// unavailable with it — unlike the tracker itself.
			{
				Topic:         d.topics.ClientTopic(key, "state"),
				ValueTemplate: "{{ 'online' if value == 'home' else 'offline' }}",
			},
		},
		AvailabilityMode: "all",
		Device:           info,
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.clientConfigTopic(s.platform, key, suffix),
		Payload:     payload,
	}, nil
}

func (d *Discovery) clientDeviceInfo(cl *model.Client) deviceInfo {
	info := deviceInfo{
		Identifiers:  []string{clientDeviceID(cl.Key())},
		Name:         cl.Name,
		Manufacturer: "", // unknown for a network client
	}
	if !cl.MAC.IsZero() {
		info.Connections = [][]string{{"mac", cl.MAC.Colon()}}
	}
	if info.Name == "" {
		info.Name = "UniFi client " + cl.Key()
	}
	// Continue the topology: the client hangs off the AP or switch that
	// reported it. Without an uplink (VPN, Teleport) it stands alone.
	if !cl.UplinkMAC.IsZero() {
		info.ViaDevice = deviceID(cl.UplinkMAC)
	}
	return info
}

// clientDeviceID namespaces client devices separately from
// infrastructure ones, so a UniFi device that also appears as a client
// cannot collide with itself in the device registry.
func clientDeviceID(key string) string { return idPrefix + "_client_" + key }

func (d *Discovery) clientConfigTopic(p Platform, key, suffix string) string {
	return d.baseTopic + "/" + string(p) + "/" + clientDeviceID(key) + "/" + suffix + "/config"
}
