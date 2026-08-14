// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Package model holds the API-neutral domain types the rest of the
// daemon works with.
//
// The two UniFi API flavours (official Integration API and the classic
// controller API) disagree about field names, identifiers and even
// which data they expose at all. Everything above internal/unifi sees
// only the types in this package — no raw API DTO ever escapes the
// client packages. That is what lets the coordinator, the Home
// Assistant discovery builder and the web UI stay unaware of which
// surface a value came from.
//
// Field comments name the Integration API source field, verified
// against the OpenAPI specification of Network application 10.5.67 /
// 10.6.90 (see CONCEPT.md §2.5). Fields marked "classic layer only"
// stay at their zero value unless CLASSIC_ENABLE is on.
package model

import (
	"net/netip"
	"time"
)

// Site is one UniFi site. A daemon instance bridges exactly one.
type Site struct {
	// ID is the Integration API UUID.
	ID string
	// Name is the display name, e.g. "Default".
	Name string
	// Internal is `internalReference`, e.g. "default" — the identifier
	// the classic API addresses the site by, which is why both are kept.
	Internal string
}

// DeviceType classifies an adopted device by the features the console
// reports for it. It drives which Home Assistant entities make sense.
type DeviceType string

// Device types.
const (
	DeviceGateway     DeviceType = "gateway"
	DeviceSwitch      DeviceType = "switch"
	DeviceAccessPoint DeviceType = "access_point"
	DeviceOther       DeviceType = "other"
)

// DeviceState mirrors the state enum of the Integration API, plus
// [DeviceUnknown] as a catch-all so a value Ubiquiti adds later flows
// through instead of failing the decode.
type DeviceState string

// Device states. The first ten are the spec's enum verbatim.
const (
	DeviceOnline                DeviceState = "ONLINE"
	DeviceOffline               DeviceState = "OFFLINE"
	DevicePendingAdoption       DeviceState = "PENDING_ADOPTION"
	DeviceUpdating              DeviceState = "UPDATING"
	DeviceGettingReady          DeviceState = "GETTING_READY"
	DeviceAdopting              DeviceState = "ADOPTING"
	DeviceDeleting              DeviceState = "DELETING"
	DeviceConnectionInterrupted DeviceState = "CONNECTION_INTERRUPTED"
	DeviceIsolated              DeviceState = "ISOLATED"
	DeviceIncorrectTopology     DeviceState = "U5G_INCORRECT_TOPOLOGY"
	DeviceUnknown               DeviceState = "UNKNOWN"
)

// IsOnline reports whether the device is in a state where its
// statistics are meaningful. The statistics poll skips everything else
// (CONCEPT.md §8.2).
func (s DeviceState) IsOnline() bool { return s == DeviceOnline }

// Device is an adopted UniFi device (gateway, switch or access point).
type Device struct {
	// MAC is the canonical identity — MQTT topic segment and unique_id
	// seed. From `macAddress`.
	MAC MAC
	// ID is the Integration API UUID, needed for actuator calls.
	ID string
	// ClassicID is the classic API's Mongo `_id`, empty without the
	// classic layer.
	ClassicID string

	Name  string
	Model string
	Type  DeviceType
	IP    netip.Addr
	State DeviceState

	// Supported reports `supported`. A false value means the console
	// manages the device only rudimentarily and most fields stay empty.
	Supported bool
	// Firmware is `firmwareVersion`.
	Firmware string
	// UpdateAvail is `firmwareUpdatable` — already present in the device
	// overview, so the update sensor needs no per-device detail call.
	UpdateAvail bool
	AdoptedAt   time.Time

	// UplinkID is `uplink.deviceId`: a UUID, NOT a MAC. The coordinator
	// resolves it to UplinkMAC using the device list (CONCEPT.md §3.4).
	UplinkID string
	// UplinkMAC is the resolved uplink device, empty on the gateway (and
	// whenever UplinkID names a device outside the polled list).
	UplinkMAC MAC

	// Features lists `features` keys the console reports, e.g.
	// "switching", "accessPoint".
	Features []string

	Ports  []Port
	Radios []Radio
}

// DeviceStats is one sample from `/devices/{id}/statistics/latest`.
type DeviceStats struct {
	Uptime        time.Duration // uptimeSec
	CPUPct        float64       // cpuUtilizationPct
	MemoryPct     float64       // memoryUtilizationPct
	LoadAvg1      float64       // loadAverage1Min
	UplinkTxBps   uint64        // uplink.txRateBps
	UplinkRxBps   uint64        // uplink.rxRateBps
	LastHeartbeat time.Time     // lastHeartbeatAt

	// RadioTxRetry maps frequencyGHz to txRetriesPct. Keyed by frequency
	// because that is the only radio identifier the statistics response
	// carries — it has no index and no MAC.
	RadioTxRetry map[float64]float64
}

// PortState is the link state of a physical port.
type PortState string

// Port states.
const (
	PortUp      PortState = "UP"
	PortDown    PortState = "DOWN"
	PortUnknown PortState = "UNKNOWN"
)

// Port is one physical interface of a device.
type Port struct {
	Idx int
	// Connector is RJ45 | SFP | SFPPLUS | SFP28 | QSFP28.
	Connector    string
	State        PortState
	SpeedMbps    int
	MaxSpeedMbps int
	// PoE is nil when the port cannot deliver power.
	PoE *PoEState
}

