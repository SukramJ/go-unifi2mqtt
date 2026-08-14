// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package integration

// Wire types for the UniFi Network Integration API v1.
//
// Every field below is transcribed from the official OpenAPI document
// (`GET /proxy/network/integration/openapi/document.json`), cross-checked
// against the archived specs for Network 10.5.67 and 10.6.90, which are
// identical for all of these schemas (CONCEPT.md §2.5).
//
// Two deliberate decisions:
//
//   - Everything is decoded permissively. Fields Ubiquiti adds are
//     ignored (no DisallowUnknownFields) and fields it removes decode to
//     the zero value. A schema change must degrade a single sensor, never
//     fail a whole poll.
//   - Numbers that the spec marks required are still plain types rather
//     than pointers. The one place presence genuinely matters —
//     distinguishing "0% CPU" from "no statistics yet" — is handled by
//     the statistics response carrying no `uptimeSec` at all, which the
//     converter checks explicitly.

// page is the envelope every list endpoint returns. The four counters
// are what drive [Client.paginate]: `totalCount` is the full result set
// while `count` is how many landed in this page.
type page[T any] struct {
	Offset     int64 `json:"offset"`
	Limit      int32 `json:"limit"`
	Count      int32 `json:"count"`
	TotalCount int64 `json:"totalCount"`
	Data       []T   `json:"data"`
}

// applicationInfo is `GET /v1/info`.
type applicationInfo struct {
	ApplicationVersion string `json:"applicationVersion"`
}

// site is one entry of `GET /v1/sites`.
type site struct {
	ID string `json:"id"`
	// InternalReference is the identifier the classic API uses for the
	// same site, e.g. "default".
	InternalReference string `json:"internalReference"`
	Name              string `json:"name"`
}

// deviceOverview is one entry of `GET /v1/sites/{siteId}/devices`.
//
// Note that firmwareVersion and firmwareUpdatable are already here, so
// the update sensor needs no per-device detail call (CONCEPT.md §8.2).
type deviceOverview struct {
	ID                string   `json:"id"`
	MACAddress        string   `json:"macAddress"`
	IPAddress         string   `json:"ipAddress"`
	Name              string   `json:"name"`
	Model             string   `json:"model"`
	State             string   `json:"state"`
	Supported         bool     `json:"supported"`
	FirmwareVersion   string   `json:"firmwareVersion"`
	FirmwareUpdatable bool     `json:"firmwareUpdatable"`
	Features          []string `json:"features"`
	Interfaces        []string `json:"interfaces"`
}

// deviceDetails is `GET /v1/sites/{siteId}/devices/{deviceId}`.
//
// It repeats the overview fields and adds the parts that need a
// per-device call: ports, radios and the uplink. `features` widens from
// a string list to an object here — the same key carrying a different
// shape in the two responses, which is why they are separate types.
type deviceDetails struct {
	ID                string `json:"id"`
	MACAddress        string `json:"macAddress"`
	IPAddress         string `json:"ipAddress"`
	Name              string `json:"name"`
	Model             string `json:"model"`
	State             string `json:"state"`
	Supported         bool   `json:"supported"`
	FirmwareVersion   string `json:"firmwareVersion"`
	FirmwareUpdatable bool   `json:"firmwareUpdatable"`
	AdoptedAt         string `json:"adoptedAt"`
	ProvisionedAt     string `json:"provisionedAt"`
	ConfigurationID   string `json:"configurationId"`

	Uplink *struct {
		// DeviceID is a UUID, not a MAC — resolving it is the
		// coordinator's job (CONCEPT.md §3.4).
		DeviceID string `json:"deviceId"`
	} `json:"uplink"`

	Features struct {
		Switching   *struct{} `json:"switching"`
		AccessPoint *struct{} `json:"accessPoint"`
	} `json:"features"`

	Interfaces struct {
		Ports  []port  `json:"ports"`
		Radios []radio `json:"radios"`
	} `json:"interfaces"`
}

type port struct {
	Idx          int    `json:"idx"`
	State        string `json:"state"`
	Connector    string `json:"connector"`
	MaxSpeedMbps int    `json:"maxSpeedMbps"`
	SpeedMbps    int    `json:"speedMbps"`
	// PoE is absent on ports that cannot deliver power, which is why it
	// is a pointer: "no PoE" and "PoE off" are different facts.
	PoE *struct {
		Standard string `json:"standard"`
		Type     int    `json:"type"`
		Enabled  bool   `json:"enabled"`
		State    string `json:"state"`
	} `json:"poe"`
}

type radio struct {
	WLANStandard    string  `json:"wlanStandard"`
	FrequencyGHz    float64 `json:"frequencyGHz"`
	ChannelWidthMHz int     `json:"channelWidthMHz"`
	Channel         int     `json:"channel"`
}

