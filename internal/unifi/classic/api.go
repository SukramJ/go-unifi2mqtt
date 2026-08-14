// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package classic

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Health returns the site's health aggregate.
//
// This is the single most valuable thing the classic API provides: the
// Integration API's /wans endpoint carries only an id and a name, so
// WAN status, latency and throughput exist nowhere else
// (CONCEPT.md §2.2).
func (c *Client) Health(ctx context.Context, siteRef string) (model.Health, error) {
	var out envelope[health]
	if err := c.get(ctx, siteRef, "/stat/health", &out); err != nil {
		return model.Health{}, err
	}

	var h model.Health
	for i := range out.Data {
		e := &out.Data[i]
		status := model.SubsystemHealth{Status: strings.ToLower(e.Status)}

		switch strings.ToLower(e.Subsystem) {
		case "wan":
			h.WAN = status
			h.WANIP = parseAddr(e.WANIP)
			h.LatencyMs = e.Latency
			h.UptimeSec = e.Uptime
			// The controller reports current throughput as bytes/s with
			// a "-r" suffix; the topic layout is bits/s, matching the
			// device uplink rates.
			if e.RxBytesR != nil {
				h.RxBps = uint64(*e.RxBytesR * 8)
			}
			if e.TxBytesR != nil {
				h.TxBps = uint64(*e.TxBytesR * 8)
			}
			h.NumGateway = deref(e.NumGW)
		case "wlan":
			h.WLAN = status
			h.NumUser += deref(e.NumUser)
			h.NumGuest += deref(e.NumGuest)
			h.NumIoT += deref(e.NumIoT)
			h.NumAP = deref(e.NumAP)
		case "lan":
			h.LAN = status
			h.NumUser += deref(e.NumUser)
			h.NumGuest += deref(e.NumGuest)
			h.NumIoT += deref(e.NumIoT)
			h.NumSwitch = deref(e.NumSW)
		case "vpn":
			h.VPN = status
		}
	}
	return h, nil
}

// ClientDetails returns the per-client data the Integration API omits,
// keyed by MAC.
//
// Returned as a map rather than a slice because it is used to enrich an
// existing client list: the Integration API decides which clients
// exist, this only adds fields.
func (c *Client) ClientDetails(ctx context.Context, siteRef string) (map[model.MAC]model.Client, error) {
	var out envelope[sta]
	if err := c.get(ctx, siteRef, "/stat/sta", &out); err != nil {
		return nil, err
	}

	details := make(map[model.MAC]model.Client, len(out.Data))
	for i := range out.Data {
		s := &out.Data[i]
		mac, err := model.ParseMAC(s.MAC)
		if err != nil || mac.IsZero() {
			continue
		}

		cl := model.Client{
			MAC:       mac,
			ClassicID: s.ID,
			Hostname:  s.Hostname,
			SSID:      s.ESSID,
			Network:   s.Network,
			Blocked:   deref(s.Blocked),
		}
		if s.VLAN != nil {
			cl.VLAN = *s.VLAN
		}
		// Controllers report signal in one field or the other depending
		// on version and radio; both are dBm.
		switch {
		case s.Signal != nil:
			cl.SignalDBm = *s.Signal
		case s.RSSI != nil:
			cl.SignalDBm = *s.RSSI
		}
		if s.LastSeen != nil {
			cl.LastSeen = time.Unix(*s.LastSeen, 0).UTC()
		}
		details[mac] = cl
	}
	return details, nil
}

// PortPower returns PoE draw in watts per device MAC and port index.
//
// The Integration API has no power field at all, so this is the only
// source (CONCEPT.md §2.2 footnote 3).
func (c *Client) PortPower(ctx context.Context, siteRef string) (map[model.MAC]map[int]float64, error) {
	var out envelope[statDevice]
	if err := c.get(ctx, siteRef, "/stat/device", &out); err != nil {
		return nil, err
	}

	power := make(map[model.MAC]map[int]float64, len(out.Data))
	for i := range out.Data {
		d := &out.Data[i]
		mac, err := model.ParseMAC(d.MAC)
		if err != nil || mac.IsZero() {
			continue
		}
		for j := range d.PortTable {
			p := &d.PortTable[j]
			// poe_power is a string ("7.40") and is "0.00" or absent on
			// ports delivering nothing.
			w, err := strconv.ParseFloat(strings.TrimSpace(p.PoEPower), 64)
			if err != nil || w == 0 {
				continue
			}
			if power[mac] == nil {
				power[mac] = make(map[int]float64, len(d.PortTable))
			}
			power[mac][p.PortIdx] = w
		}
	}
	return power, nil
}

// stamgrCommand is the body of a client-manager command.
type stamgrCommand struct {
	Cmd     string `json:"cmd"`
	MAC     string `json:"mac"`
	Minutes int    `json:"minutes,omitempty"`
}

// SetClientBlocked blocks or unblocks a client.
func (c *Client) SetClientBlocked(ctx context.Context, siteRef string, mac model.MAC, blocked bool) error {
	cmd := "unblock-sta"
	if blocked {
		cmd = "block-sta"
	}
	return c.post(ctx, siteRef, "/cmd/stamgr", stamgrCommand{Cmd: cmd, MAC: mac.Colon()}, nil)
}

// devmgrCommand is the body of a device-manager command.
type devmgrCommand struct {
	Cmd string `json:"cmd"`
	MAC string `json:"mac"`
}

// SetLocate turns a device's locate LED on or off.
func (c *Client) SetLocate(ctx context.Context, siteRef string, mac model.MAC, on bool) error {
	cmd := "unset-locate"
	if on {
		cmd = "set-locate"
	}
	return c.post(ctx, siteRef, "/cmd/devmgr", devmgrCommand{Cmd: cmd, MAC: mac.Colon()}, nil)
}

// deref reads through a pointer field, treating absence as the zero
// value. The classic API omits fields rather than sending nulls, so
// absence is the norm and not worth reporting.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func parseAddr(s string) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}
	}
	return a
}
