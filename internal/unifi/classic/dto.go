// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package classic

import (
	"net/http"

	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// Wire types for the classic controller API.
//
// Unlike the Integration API these have no published schema, so every
// field below is what controllers actually return rather than what a
// specification promises. Decoding is correspondingly permissive:
// pointers where absence is meaningful, and nothing fails a whole
// response because one field changed shape.

// envelope wraps every classic response.
type envelope[T any] struct {
	Meta meta `json:"meta"`
	Data []T  `json:"data"`
}

// meta is the classic API's status block.
type meta struct {
	RC  string `json:"rc"`
	Msg string `json:"msg"`
}

// apiError reports a failure the controller signalled in the body
// rather than in the status code.
//
// This is the trap of the classic API: an expired session arrives as
// HTTP 200 with meta.rc = "error", so a client that only checks status
// codes sees success, decodes an empty data array, and treats a
// logged-out session as "the site has no clients" — forever.
func (e envelope[T]) apiError() error {
	if e.Meta.RC != "error" {
		return nil
	}
	return &unifi.APIError{
		Status:  http.StatusOK,
		Code:    e.Meta.Msg,
		Message: "controller reported an error in the response body",
	}
}

// metaCarrier is implemented by every response envelope, so the request
// path can check for a body-level error without knowing the payload
// type.
type metaCarrier interface {
	apiError() error
}

// health is one subsystem entry of `/stat/health`.
//
// The controller returns one object per subsystem, each carrying a
// different subset of fields — a wan entry has latency and an IP, a
// wlan entry has user counts. One permissive struct covers all of them.
type health struct {
	Subsystem string `json:"subsystem"`
	Status    string `json:"status"`

	// WAN
	WANIP         string   `json:"wan_ip"`
	Latency       *int     `json:"latency"`
	Uptime        *int64   `json:"uptime"`
	XputDown      *float64 `json:"xput_down"`
	XputUp        *float64 `json:"xput_up"`
	SpeedtestPing *float64 `json:"speedtest_ping"`
	RxBytesR      *float64 `json:"rx_bytes-r"`
	TxBytesR      *float64 `json:"tx_bytes-r"`

	// Counts, present on several subsystems.
	NumUser    *int `json:"num_user"`
	NumGuest   *int `json:"num_guest"`
	NumIoT     *int `json:"num_iot"`
	NumAP      *int `json:"num_ap"`
	NumAdopted *int `json:"num_adopted"`
	NumSW      *int `json:"num_sw"`
	NumGW      *int `json:"num_gw"`
}

// sta is one entry of `/stat/sta` — the connected-client list.
//
// This is what the Integration API cannot provide: SSID, signal
// strength, hostname and the blocked flag (CONCEPT.md §2.2).
type sta struct {
	MAC      string `json:"mac"`
	ID       string `json:"_id"`
	Hostname string `json:"hostname"`
	Name     string `json:"name"`
	IP       string `json:"ip"`

	ESSID    string `json:"essid"`
	Signal   *int   `json:"signal"`
	RSSI     *int   `json:"rssi"`
	IsWired  *bool  `json:"is_wired"`
	IsGuest  *bool  `json:"is_guest"`
	Blocked  *bool  `json:"blocked"`
	LastSeen *int64 `json:"last_seen"`
	VLAN     *int   `json:"vlan"`
	Network  string `json:"network"`
}

// statDevice is one entry of `/stat/device`, read only for the PoE
// power draw the Integration API omits entirely.
type statDevice struct {
	MAC       string           `json:"mac"`
	ID        string           `json:"_id"`
	PortTable []statDevicePort `json:"port_table"`
}

type statDevicePort struct {
	PortIdx   int    `json:"port_idx"`
	PoEPower  string `json:"poe_power"`
	PoEEnable *bool  `json:"poe_enable"`
	PoEMode   string `json:"poe_mode"`
}