// deviceStatistics is
// `GET /v1/sites/{siteId}/devices/{deviceId}/statistics/latest`.
//
// Only `interfaces` is required by the spec; everything else is absent
// while a device is still coming up.
type deviceStatistics struct {
	UptimeSec            *int64   `json:"uptimeSec"`
	LastHeartbeatAt      string   `json:"lastHeartbeatAt"`
	NextHeartbeatAt      string   `json:"nextHeartbeatAt"`
	LoadAverage1Min      *float64 `json:"loadAverage1Min"`
	LoadAverage5Min      *float64 `json:"loadAverage5Min"`
	LoadAverage15Min     *float64 `json:"loadAverage15Min"`
	CPUUtilizationPct    *float64 `json:"cpuUtilizationPct"`
	MemoryUtilizationPct *float64 `json:"memoryUtilizationPct"`

	Uplink *struct {
		TxRateBps uint64 `json:"txRateBps"`
		RxRateBps uint64 `json:"rxRateBps"`
	} `json:"uplink"`

	Interfaces struct {
		Radios []struct {
			FrequencyGHz float64  `json:"frequencyGHz"`
			TxRetriesPct *float64 `json:"txRetriesPct"`
		} `json:"radios"`
	} `json:"interfaces"`
}

// clientOverview is one entry of `GET /v1/sites/{siteId}/clients`.
//
// The API models clients polymorphically with `type` as the
// discriminator, but every variant only *adds* optional fields to the
// same base, so one permissive struct decodes all four. `macAddress`
// and `uplinkDeviceId` are simply absent for VPN and Teleport clients.
//
// This is the whole payload: no SSID, no VLAN, no signal strength and
// no hostname. Those need the classic layer (CONCEPT.md §2.2).
type clientOverview struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	ConnectedAt    string `json:"connectedAt"`
	IPAddress      string `json:"ipAddress"`
	MACAddress     string `json:"macAddress"`
	UplinkDeviceID string `json:"uplinkDeviceId"`

	Access struct {
		Type string `json:"type"`
		// Authorized is only present on guest access.
		Authorized *bool `json:"authorized"`
	} `json:"access"`
}

// networkOverview is one entry of `GET /v1/sites/{siteId}/networks`.
//
// Deliberately carries no IP configuration: the list endpoint returns
// only management, id, name, enabled, vlanId, metadata, zoneId and
// default. The subnets live in [networkDetails] and need a per-network
// call — verified against a live 10.5.67 console, since the OpenAPI
// schema alone makes this easy to get wrong (the details schema
// inherits from the overview one, so both look plausible).
type networkOverview struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	VLANID     int    `json:"vlanId"`
	Default    bool   `json:"default"`
	Management string `json:"management"`
}

// networkDetails is `GET /v1/sites/{siteId}/networks/{networkId}`.
//
// `ipv4Configuration` only appears on GATEWAY-managed networks and is
// what makes client→VLAN mapping possible without the classic API — so
// this call is the price of that capability.
type networkDetails struct {
	networkOverview

	IPv4Configuration *struct {
		HostIPAddress string `json:"hostIpAddress"`
		PrefixLength  int    `json:"prefixLength"`
		// AdditionalHostIPSubnets are extra CIDRs bound to the same
		// VLAN, e.g. "10.0.5.1/24".
		AdditionalHostIPSubnets []string `json:"additionalHostIpSubnets"`
	} `json:"ipv4Configuration"`
}

// wifiBroadcastOverview is one entry of
// `GET /v1/sites/{siteId}/wifi/broadcasts` — the SSID catalogue.
type wifiBroadcastOverview struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	// Network is a reference discriminated by its own `type`: NATIVE
	// (the default network) carries no id, SPECIFIC names one.
	Network *struct {
		Type      string `json:"type"`
		NetworkID string `json:"networkId"`
	} `json:"network"`
}

// deviceActionRequest is the body of
// `POST /v1/sites/{siteId}/devices/{deviceId}/actions`.
type deviceActionRequest struct {
	Action string `json:"action"`
}

// portActionRequest is the body of
// `POST /…/devices/{deviceId}/interfaces/ports/{portIdx}/actions`.
type portActionRequest struct {
	Action string `json:"action"`
}

// clientActionRequest is the body of
// `POST /v1/sites/{siteId}/clients/{clientId}/actions`.
type clientActionRequest struct {
	Action string `json:"action"`
	// TimeLimitMinutes is optional and only meaningful for guest
	// authorization.
	TimeLimitMinutes *int `json:"timeLimitMinutes,omitempty"`
}
