// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/SukramJ/go-bridgeui"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/state"
)

// The groups and tables the layout places.
const (
	groupHealth  = "health"
	groupWLANs   = "wlans"
	tableDevices = "devices"
	tableClients = "clients"
)

// refresh is how often the page is redrawn. The store has no change
// signal of its own, and the ages it shows (loops, absent clients) move
// without one; the previous page polled at the same rate.
var refresh = 5 * time.Second

// staleLoop is the age from which a loop that has not completed is
// flagged before it errors — the clearest sign something is wrong.
const staleLoop = 5 * time.Minute

// adapter presents the state store as go-bridgeui's contract: the site is
// the one device, its health and WLANs are entries, its devices and
// clients tables. It writes nothing.
type adapter struct {
	store   *state.Store
	cfg     *config.Config
	version string
}

// siteID is the device ID of the site.
func siteID(snap *state.Snapshot) string {
	if snap.Site.Internal != "" {
		return snap.Site.Internal
	}
	return "site"
}

func (a *adapter) Snapshot() bridgeui.Snapshot {
	snap := a.store.Snapshot()
	now := time.Now()
	dev := bridgeui.Device{
		ID:      siteID(&snap),
		Title:   firstNonEmpty(snap.Site.Name, "UniFi"),
		Entries: healthEntries(snap.Health),
		Tables:  []bridgeui.Table{devicesTable(snap.Devices)},
	}
	if v := snap.Info.ApplicationVersion; v != "" {
		dev.Subtitle = "UniFi Network " + v
	}
	for _, at := range snap.LastPoll {
		if at.After(dev.UpdatedAt) {
			dev.UpdatedAt = at
		}
	}
	for _, w := range snap.WLANs {
		dev.Entries = append(dev.Entries, bridgeui.Entry{
			Group: groupWLANs, Key: "wlan_" + w.ID, Name: w.Name, Value: w.Enabled, Tone: tone(w.Enabled, ""),
		})
	}
	if a.cfg.Clients.Enable {
		dev.Tables = append(dev.Tables, clientsTable(snap.Clients, now))
	}
	return bridgeui.Snapshot{Devices: []bridgeui.Device{dev}}
}

func healthEntries(h *model.Health) []bridgeui.Entry {
	if h == nil {
		return nil
	}
	sub := func(key, name, status string) bridgeui.Entry {
		t := ""
		switch status {
		case "ok":
			t = "ok"
		case "", "unknown":
		default:
			t = "warn"
		}
		return bridgeui.Entry{Group: groupHealth, Key: key, Name: name, Value: firstNonEmpty(status, "–"), Tone: t}
	}
	num := func(key, name string, v any, unit string) bridgeui.Entry {
		return bridgeui.Entry{Group: groupHealth, Key: key, Name: name, Value: v, Unit: unit}
	}
	out := []bridgeui.Entry{
		sub("wan", "WAN", h.WAN.Status),
		sub("lan", "LAN", h.LAN.Status),
		sub("wlan", "WLAN", h.WLAN.Status),
		sub("vpn", "VPN", h.VPN.Status),
		num("rx", "health.rx", mbit(h.RxBps), "Mbit/s"),
		num("tx", "health.tx", mbit(h.TxBps), "Mbit/s"),
		num("clients", "health.clients", h.NumUser+h.NumGuest+h.NumIoT, ""),
		num("guests", "health.guests", h.NumGuest, ""),
		num("aps", "health.aps", h.NumAP, ""),
		num("switches", "health.switches", h.NumSwitch, ""),
		num("gateways", "health.gateways", h.NumGateway, ""),
	}
	if h.WANIP.IsValid() {
		out = append(out, num("wan_ip", "health.wanIP", h.WANIP.String(), ""))
	}
	if h.LatencyMs != nil {
		out = append(out, num("latency", "health.latency", *h.LatencyMs, "ms"))
	}
	return out
}

// mbit is a bit rate in Mbit/s, to one decimal.
func mbit(bps uint64) float64 {
	return float64(bps/100_000) / 10
}

var deviceColumns = []bridgeui.Column{
	{Label: "col.name"},
	{Label: "col.model"},
	{Label: "col.state", Format: bridgeui.FormatI18n},
	{Label: "col.ip"},
	{Label: "col.firmware"},
	{Label: "col.uptime", Format: bridgeui.FormatDuration, Align: "right"},
	{Label: "col.cpu", Unit: "%", Align: "right"},
	{Label: "col.memory", Unit: "%", Align: "right"},
	{Label: "col.rx", Format: bridgeui.FormatBitrate, Align: "right"},
	{Label: "col.tx", Format: bridgeui.FormatBitrate, Align: "right"},
	{Label: "col.poe", Unit: "W", Align: "right"},
	{Label: "col.uplink"},
}

