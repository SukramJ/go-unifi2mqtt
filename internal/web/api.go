// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
	"github.com/SukramJ/go-unifi2mqtt/internal/version"
)

// The JSON shape the UI consumes.
//
// Deliberately its own type rather than marshalling the domain model:
// the model carries fields the UI has no business showing (API UUIDs,
// the classic Mongo ids), and pinning the wire shape here means a
// refactor of the model cannot silently change what the page reads.

type stateResponse struct {
	Bridge   bridgeInfo        `json:"bridge"`
	Site     siteInfo          `json:"site"`
	Devices  []deviceInfo      `json:"devices"`
	Clients  []clientInfo      `json:"clients"`
	WLANs    []wlanInfo        `json:"wlans"`
	Health   *healthInfo       `json:"health"`
	Loops    []loopInfo        `json:"loops"`
	Errors   []state.LoopError `json:"errors"`
	Language string            `json:"language"`
}

type bridgeInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	UptimeSeconds int64  `json:"uptime_s"`
	MQTTConnected bool   `json:"mqtt_connected"`
	// Capabilities lists the classic-layer features and whether each is
	// currently usable, so a degraded layer is visible rather than
	// showing up as mysteriously missing data.
	Capabilities map[string]bool `json:"capabilities"`
}

type siteInfo struct {
	Name               string `json:"name"`
	Reference          string `json:"reference"`
	ApplicationVersion string `json:"application_version"`
}

type deviceInfo struct {
	MAC         string  `json:"mac"`
	Name        string  `json:"name"`
	Model       string  `json:"model"`
	Type        string  `json:"type"`
	State       string  `json:"state"`
	IP          string  `json:"ip,omitempty"`
	Firmware    string  `json:"firmware,omitempty"`
	UpdateAvail bool    `json:"update_available"`
	UplinkMAC   string  `json:"uplink_mac,omitempty"`
	UptimeS     int64   `json:"uptime_s"`
	CPUPct      float64 `json:"cpu_pct"`
	MemoryPct   float64 `json:"memory_pct"`
	TxBps       uint64  `json:"tx_bps"`
	RxBps       uint64  `json:"rx_bps"`
	// StatsAgeS says how old the statistics are. Without it a stalled
	// statistics loop looks identical to a device that is simply idle.
	StatsAgeS int     `json:"stats_age_s"`
	HasStats  bool    `json:"has_stats"`
	Ports     int     `json:"ports"`
	PoEPorts  int     `json:"poe_ports"`
	PoEWatts  float64 `json:"poe_watts"`
	Radios    int     `json:"radios"`
}

type clientInfo struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	MAC       string `json:"mac,omitempty"`
	Type      string `json:"type"`
	IP        string `json:"ip,omitempty"`
	Network   string `json:"network,omitempty"`
	VLAN      int    `json:"vlan,omitempty"`
	SSID      string `json:"ssid,omitempty"`
	SignalDBm int    `json:"signal_dbm,omitempty"`
	UplinkMAC string `json:"uplink_mac,omitempty"`
	Home      bool   `json:"home"`
	Guest     bool   `json:"guest"`
	Blocked   bool   `json:"blocked"`
	AwayForS  int    `json:"away_for_s"`
}

type wlanInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type healthInfo struct {
	WAN        string `json:"wan"`
	LAN        string `json:"lan"`
	WLAN       string `json:"wlan"`
	VPN        string `json:"vpn"`
	WANIP      string `json:"wan_ip,omitempty"`
	LatencyMs  *int   `json:"latency_ms"`
	RxBps      uint64 `json:"rx_bps"`
	TxBps      uint64 `json:"tx_bps"`
	NumUser    int    `json:"clients_user"`
	NumGuest   int    `json:"clients_guest"`
	NumIoT     int    `json:"clients_iot"`
	NumAP      int    `json:"access_points"`
	NumSwitch  int    `json:"switches"`
	NumGateway int    `json:"gateways"`
}

type loopInfo struct {
	Name   string `json:"name"`
	AgeS   int    `json:"age_s"`
	Failed bool   `json:"failed"`
}

