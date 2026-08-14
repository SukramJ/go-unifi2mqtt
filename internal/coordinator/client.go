// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// Client presence.
//
// device_tracker knows two states, and the mapping is deliberately not
// "in the poll → home, absent → not_home": wireless clients drop out of
// the client list for a cycle while roaming between access points, and
// power-saving phones disappear regularly. Flipping on the first missed
// poll makes every presence automation flap, so a client stays home
// until it has been absent for AWAY_TIMEOUT (CONCEPT.md §6.4).

// Presence payloads. These are the strings device_tracker expects by
// default, so discovery needs no payload_home override.
const (
	payloadHome    = "home"
	payloadNotHome = "not_home"
)

// clientState is what the coordinator remembers between polls.
type clientState struct {
	// lastSeen is when the client last appeared in a poll.
	lastSeen time.Time
	// home is the presence currently published, so a change can be
	// detected without re-deriving it.
	home bool
	// client is the last full record, kept so an absent client's
	// attributes stay published rather than vanishing.
	client model.Client
}

// refreshClients polls the client list, applies the filters and
// publishes presence plus metadata.
func (c *Coordinator) refreshClients(ctx context.Context) error {
	if !c.cfg.Clients.Enable {
		return nil
	}

	clients, err := c.src.Clients(ctx, c.site.ID)
	if err != nil {
		return err
	}

	c.mu.RLock()
	networks := c.networks
	byID := c.deviceIDToMAC
	c.mu.RUnlock()

	// Resolve each client's uplink device the same way devices resolve
	// theirs: the API reports a UUID, everything here is keyed on MACs.
	for i := range clients {
		if id := clients[i].UplinkID; id != "" {
			clients[i].UplinkMAC = byID[id]
		}
	}

	filter := newClientFilter(&c.cfg.Clients, networks, c.log)
	kept := filter.Apply(clients)

	now := c.now()
	present := make(map[string]bool, len(kept))
	for i := range kept {
		present[kept[i].Key()] = true
	}

	for i := range kept {
		if err := c.trackPresent(ctx, &kept[i], now); err != nil {
			c.log.Warn("coordinator.client_publish_failed",
				slog.String("client", kept[i].Name), slog.String("err", err.Error()))
		}
	}
	c.expireAbsent(ctx, present, now)
	return nil
}

// trackPresent records a client as seen and publishes it.
func (c *Coordinator) trackPresent(ctx context.Context, cl *model.Client, now time.Time) error {
	key := cl.Key()

	c.mu.Lock()
	st, known := c.clients[key]
	st.lastSeen = now
	st.home = true
	st.client = *cl
	c.clients[key] = st
	c.mu.Unlock()

	if !known {
		if err := c.publishClientDiscovery(ctx, cl); err != nil {
			return err
		}
	}
	return c.publishClient(ctx, cl, true)
}

// expireAbsent flips clients to not_home once they have been missing
// for longer than the grace period.
//
// Absent clients keep their entities: a device_tracker that disappears
// is worse than one reporting not_home, because an automation
// referencing it starts erroring instead of simply seeing "away".
func (c *Coordinator) expireAbsent(ctx context.Context, present map[string]bool, now time.Time) {
	timeout := c.cfg.Clients.AwayTimeoutDuration()

	c.mu.Lock()
	type expiry struct {
		key    string
		client model.Client
	}
	var expired []expiry
	for key := range c.clients {
		st := c.clients[key]
		if present[key] || !st.home {
			continue
		}
		if now.Sub(st.lastSeen) < timeout {
			continue
		}
		st.home = false
		c.clients[key] = st
		expired = append(expired, expiry{key: key, client: st.client})
	}
	c.mu.Unlock()

	for i := range expired {
		e := &expired[i]
		c.log.Debug("coordinator.client_away",
			slog.String("client", e.client.Name),
			slog.Duration("absent_for", timeout))
		if err := c.publishClient(ctx, &e.client, false); err != nil {
			c.log.Warn("coordinator.client_publish_failed",
				slog.String("client", e.client.Name), slog.String("err", err.Error()))
		}
	}
}