func devicesTable(devices []state.DeviceView) bridgeui.Table {
	rows := make([]bridgeui.Row, 0, len(devices))
	for i := range devices {
		d := &devices[i]
		name := d.Name
		if d.UpdateAvail {
			name += " ⬆"
		}
		firmware := d.Firmware
		var uptime, cpu, mem, rx, tx any
		// Without statistics there is nothing to show: a stalled
		// statistics loop must not read as an idle device.
		if !d.StatsSeen.IsZero() {
			uptime = int64(d.Stats.Uptime.Seconds())
			cpu, mem = round1(d.Stats.CPUPct), round1(d.Stats.MemoryPct)
			rx, tx = d.Stats.UplinkRxBps, d.Stats.UplinkTxBps
		}
		var poe any
		var watts float64
		ports := 0
		for j := range d.Ports {
			if p := d.Ports[j].PoE; p != nil {
				ports++
				watts += p.PowerW
			}
		}
		if ports > 0 {
			poe = round1(watts)
		}
		var ip, uplink any
		if d.IP.IsValid() {
			ip = d.IP.String()
		}
		if !d.UplinkMAC.IsZero() {
			uplink = d.UplinkMAC.Colon()
		}
		rows = append(rows, bridgeui.Row{
			Tone:  deviceTone(d.State),
			Cells: []any{name, d.Model, "state." + string(d.State), ip, firmware, uptime, cpu, mem, rx, tx, poe, uplink},
		})
	}
	return bridgeui.Table{ID: tableDevices, Title: "table.devices", Columns: deviceColumns, Rows: rows}
}

func deviceTone(s model.DeviceState) string {
	switch s {
	case model.DeviceOnline:
		return "ok"
	case model.DeviceOffline, model.DeviceConnectionInterrupted, model.DeviceIsolated:
		return "err"
	case model.DevicePendingAdoption, model.DeviceUpdating, model.DeviceGettingReady, model.DeviceAdopting,
		model.DeviceDeleting, model.DeviceIncorrectTopology, model.DeviceUnknown:
		return "warn"
	}
	return "warn"
}

var clientColumns = []bridgeui.Column{
	{Label: "col.name"},
	{Label: "col.type", Format: bridgeui.FormatI18n},
	{Label: "col.presence", Format: bridgeui.FormatI18n},
	{Label: "col.awayFor", Format: bridgeui.FormatDuration, Align: "right"},
	{Label: "col.address"},
	{Label: "col.network"},
	{Label: "col.ssid"},
	{Label: "col.signal", Unit: "dBm", Align: "right"},
}

func clientsTable(clients []state.ClientView, now time.Time) bridgeui.Table {
	rows := make([]bridgeui.Row, 0, len(clients))
	for i := range clients {
		c := &clients[i]
		presence, rowTone := "val.home", ""
		var away any
		if !c.Home {
			presence, rowTone = "val.away", "muted"
			if !c.LastSeen.IsZero() {
				away = int64(now.Sub(c.LastSeen).Seconds())
			}
		}
		if c.Blocked {
			presence, rowTone = "val.blocked", "err"
		}
		var addr, network, ssid, signal any
		switch {
		case c.IP.IsValid():
			addr = c.IP.String()
		case !c.MAC.IsZero():
			addr = c.MAC.Colon()
		}
		if c.Network != "" {
			n := c.Network
			if c.VLAN != 0 {
				n += " · VLAN " + strconv.Itoa(c.VLAN)
			}
			network = n
		}
		if c.SSID != "" {
			ssid = c.SSID
		}
		if c.SignalDBm != 0 {
			signal = c.SignalDBm
		}
		rows = append(rows, bridgeui.Row{
			Tone:  rowTone,
			Cells: []any{c.Name, "client." + string(c.Type), presence, away, addr, network, ssid, signal},
		})
	}
	return bridgeui.Table{ID: tableClients, Title: "table.clients", Columns: clientColumns, Rows: rows}
}

func (a *adapter) Health() bridgeui.Health {
	snap := a.store.Snapshot()
	now := time.Now()

	status := "ok"
	switch {
	case len(snap.LastPoll) == 0 && len(snap.LastErrors) == 0:
		status = "starting"
	case !snap.MQTTConnected || len(snap.LastErrors) > 0:
		status = "degraded"
	}

	mqtt := bridgeui.Tile{Label: "health.mqtt", Text: "val.disconnected", Tone: "err", Sub: a.cfg.MQTTServer}
	if snap.MQTTConnected {
		mqtt.Text, mqtt.Tone = "val.connected", "ok"
	}
	site := bridgeui.Tile{Label: "health.site", Value: firstNonEmpty(snap.Site.Name, "—"), Sub: snap.Info.ApplicationVersion}
	loops := loopTiles(&snap, now)
	tiles := make([]bridgeui.Tile, 0, 3+len(loops))
	tiles = append(tiles, mqtt, site, bridgeui.Tile{Label: "health.topic", Value: firstNonEmpty(a.cfg.MQTTTopic, "—")})
	tiles = append(tiles, loops...)

	return bridgeui.Health{
		Status:    status,
		Version:   a.version,
		StartedAt: snap.StartedAt,
		Tiles:     tiles,
		Problems:  problems(&snap),
	}
}