// PoEState is the power-over-Ethernet state of a port.
type PoEState struct {
	Enabled bool
	// Standard is "802.3af" | "802.3at" | "802.3bt".
	Standard string
	// Type is the PoE class, 1..4.
	Type int
	// State is UP | DOWN | LIMITED | UNKNOWN.
	State string
	// PowerW is the actual draw in watts. Classic layer ONLY — the
	// Integration API has no power field at all (CONCEPT.md §2.2).
	PowerW float64
}

// Radio is one wireless radio of an access point.
type Radio struct {
	// FrequencyGHz is 2.4 | 5 | 6 | 60 and doubles as the radio's
	// identity within a device.
	FrequencyGHz float64
	Channel      int
	// ChannelWidth is `channelWidthMHz`.
	ChannelWidth int
	// Standard is 802.11a/b/g/n/ac/ax/be.
	Standard string
	// TxRetriesPct comes from the statistics endpoint, not from the
	// device itself, and is merged in by the facade.
	TxRetriesPct float64
}

// ClientType is the connection kind, mirroring the API discriminator.
type ClientType string

// Client types.
const (
	ClientWired    ClientType = "WIRED"
	ClientWireless ClientType = "WIRELESS"
	ClientVPN      ClientType = "VPN"
	ClientTeleport ClientType = "TELEPORT"
	ClientUnknown  ClientType = "UNKNOWN"
)

// Client is a device connected to the network.
//
// The Integration API is deliberately sparse here: it returns only
// type, id, name, connectedAt, ipAddress, macAddress, uplinkDeviceId
// and access. Everything from Hostname downwards needs the classic
// layer; Network and VLAN are derived locally from the network
// catalogue (CONCEPT.md §2.2).
type Client struct {
	// MAC is empty for VPN and Teleport clients — they have no hardware
	// address, so those are keyed by ID instead.
	MAC       MAC
	ID        string
	ClassicID string

	Name string
	IP   netip.Addr
	Type ClientType

	// UplinkID is `uplinkDeviceId` (UUID); empty for VPN/Teleport.
	UplinkID string
	// UplinkMAC is the resolved AP or switch the client hangs off.
	UplinkMAC MAC

	// IsGuest is access.type == "GUEST"; Authorized is access.authorized
	// and only meaningful for guests.
	IsGuest    bool
	Authorized bool

	ConnectedAt time.Time

	// Network and VLAN are derived by matching IP against the subnets in
	// the network catalogue; VLAN 0 means untagged or unmappable.
	Network string
	VLAN    int

	// Classic layer only:
	Hostname  string
	SSID      string
	SignalDBm int
	LastSeen  time.Time
	Blocked   bool
}

// Key returns the stable identity used for MQTT topics and unique_ids:
// the MAC where there is one, the API UUID otherwise (VPN/Teleport).
func (c Client) Key() string {
	if !c.MAC.IsZero() {
		return c.MAC.String()
	}
	return c.ID
}

// NetworkManagement says which device type owns a network.
type NetworkManagement string

// Network management kinds.
const (
	NetworkUnmanaged NetworkManagement = "UNMANAGED"
	NetworkGateway   NetworkManagement = "GATEWAY"
	NetworkSwitch    NetworkManagement = "SWITCH"
)

// Network is one entry of the site's network/VLAN catalogue. Its
// Subnets are what turn a client's IP into a VLAN (CONCEPT.md §2.2).
type Network struct {
	ID      string
	Name    string
	VLAN    int
	Enabled bool
	// Default marks the site's primary network.
	Default    bool
	Management NetworkManagement
	// Subnets is empty for UNMANAGED networks, which carry no IP
	// configuration the console knows about.
	Subnets []netip.Prefix
}

// WLAN is one entry of the site's SSID catalogue.
type WLAN struct {
	ID   string
	Name string // the SSID
	// Enabled reflects whether the SSID is currently broadcast.
	Enabled bool
	// NetworkID references the [Network] this SSID bridges to; empty
	// when the WLAN uses the native/default network.
	NetworkID string
}

// SubsystemHealth is the status of one health subsystem.
type SubsystemHealth struct {
	// Status is "ok" | "warning" | "error" | "unknown".
	Status string
}

// Health is the site-wide health aggregate. Classic layer only: the
// Integration API's /wans endpoint carries just id and name
// (CONCEPT.md §2.2).
type Health struct {
	WAN  SubsystemHealth
	LAN  SubsystemHealth
	WLAN SubsystemHealth
	VPN  SubsystemHealth

	WANIP netip.Addr
	// LatencyMs and UptimeSec are pointers because controllers omit
	// them entirely depending on version and WAN configuration. A
	// missing latency published as 0 reads as "0 ms", which is a
	// measurement rather than the absence of one.
	LatencyMs *int
	UptimeSec *int64
	RxBps     uint64
	TxBps     uint64

	NumUser    int
	NumGuest   int
	NumIoT     int
	NumAP      int
	NumSwitch  int
	NumGateway int
}

// ControllerInfo is what `/v1/info` reports.
type ControllerInfo struct {
	// ApplicationVersion is the UniFi Network application version, e.g.
	// "10.5.67". The daemon logs it at startup and warns below the
	// supported floor (CONCEPT.md §13).
	ApplicationVersion string
}
