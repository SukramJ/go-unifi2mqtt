// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package integration

import (
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Conversion from wire types to the domain model.
//
// The guiding rule is that a malformed *field* must never fail a whole
// object: a device with an unparseable IP is still a device worth
// publishing, it just has no IP sensor. Ubiquiti ships firmware that
// occasionally reports an empty string where the schema promises a
// value, and losing an entire poll over that would be the worse
// outcome. Anything dropped this way is logged at debug level.

func toSite(s site) model.Site {
	return model.Site{
		ID:       s.ID,
		Name:     s.Name,
		Internal: s.InternalReference,
	}
}

func toDeviceOverview(d *deviceOverview, log *slog.Logger) model.Device {
	return model.Device{
		MAC:         parseMAC(d.MACAddress, "device "+d.Name, log),
		ID:          d.ID,
		Name:        d.Name,
		Model:       d.Model,
		Type:        model.DeviceTypeFrom(d.Features, d.Model),
		IP:          parseAddr(d.IPAddress, "device "+d.Name, log),
		State:       toDeviceState(d.State),
		Supported:   d.Supported,
		Firmware:    d.FirmwareVersion,
		UpdateAvail: d.FirmwareUpdatable,
		Features:    d.Features,
	}
}

func toDeviceDetails(d *deviceDetails, log *slog.Logger) model.Device {
	dev := model.Device{
		MAC:         parseMAC(d.MACAddress, "device "+d.Name, log),
		ID:          d.ID,
		Name:        d.Name,
		Model:       d.Model,
		Type:        model.DeviceTypeFrom(featureKeys(d), d.Model),
		IP:          parseAddr(d.IPAddress, "device "+d.Name, log),
		State:       toDeviceState(d.State),
		Supported:   d.Supported,
		Firmware:    d.FirmwareVersion,
		UpdateAvail: d.FirmwareUpdatable,
		AdoptedAt:   parseTime(d.AdoptedAt, "device "+d.Name, log),
		Features:    featureKeys(d),
	}
	if d.Uplink != nil {
		dev.UplinkID = d.Uplink.DeviceID
	}

	dev.Ports = make([]model.Port, 0, len(d.Interfaces.Ports))
	for _, p := range d.Interfaces.Ports {
		dev.Ports = append(dev.Ports, toPort(p))
	}
	dev.Radios = make([]model.Radio, 0, len(d.Interfaces.Radios))
	for _, r := range d.Interfaces.Radios {
		dev.Radios = append(dev.Radios, model.Radio{
			FrequencyGHz: r.FrequencyGHz,
			Channel:      r.Channel,
			ChannelWidth: r.ChannelWidthMHz,
			Standard:     r.WLANStandard,
		})
	}
	return dev
}

// featureKeys flattens the details response's feature object into the
// same string slice the overview response carries, so device
// classification has one input shape regardless of which call produced
// the device.
func featureKeys(d *deviceDetails) []string {
	var out []string
	if d.Features.Switching != nil {
		out = append(out, "switching")
	}
	if d.Features.AccessPoint != nil {
		out = append(out, "accessPoint")
	}
	return out
}

func toPort(p port) model.Port {
	out := model.Port{
		Idx:          p.Idx,
		State:        toPortState(p.State),
		Connector:    p.Connector,
		SpeedMbps:    p.SpeedMbps,
		MaxSpeedMbps: p.MaxSpeedMbps,
	}
	if p.PoE != nil {
		out.PoE = &model.PoEState{
			Enabled:  p.PoE.Enabled,
			Standard: p.PoE.Standard,
			Type:     p.PoE.Type,
			State:    p.PoE.State,
			// PowerW stays zero: the Integration API has no power field
			// at all, only the classic layer supplies it.
		}
	}
	return out
}

// toDeviceState maps the wire enum, folding anything unrecognised into
// UNKNOWN so a value Ubiquiti adds later flows through instead of
// silently reading as OFFLINE.
func toDeviceState(s string) model.DeviceState {
	switch model.DeviceState(s) {
	case model.DeviceOnline, model.DeviceOffline, model.DevicePendingAdoption,
		model.DeviceUpdating, model.DeviceGettingReady, model.DeviceAdopting,
		model.DeviceDeleting, model.DeviceConnectionInterrupted,
		model.DeviceIsolated, model.DeviceIncorrectTopology:
		return model.DeviceState(s)
	default:
		return model.DeviceUnknown
	}
}

func toPortState(s string) model.PortState {
	switch model.PortState(s) {
	case model.PortUp, model.PortDown:
		return model.PortState(s)
	default:
		return model.PortUnknown
	}
}

func toDeviceStats(s *deviceStatistics, name string, log *slog.Logger) model.DeviceStats {
	out := model.DeviceStats{
		LastHeartbeat: parseTime(s.LastHeartbeatAt, "stats "+name, log),
	}
	if s.UptimeSec != nil {
		out.Uptime = time.Duration(*s.UptimeSec) * time.Second
	}
	if s.CPUUtilizationPct != nil {
		out.CPUPct = *s.CPUUtilizationPct
	}
	if s.MemoryUtilizationPct != nil {
		out.MemoryPct = *s.MemoryUtilizationPct
	}
	if s.LoadAverage1Min != nil {
		out.LoadAvg1 = *s.LoadAverage1Min
	}
	if s.Uplink != nil {
		out.UplinkTxBps = s.Uplink.TxRateBps
		out.UplinkRxBps = s.Uplink.RxRateBps
	}

	for _, r := range s.Interfaces.Radios {
		if r.TxRetriesPct == nil {
			continue
		}
		if out.RadioTxRetry == nil {
			out.RadioTxRetry = make(map[float64]float64, len(s.Interfaces.Radios))
		}
		out.RadioTxRetry[r.FrequencyGHz] = *r.TxRetriesPct
	}
	return out
}

func toClient(c *clientOverview, log *slog.Logger) model.Client {
	out := model.Client{
		ID:          c.ID,
		Name:        c.Name,
		Type:        toClientType(c.Type),
		UplinkID:    c.UplinkDeviceID,
		ConnectedAt: parseTime(c.ConnectedAt, "client "+c.Name, log),
		IP:          parseAddr(c.IPAddress, "client "+c.Name, log),
		// VPN and Teleport clients have no MAC; parseMAC returns the
		// zero value for an empty string without complaining.
		MAC:     parseMAC(c.MACAddress, "client "+c.Name, log),
		IsGuest: strings.EqualFold(c.Access.Type, "GUEST"),
	}
	if c.Access.Authorized != nil {
		out.Authorized = *c.Access.Authorized
	}
	return out
}

func toClientType(s string) model.ClientType {
	switch model.ClientType(strings.ToUpper(s)) {
	case model.ClientWired, model.ClientWireless, model.ClientVPN, model.ClientTeleport:
		return model.ClientType(strings.ToUpper(s))
	default:
		return model.ClientUnknown
	}
}

func toNetwork(n *networkOverview, log *slog.Logger) model.Network {
	out := model.Network{
		ID:         n.ID,
		Name:       n.Name,
		VLAN:       n.VLANID,
		Enabled:    n.Enabled,
		Default:    n.Default,
		Management: toManagement(n.Management),
	}
	if n.IPv4Configuration == nil {
		return out
	}

	// hostIpAddress is the gateway's own address inside the subnet
	// ("192.168.1.1"), and prefixLength its size. netip.PrefixFrom keeps
	// the host bits, so Masked() is required to turn it into the network
	// prefix that Contains() expects.
	if addr, err := netip.ParseAddr(n.IPv4Configuration.HostIPAddress); err == nil {
		p := netip.PrefixFrom(addr, n.IPv4Configuration.PrefixLength)
		if p.IsValid() {
			out.Subnets = append(out.Subnets, p.Masked())
		} else {
			log.Debug("integration.bad_subnet",
				slog.String("network", n.Name),
				slog.String("host_ip", n.IPv4Configuration.HostIPAddress),
				slog.Int("prefix_length", n.IPv4Configuration.PrefixLength))
		}
	} else if n.IPv4Configuration.HostIPAddress != "" {
		log.Debug("integration.bad_host_ip",
			slog.String("network", n.Name),
			slog.String("host_ip", n.IPv4Configuration.HostIPAddress))
	}

	for _, extra := range n.IPv4Configuration.AdditionalHostIPSubnets {
		p, err := netip.ParsePrefix(extra)
		if err != nil {
			log.Debug("integration.bad_additional_subnet",
				slog.String("network", n.Name),
				slog.String("subnet", extra))
			continue
		}
		out.Subnets = append(out.Subnets, p.Masked())
	}
	return out
}

func toManagement(s string) model.NetworkManagement {
	switch model.NetworkManagement(strings.ToUpper(s)) {
	case model.NetworkGateway, model.NetworkSwitch, model.NetworkUnmanaged:
		return model.NetworkManagement(strings.ToUpper(s))
	default:
		return model.NetworkUnmanaged
	}
}

func toWLAN(w wifiBroadcastOverview) model.WLAN {
	out := model.WLAN{
		ID:      w.ID,
		Name:    w.Name,
		Enabled: w.Enabled,
	}
	// A NATIVE reference carries no id — the WLAN simply uses the site's
	// default network.
	if w.Network != nil && strings.EqualFold(w.Network.Type, "SPECIFIC") {
		out.NetworkID = w.Network.NetworkID
	}
	return out
}

// --- lenient field parsers ---

func parseMAC(s, subject string, log *slog.Logger) model.MAC {
	m, err := model.ParseMAC(s)
	if err != nil {
		log.Debug("integration.bad_mac", slog.String("subject", subject), slog.String("value", s))
		return ""
	}
	return m
}

func parseAddr(s, subject string, log *slog.Logger) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		log.Debug("integration.bad_ip", slog.String("subject", subject), slog.String("value", s))
		return netip.Addr{}
	}
	return a
}

func parseTime(s, subject string, log *slog.Logger) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		log.Debug("integration.bad_timestamp", slog.String("subject", subject), slog.String("value", s))
		return time.Time{}
	}
	return t.UTC()
}
