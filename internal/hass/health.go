// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import "encoding/json"

// Site health discovery.
//
// Health entities belong to a synthetic "site" device rather than to
// any physical one: WAN latency is a property of the site, and hanging
// it off the gateway would make it disappear when the gateway is
// swapped.

// healthSpecs lists the site-level entities.
func healthSpecs() []spec {
	return []spec{
		{
			platform: PlatformBinarySensor, key: "wan_connectivity",
			nameKey: "wan_connectivity", stateSuffix: "wan/state",
			deviceClass: "connectivity", payloadOn: "ok",
		},
		{
			platform: PlatformSensor, key: "wan_ip", nameKey: "wan_ip",
			stateSuffix: "wan/ip", category: "diagnostic", icon: "mdi:ip-network-outline",
		},
		{
			platform: PlatformSensor, key: "wan_latency", nameKey: "wan_latency",
			stateSuffix: "wan/latency_ms", unit: "ms", stateClass: "measurement",
			category: "diagnostic", icon: "mdi:timer-outline",
		},
		{
			platform: PlatformSensor, key: "wan_rx_bps", nameKey: "wan_rx_bps",
			stateSuffix: "wan/rx_bps", unit: "bit/s", deviceClass: "data_rate",
			stateClass: "measurement",
		},
		{
			platform: PlatformSensor, key: "wan_tx_bps", nameKey: "wan_tx_bps",
			stateSuffix: "wan/tx_bps", unit: "bit/s", deviceClass: "data_rate",
			stateClass: "measurement",
		},
		{
			platform: PlatformSensor, key: "clients_total", nameKey: "clients_total",
			stateSuffix: "clients/total", stateClass: "measurement", icon: "mdi:account-multiple",
		},
		{
			platform: PlatformSensor, key: "clients_guest", nameKey: "clients_guest",
			stateSuffix: "clients/guest", stateClass: "measurement",
			category: "diagnostic", icon: "mdi:account-question",
		},
	}
}

// Health returns the discovery entries for the site health aggregate.
//
// Only called when the classic layer is available: without it these
// topics never receive a value, and the entities would sit unavailable
// forever (CONCEPT.md §3.3).
func (d *Discovery) Health(siteName string) ([]Entry, error) {
	info := deviceInfo{
		Identifiers:  []string{siteDeviceID(d.site)},
		Name:         "UniFi Site " + siteName,
		Manufacturer: Manufacturer,
		Model:        "Site",
	}

	specs := healthSpecs()
	entries := make([]Entry, 0, len(specs))
	for i := range specs {
		s := &specs[i]
		uid := idPrefix + "_site_" + d.site + "_" + s.key
		seed := entityIDSeed(info.Name, s.key)
		e := entity{
			Name:                name(s.nameKey, d.lang),
			UniqueID:            uid,
			ObjectID:            seed,
			DefaultEntityID:     string(s.platform) + "." + seed,
			StateTopic:          d.topics.HealthTopic(s.stateSuffix),
			UnitOfMeasurement:   s.unit,
			DeviceClass:         s.deviceClass,
			StateClass:          s.stateClass,
			EntityCategory:      s.category,
			Icon:                s.icon,
			PayloadOn:           s.payloadOn,
			PayloadOff:          s.payloadOff,
			JSONAttributesTopic: d.topics.HealthTopic("attributes"),
			Availability: []availabilityEntry{
				{Topic: d.topics.AvailabilityTopic()},
			},
			AvailabilityMode: "all",
			Device:           info,
		}

		payload, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{
			ConfigTopic: d.baseTopic + "/" + string(s.platform) + "/" +
				siteDeviceID(d.site) + "/" + s.key + "/config",
			Payload: payload,
		})
	}
	return entries, nil
}

func siteDeviceID(site string) string { return idPrefix + "_site_" + site }