// publishClient writes one client's presence and metadata.
func (c *Coordinator) publishClient(ctx context.Context, cl *model.Client, home bool) error {
	key := cl.Key()

	state := payloadNotHome
	if home {
		state = payloadHome
	}
	if err := c.pub.publish(ctx, c.topics.client(key, keyState), state); err != nil {
		return err
	}

	// An absent client's IP is stale by definition; publishing the last
	// known one would suggest it is still reachable there.
	ip := ""
	if home && cl.IP.IsValid() {
		ip = cl.IP.String()
	}
	if err := c.pub.publish(ctx, c.topics.client(key, keyClientIP), ip); err != nil {
		return err
	}

	if c.clientSignalEnabled() && cl.Type == model.ClientWireless {
		signal := ""
		if home && cl.SignalDBm != 0 {
			signal = strconv.Itoa(cl.SignalDBm)
		}
		if err := c.pub.publish(ctx, c.topics.client(key, keyClientSignal), signal); err != nil {
			return err
		}
	}

	payload, err := json.Marshal(clientAttributes(cl, home))
	if err != nil {
		return err
	}
	return c.pub.publish(ctx, c.topics.client(key, keyAttributes), string(payload))
}

// clientAttrs is the JSON object bound as json_attributes_topic.
type clientAttrs struct {
	Name        string `json:"name"`
	MAC         string `json:"mac,omitempty"`
	Type        string `json:"type"`
	IP          string `json:"ip,omitempty"`
	Network     string `json:"network,omitempty"`
	VLAN        int    `json:"vlan,omitempty"`
	SSID        string `json:"ssid,omitempty"`
	UplinkMAC   string `json:"uplink_mac,omitempty"`
	Guest       bool   `json:"guest"`
	ConnectedAt string `json:"connected_at,omitempty"`
}

func clientAttributes(cl *model.Client, home bool) clientAttrs {
	a := clientAttrs{
		Name:    cl.Name,
		Type:    string(cl.Type),
		Network: cl.Network,
		VLAN:    cl.VLAN,
		SSID:    cl.SSID,
		Guest:   cl.IsGuest,
	}
	if !cl.MAC.IsZero() {
		a.MAC = cl.MAC.Colon()
	}
	if !cl.UplinkMAC.IsZero() {
		a.UplinkMAC = cl.UplinkMAC.Colon()
	}
	if home {
		if cl.IP.IsValid() {
			a.IP = cl.IP.String()
		}
		if !cl.ConnectedAt.IsZero() {
			a.ConnectedAt = cl.ConnectedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return a
}

// publishClientDiscovery announces a newly seen client to Home
// Assistant.
func (c *Coordinator) publishClientDiscovery(ctx context.Context, cl *model.Client) error {
	if c.hass == nil {
		return nil
	}
	entries, err := c.hass.Client(cl, hass.ClientOptions{Signal: c.clientSignalEnabled()})
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := c.pub.publishConfig(ctx, e.ConfigTopic, e.Payload); err != nil {
			return err
		}
	}

	c.mu.Lock()
	topics := make([]string, 0, len(entries))
	for _, e := range entries {
		topics = append(topics, e.ConfigTopic)
	}
	c.announcedClients[cl.Key()] = topics
	c.mu.Unlock()
	return nil
}

// clientSignalEnabled reports whether the signal sensor should be
// announced: the operator asked for it and the classic layer can
// actually supply the values.
func (c *Coordinator) clientSignalEnabled() bool {
	return c.cfg.Clients.SignalSensor && c.caps.Has(unifi.CapClientDetails)
}

// ClientTopic returns the state topic for one client value. Part of the
// hass.Topics contract, so discovery points at the topics this package
// actually publishes to.
func (c *Coordinator) ClientTopic(key, valueKey string) string {
	return c.topics.client(key, valueKey)
}