// render converts a store snapshot into the wire shape.
func (s *Server) render(snap state.Snapshot) stateResponse {
	now := time.Now()

	out := stateResponse{
		Loops:  []loopInfo{},
		Errors: []state.LoopError{},
		Bridge: bridgeInfo{
			Version:       version.Version,
			Commit:        version.Commit,
			UptimeSeconds: int64(now.Sub(snap.StartedAt).Seconds()),
			MQTTConnected: snap.MQTTConnected,
			Capabilities:  snap.Capabilities,
		},
		Site: siteInfo{
			Name:               snap.Site.Name,
			Reference:          snap.Site.Internal,
			ApplicationVersion: snap.Info.ApplicationVersion,
		},
		Language: s.cfg.Language,
	}

	if snap.LastErrors != nil {
		out.Errors = snap.LastErrors
	}

	failed := make(map[string]bool, len(snap.LastErrors))
	for _, e := range snap.LastErrors {
		failed[e.Loop] = true
	}
	for name, at := range snap.LastPoll {
		out.Loops = append(out.Loops, loopInfo{
			Name:   name,
			AgeS:   int(now.Sub(at).Seconds()),
			Failed: failed[name],
		})
	}
	// A loop that failed before it ever succeeded has no timestamp, and
	// omitting it would hide the very thing worth seeing.
	for _, e := range snap.LastErrors {
		if _, ok := snap.LastPoll[e.Loop]; !ok {
			out.Loops = append(out.Loops, loopInfo{Name: e.Loop, AgeS: -1, Failed: true})
		}
	}

	out.Devices = make([]deviceInfo, 0, len(snap.Devices))
	for i := range snap.Devices {
		out.Devices = append(out.Devices, renderDevice(&snap.Devices[i], now))
	}

	out.Clients = make([]clientInfo, 0, len(snap.Clients))
	for i := range snap.Clients {
		out.Clients = append(out.Clients, renderClient(&snap.Clients[i], now))
	}

	out.WLANs = make([]wlanInfo, 0, len(snap.WLANs))
	for _, w := range snap.WLANs {
		out.WLANs = append(out.WLANs, wlanInfo{ID: w.ID, Name: w.Name, Enabled: w.Enabled})
	}

	if snap.Health != nil {
		out.Health = renderHealth(snap.Health)
	}
	return out
}

func renderDevice(d *state.DeviceView, now time.Time) deviceInfo {
	out := deviceInfo{
		MAC:         d.MAC.Colon(),
		Name:        d.Name,
		Model:       d.Model,
		Type:        string(d.Type),
		State:       string(d.State),
		Firmware:    d.Firmware,
		UpdateAvail: d.UpdateAvail,
		Ports:       len(d.Ports),
		Radios:      len(d.Radios),
		CPUPct:      d.Stats.CPUPct,
		MemoryPct:   d.Stats.MemoryPct,
		UptimeS:     int64(d.Stats.Uptime.Seconds()),
		TxBps:       d.Stats.UplinkTxBps,
		RxBps:       d.Stats.UplinkRxBps,
	}
	if d.IP.IsValid() {
		out.IP = d.IP.String()
	}
	if !d.UplinkMAC.IsZero() {
		out.UplinkMAC = d.UplinkMAC.Colon()
	}
	if !d.StatsSeen.IsZero() {
		out.HasStats = true
		out.StatsAgeS = int(now.Sub(d.StatsSeen).Seconds())
	}
	for i := range d.Ports {
		if p := d.Ports[i].PoE; p != nil {
			out.PoEPorts++
			out.PoEWatts += p.PowerW
		}
	}
	return out
}

func renderClient(c *state.ClientView, now time.Time) clientInfo {
	out := clientInfo{
		Key:       c.Key(),
		Name:      c.Name,
		Type:      string(c.Type),
		Network:   c.Network,
		VLAN:      c.VLAN,
		SSID:      c.SSID,
		SignalDBm: c.SignalDBm,
		Home:      c.Home,
		Guest:     c.IsGuest,
		Blocked:   c.Blocked,
	}
	if !c.MAC.IsZero() {
		out.MAC = c.MAC.Colon()
	}
	if c.IP.IsValid() {
		out.IP = c.IP.String()
	}
	if !c.UplinkMAC.IsZero() {
		out.UplinkMAC = c.UplinkMAC.Colon()
	}
	if !c.Home && !c.LastSeen.IsZero() {
		out.AwayForS = int(now.Sub(c.LastSeen).Seconds())
	}
	return out
}

func renderHealth(h *model.Health) *healthInfo {
	out := &healthInfo{
		WAN:        h.WAN.Status,
		LAN:        h.LAN.Status,
		WLAN:       h.WLAN.Status,
		VPN:        h.VPN.Status,
		LatencyMs:  h.LatencyMs,
		RxBps:      h.RxBps,
		TxBps:      h.TxBps,
		NumUser:    h.NumUser,
		NumGuest:   h.NumGuest,
		NumIoT:     h.NumIoT,
		NumAP:      h.NumAP,
		NumSwitch:  h.NumSwitch,
		NumGateway: h.NumGateway,
	}
	if h.WANIP.IsValid() {
		out.WANIP = h.WANIP.String()
	}
	return out
}