// loopTiles shows each poll loop's last completion, flagged when it
// failed or has not completed in a while.
func loopTiles(snap *state.Snapshot, now time.Time) []bridgeui.Tile {
	failed := make(map[string]bool, len(snap.LastErrors))
	for _, e := range snap.LastErrors {
		failed[e.Loop] = true
	}
	names := make([]string, 0, len(snap.LastPoll)+len(failed))
	for name := range snap.LastPoll {
		names = append(names, name)
	}
	// A loop that failed before it ever succeeded has no timestamp, and
	// omitting it would hide the very thing worth seeing.
	for name := range failed {
		if _, ok := snap.LastPoll[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]bridgeui.Tile, 0, len(names))
	for _, name := range names {
		tile := bridgeui.Tile{Label: "loop." + name}
		at, ok := snap.LastPoll[name]
		if ok {
			age := now.Sub(at)
			tile.Value = formatAge(age)
			if age > staleLoop {
				tile.Tone = "warn"
			}
		} else {
			tile.Text, tile.Tone = "val.never", "warn"
		}
		if failed[name] {
			tile.Tone = "err"
		}
		out = append(out, tile)
	}
	return out
}

// problems are the loops' last failures and the classic-layer features
// that are not usable.
func problems(snap *state.Snapshot) []bridgeui.Problem {
	out := make([]bridgeui.Problem, 0, len(snap.LastErrors))
	for _, e := range snap.LastErrors {
		out = append(out, bridgeui.Problem{At: e.At, Event: "loop." + e.Loop, Detail: e.Err})
	}
	caps := make([]string, 0, len(snap.Capabilities))
	for c, ok := range snap.Capabilities {
		if !ok {
			caps = append(caps, c)
		}
	}
	sort.Strings(caps)
	for _, c := range caps {
		out = append(out, bridgeui.Problem{At: snap.StartedAt, Event: "capability.unavailable", Detail: c})
	}
	return out
}

func (a *adapter) Config() []bridgeui.Tile {
	c := a.cfg
	onOff := func(label string, on bool) bridgeui.Tile {
		if on {
			return bridgeui.Tile{Label: label, Text: "val.on"}
		}
		return bridgeui.Tile{Label: label, Text: "val.off"}
	}
	console := bridgeui.Tile{Label: "cfg.console", Value: firstNonEmpty(c.Host, "—"), Sub: c.Site}
	if c.Port != 0 {
		console.Value = c.Host + ":" + strconv.Itoa(c.Port)
	}
	tls := onOff("cfg.verifyTLS", c.VerifyTLS)
	if !c.VerifyTLS {
		tls.Tone = "warn"
	}
	hass := onOff("cfg.hass", c.HASSEnable)
	if c.HASSEnable {
		hass.Sub = c.HASSBaseTopic
	}
	auth := bridgeui.Tile{Label: "cfg.webAuth", Text: "val.on"}
	if c.WebUser == "" && c.WebPassword.Reveal() == "" {
		auth = bridgeui.Tile{Label: "cfg.webAuth", Text: "val.off", Tone: "warn"}
	}
	clients := onOff("cfg.clients", c.Clients.Enable)
	if !c.Clients.Enable {
		clients.Sub = "CLIENTS.ENABLE"
	}
	return []bridgeui.Tile{
		console,
		tls,
		onOff("cfg.classic", c.ClassicEnable),
		{Label: "cfg.mqtt", Value: firstNonEmpty(c.MQTTServer, "—"), Sub: c.MQTTTopic},
		hass,
		clients,
		onOff("cfg.controls", c.Controls.Enable),
		{Label: "cfg.refresh", Value: strconv.Itoa(c.RefreshDevices) + " / " + strconv.Itoa(c.RefreshDeviceStats) + " / " +
			strconv.Itoa(c.RefreshClients) + " / " + strconv.Itoa(c.RefreshHealth) + " s"},
		auth,
		{Label: "cfg.language", Value: firstNonEmpty(c.Language, "en")},
		onOff("cfg.debug", c.Debug),
	}
}

// Items is empty: the UI is read-only, the write path is MQTT.
func (a *adapter) Items() []bridgeui.Item { return nil }

func (a *adapter) Write(context.Context, string, string, string) error {
	return bridgeui.ErrNotWritable
}

// Changes signals every refresh interval.
func (a *adapter) Changes() (events <-chan struct{}, cancel func()) {
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(refresh)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	return out, func() { once.Do(func() { close(done) }) }
}

func tone(ok bool, otherwise string) string {
	if ok {
		return "ok"
	}
	return otherwise
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

func formatAge(d time.Duration) string {
	s := int64(d.Seconds())
	switch {
	case s < 60:
		return strconv.FormatInt(max(s, 0), 10) + " s"
	case s < 3600:
		return strconv.FormatInt(s/60, 10) + " min"
	}
	return strconv.FormatInt(s/3600, 10) + " h"
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
