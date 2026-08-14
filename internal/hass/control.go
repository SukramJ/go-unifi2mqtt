// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"strconv"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Control entities — the write-back half of the integration.
//
// These are gated twice: on the operator having enabled the control,
// and on the capability actually being available. Announcing a switch
// the daemon cannot serve produces an entity that errors on click,
// which is worse than not offering it at all (CONCEPT.md §3.3).

// ControlOptions selects which control entities to announce.
type ControlOptions struct {
	DeviceRestart  bool
	PortPowerCycle bool
	DeviceLocate   bool
	ClientBlock    bool
	GuestAuthorize bool
	WLANEnable     bool
}

// Any reports whether any control is enabled.
func (o ControlOptions) Any() bool {
	return o.DeviceRestart || o.PortPowerCycle || o.DeviceLocate ||
		o.ClientBlock || o.GuestAuthorize || o.WLANEnable
}

// controlEntity extends the shared shape with the command-side fields.
type controlEntity struct {
	entity
	CommandTopic string `json:"command_topic"`
	PayloadPress string `json:"payload_press,omitempty"`
	StateOn      string `json:"state_on,omitempty"`
	StateOff     string `json:"state_off,omitempty"`
	// Optimistic is deliberately false everywhere: the state comes back
	// from the console after the follow-up poll, so a failed command
	// snaps the entity back instead of lying (CONCEPT.md §9).
	Optimistic bool `json:"optimistic"`
}

