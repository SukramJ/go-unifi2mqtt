// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"errors"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// Site health.
//
// This is the one area with no official equivalent: the Integration
// API's /wans endpoint carries an id and a name and nothing else, so
// WAN status, latency and throughput come from the classic API or not
// at all (CONCEPT.md §2.2).

// Health value keys, doubling as topic suffixes and entity keys.
const (
	keyWANState     = "wan/state"
	keyWANIP        = "wan/ip"
	keyWANLatency   = "wan/latency_ms"
	keyWANRx        = "wan/rx_bps"
	keyWANTx        = "wan/tx_bps"
	keyLANState     = "lan/state"
	keyWLANState    = "wlan/state"
	keyVPNState     = "vpn/state"
	keyClientsTotal = "clients/total"
	keyClientsGuest = "clients/guest"
)

// refreshHealth polls the site health aggregate and publishes it.
func (c *Coordinator) refreshHealth(ctx context.Context) error {
	health, err := c.src.Health(ctx, c.site.ID)
	if err != nil {
		// Without the classic layer this endpoint simply does not
		// exist. That is a configuration choice, not a failure, so the
		// loop reports nothing and stops rather than warning forever.
		if errors.Is(err, unifi.ErrCapabilityUnavailable) {
			return nil
		}
		return err
	}
	// Announce the entities only now: the classic layer answered, so
	// the topics they point at will actually receive values.
	if err := c.publishHealthDiscovery(ctx); err != nil {
		return err
	}
	if c.store != nil {
		c.store.SetHealth(health)
	}
	return c.publishHealth(ctx, &health)
}

func (c *Coordinator) publishHealth(ctx context.Context, h *model.Health) error {
	values := []struct {
		key   string
		value any
	}{
		{keyWANState, h.WAN.Status},
		{keyWANIP, optString(addrString(h.WANIP))},
		// Cleared when the controller reports no latency at all, so Home
		// Assistant shows "unknown" rather than a measured 0 ms.
		{keyWANLatency, optInt(h.LatencyMs)},
		{keyWANRx, h.RxBps},
		{keyWANTx, h.TxBps},
		{keyLANState, h.LAN.Status},
		{keyWLANState, h.WLAN.Status},
		{keyVPNState, h.VPN.Status},
		{keyClientsTotal, h.NumUser + h.NumGuest + h.NumIoT},
		{keyClientsGuest, h.NumGuest},
	}
	for _, v := range values {
		if err := c.pub.publish(ctx, c.topics.health(v.key), v.value); err != nil {
			return err
		}
	}

	return c.pub.publish(ctx, c.topics.health(keyAttributes), healthAttrs{
		WANStatus:  h.WAN.Status,
		LANStatus:  h.LAN.Status,
		WLANStatus: h.WLAN.Status,
		VPNStatus:  h.VPN.Status,
		WANIP:      addrString(h.WANIP),
		LatencyMs:  h.LatencyMs,
		UptimeSec:  h.UptimeSec,
		NumUser:    h.NumUser,
		NumGuest:   h.NumGuest,
		NumIoT:     h.NumIoT,
		NumAP:      h.NumAP,
		NumSwitch:  h.NumSwitch,
		NumGateway: h.NumGateway,
	})
}

// healthAttrs is the JSON object bound as json_attributes_topic.
type healthAttrs struct {
	WANStatus  string `json:"wan_status"`
	LANStatus  string `json:"lan_status"`
	WLANStatus string `json:"wlan_status"`
	VPNStatus  string `json:"vpn_status"`
	WANIP      string `json:"wan_ip,omitempty"`
	LatencyMs  *int   `json:"latency_ms,omitempty"`
	UptimeSec  *int64 `json:"uptime_s,omitempty"`
	NumUser    int    `json:"clients_user"`
	NumGuest   int    `json:"clients_guest"`
	NumIoT     int    `json:"clients_iot"`
	NumAP      int    `json:"access_points"`
	NumSwitch  int    `json:"switches"`
	NumGateway int    `json:"gateways"`
}

// optInt is an optional integer as a status value: nil, which clears
// the item, when absent.
func optInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// optString is an optional string as a status value: nil, which clears
// the item, when empty. A status object carrying `""` would show an
// empty state in Home Assistant where "unknown" is the truth.
func optString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func addrString(a interface{ IsValid() bool }) string {
	type stringer interface{ String() string }
	if !a.IsValid() {
		return ""
	}
	if s, ok := a.(stringer); ok {
		return s.String()
	}
	return ""
}