// DeviceControls returns the control entities for one device.
func (d *Discovery) DeviceControls(dev *model.Device, opts ControlOptions) ([]Entry, error) {
	if dev.MAC.IsZero() {
		return nil, nil
	}
	info := d.deviceInfo(dev)
	entries := make([]Entry, 0, 2+len(dev.Ports))

	if opts.DeviceRestart {
		e, err := d.button(dev.MAC, info, "restart", "device_restart",
			d.topics.DeviceTopic(dev.MAC, "cmd/restart"), "mdi:restart", "config")
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	if opts.DeviceLocate {
		e, err := d.deviceSwitch(dev.MAC, info, "locate", "device_locate",
			d.topics.DeviceTopic(dev.MAC, "locate"),
			d.topics.DeviceTopic(dev.MAC, "cmd/locate/set"), "mdi:map-marker-radius")
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	if opts.PortPowerCycle {
		for i := range dev.Ports {
			p := &dev.Ports[i]
			// A port with no PoE has nothing to power-cycle.
			if p.PoE == nil {
				continue
			}
			idx := strconv.Itoa(p.Idx)
			e, err := d.button(dev.MAC, info, "port_"+idx+"_power_cycle", "port_power_cycle",
				d.topics.DeviceTopic(dev.MAC, "port/"+idx+"/cmd/power_cycle"),
				"mdi:power-cycle", "config")
			if err != nil {
				return nil, err
			}
			// The port number belongs in the name, not just the key.
			if err := renameWith(&e, nameWith("port_power_cycle", d.lang, idx)); err != nil {
				return nil, err
			}
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// ClientControls returns the control entities for one client.
func (d *Discovery) ClientControls(cl *model.Client, opts ControlOptions) ([]Entry, error) {
	key := cl.Key()
	if key == "" {
		return nil, nil
	}
	info := d.clientDeviceInfo(cl)
	entries := make([]Entry, 0, 2)

	// Blocking is keyed by MAC on the wire, so a client without one
	// (VPN, Teleport) cannot be blocked.
	if opts.ClientBlock && !cl.MAC.IsZero() {
		uid := idPrefix + "_client_" + key + "_blocked"
		seed := entityIDSeed(info.Name, "blocked")
		e := controlEntity{
			entity: entity{
				Name:            name("client_blocked", d.lang),
				UniqueID:        uid,
				ObjectID:        seed,
				DefaultEntityID: string(PlatformSwitch) + "." + seed,
				StateTopic:      d.topics.ClientTopic(key, "blocked"),
				Icon:            "mdi:cancel",
				Availability: []availabilityEntry{
					{Topic: d.topics.AvailabilityTopic()},
				},
				AvailabilityMode: "all",
				Device:           info,
			},
			CommandTopic: d.topics.ClientTopic(key, "blocked/set"),
			StateOn:      payloadON,
			StateOff:     payloadOFF,
		}
		payload, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{
			ConfigTopic: d.clientConfigTopic(PlatformSwitch, key, "blocked"),
			Payload:     payload,
		})
	}

	if opts.GuestAuthorize && cl.IsGuest {
		uid := idPrefix + "_client_" + key + "_authorize"
		seed := entityIDSeed(info.Name, "authorize")
		e := controlEntity{
			entity: entity{
				Name:            name("client_authorize", d.lang),
				UniqueID:        uid,
				ObjectID:        seed,
				DefaultEntityID: string(PlatformButton) + "." + seed,
				// A button has no state topic; Home Assistant only needs
				// somewhere to publish.
				StateTopic: "",
				Icon:       "mdi:account-check",
				Availability: []availabilityEntry{
					{Topic: d.topics.AvailabilityTopic()},
				},
				AvailabilityMode: "all",
				Device:           info,
			},
			CommandTopic: d.topics.ClientTopic(key, "cmd/authorize"),
			PayloadPress: "PRESS",
		}
		payload, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{
			ConfigTopic: d.clientConfigTopic(PlatformButton, key, "authorize"),
			Payload:     payload,
		})
	}
	return entries, nil
}

// WLANControl returns the enable/disable switch for one SSID.
func (d *Discovery) WLANControl(w *model.WLAN) (Entry, error) {
	uid := idPrefix + "_wlan_" + w.ID + "_enabled"
	seed := entityIDSeed("unifi_wlan", w.Name)
	info := deviceInfo{
		Identifiers:  []string{siteDeviceID(d.site)},
		Name:         "UniFi Site " + d.site,
		Manufacturer: Manufacturer,
		Model:        "Site",
	}

	e := controlEntity{
		entity: entity{
			// The SSID is the useful label here; the generic "Enabled"
			// would produce a page full of identical names.
			Name:            w.Name,
			UniqueID:        uid,
			ObjectID:        seed,
			DefaultEntityID: string(PlatformSwitch) + "." + seed,
			StateTopic:      d.topics.WLANTopic(w.ID, "enabled"),
			Icon:            "mdi:wifi",
			Availability: []availabilityEntry{
				{Topic: d.topics.AvailabilityTopic()},
			},
			AvailabilityMode: "all",
			Device:           info,
		},
		CommandTopic: d.topics.WLANTopic(w.ID, "enabled/set"),
		StateOn:      payloadON,
		StateOff:     payloadOFF,
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.baseTopic + "/" + string(PlatformSwitch) + "/" +
			siteDeviceID(d.site) + "/wlan_" + w.ID + "/config",
		Payload: payload,
	}, nil
}

// Binary payloads, matching what the coordinator publishes.
const (
	payloadON  = "ON"
	payloadOFF = "OFF"
)

func (d *Discovery) button(
	mac model.MAC, info deviceInfo, key, nameKey, commandTopic, icon, category string,
) (Entry, error) {
	uid := idPrefix + "_" + mac.String() + "_" + key
	seed := entityIDSeed(info.Name, key)
	e := controlEntity{
		entity: entity{
			Name:            name(nameKey, d.lang),
			UniqueID:        uid,
			ObjectID:        seed,
			DefaultEntityID: string(PlatformButton) + "." + seed,
			Icon:            icon,
			EntityCategory:  category,
			Availability: []availabilityEntry{
				{Topic: d.topics.AvailabilityTopic()},
				{
					Topic:         d.topics.DeviceTopic(mac, "state"),
					ValueTemplate: "{{ 'online' if value == 'ONLINE' else 'offline' }}",
				},
			},
			AvailabilityMode: "all",
			Device:           info,
		},
		CommandTopic: commandTopic,
		PayloadPress: "PRESS",
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.configTopic(PlatformButton, mac, key),
		Payload:     payload,
	}, nil
}

func (d *Discovery) deviceSwitch(
	mac model.MAC, info deviceInfo, key, nameKey, stateTopic, commandTopic, icon string,
) (Entry, error) {
	uid := idPrefix + "_" + mac.String() + "_" + key
	seed := entityIDSeed(info.Name, key)
	e := controlEntity{
		entity: entity{
			Name:            name(nameKey, d.lang),
			UniqueID:        uid,
			ObjectID:        seed,
			DefaultEntityID: string(PlatformButton) + "." + seed,
			StateTopic:      stateTopic,
			Icon:            icon,
			EntityCategory:  "config",
			Availability: []availabilityEntry{
				{Topic: d.topics.AvailabilityTopic()},
				{
					Topic:         d.topics.DeviceTopic(mac, "state"),
					ValueTemplate: "{{ 'online' if value == 'ONLINE' else 'offline' }}",
				},
			},
			AvailabilityMode: "all",
			Device:           info,
		},
		CommandTopic: commandTopic,
		StateOn:      payloadON,
		StateOff:     payloadOFF,
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		ConfigTopic: d.configTopic(PlatformSwitch, mac, key),
		Payload:     payload,
	}, nil
}

// renameWith rewrites an already-marshalled entry's display name.
// Cheaper than threading a name override through every constructor for
// the one case that needs it.
func renameWith(e *Entry, newName string) error {
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return err
	}
	payload["name"] = newName
	out, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	e.Payload = out
	return nil
}
