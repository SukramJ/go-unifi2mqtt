// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	hapub "github.com/SukramJ/go-hamqtt/publisher"
	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/model"
)

// Inbound commands.
//
// Three rules shape this file, and each one exists because breaking it
// produces a specific, unpleasant failure:
//
//  1. The MQTT handler never blocks. It runs inline in the client's
//     read loop, the same goroutine that decodes acknowledgements and
//     feeds the keep-alive watchdog. A handler that waits on an HTTP
//     round-trip to the console stalls both, and the watchdog declares
//     a healthy connection dead.
//
//  2. Retained messages are dropped. On every (re)connect the broker
//     re-delivers the last retained message per filter. Without this
//     check a stale `mosquitto_pub -r` from a test months ago
//     power-cycles a real port on every daemon start.
//
//  3. No optimistic state updates. After a command the affected object
//     is re-polled and the published state comes from the console. A
//     failed command therefore snaps the Home Assistant entity back
//     instead of leaving it lying about what happened (CONCEPT.md §9).

// Command topic suffixes.
const (
	cmdRestart     = "cmd/restart"
	cmdLocateSet   = "cmd/locate/set"
	cmdPowerCycle  = "cmd/power_cycle"
	cmdBlockedSet  = "blocked/set"
	cmdAuthorize   = "cmd/authorize"
	cmdWLANEnabled = "enabled/set"
)

// commandQueueSize bounds the pending-command buffer.
//
// Small on purpose: these are human-initiated actions, and a backlog of
// hundreds would mean something is publishing in a loop. Dropping with
// a warning beats growing without limit.
const commandQueueSize = 32

// command is one queued action.
type command struct {
	kind    commandKind
	mac     model.MAC
	id      string
	portIdx int
	on      bool
	minutes int
}

type commandKind int

const (
	cmdKindRestart commandKind = iota
	cmdKindLocate
	cmdKindPowerCycle
	cmdKindBlock
	cmdKindAuthorize
	cmdKindWLAN
)

func (k commandKind) String() string {
	switch k {
	case cmdKindRestart:
		return "restart"
	case cmdKindLocate:
		return "locate"
	case cmdKindPowerCycle:
		return "power_cycle"
	case cmdKindBlock:
		return "block"
	case cmdKindAuthorize:
		return "authorize"
	case cmdKindWLAN:
		return "wlan_enabled"
	default:
		return "unknown"
	}
}

// ErrNoSubscriber is returned by the planes' transport when no inbound
// MQTT client has been wired in.
//
// Reported rather than ignored: the sweep's snapshot window and the
// command router both need a subscription, and a silent no-op there is
// a daemon that accepts no commands and clears no orphans while looking
// perfectly healthy.
var ErrNoSubscriber = errors.New("coordinator: no MQTT subscriber configured")

// commandFilters is the inbound topic tree, as topic filters.
//
// One wildcard subscription per shape rather than one per object: a
// site with 120 clients would otherwise need 120 subscriptions, and
// each new client would need another at runtime.
//
// The six are pairwise disjoint, and that is a property this daemon
// depends on rather than a coincidence. A broker sends one PUBLISH copy
// per matching subscription (MQTT 3.1.1 §4.7.3 / 5.0 §3.3.4 permit it
// and both Mosquitto and EMQX do it), and a client that decides
// delivery by re-matching each arriving copy against its whole filter
// list then runs every matching handler for every copy — so two
// overlapping filters power-cycle a port twice per press, with nothing
// in any log. Disjointness is not asserted in prose here: every filter
// is registered on a [hapub.CommandRouter], whose Handle refuses an
// ambiguous pair outright, and TestSubscriptionFiltersCannotOverlap
// drives that refusal against a deliberately overlapping seventh so the
// pass cannot be vacuous.
//
// Disjointness is a property of these six and nothing wider. This
// daemon holds two other subscriptions — Home Assistant's status topic
// and the sweep's transient `<prefix>/#` window — and those two do
// overlap each other; see watchHomeAssistant for why that is benign.
//
// It is also used as [hapub.StateConfig.CommandFilters], which refuses
// a state publish that would land inside this process's own
// subscription. That guard is inert today and the inertness is
// asserted, not assumed — see TestNothingThisDaemonPublishesIsAlsoSubscribed.
func (c *Coordinator) commandFilters() []string { return commandFilters(c.topics) }

// commandFilters is [Coordinator.commandFilters] over a bare topic
// builder, so the state plane can be given the list at construction —
// before there is a Coordinator to ask.
func commandFilters(b topicBuilder) []string {
	root := b.root
	site := b.site
	return []string{
		root + "/" + site + "/device/+/" + cmdRestart,
		root + "/" + site + "/device/+/" + cmdLocateSet,
		root + "/" + site + "/device/+/port/+/" + cmdPowerCycle,
		root + "/" + site + "/client/+/" + cmdBlockedSet,
		root + "/" + site + "/client/+/" + cmdAuthorize,
		root + "/" + site + "/wlan/+/" + cmdWLANEnabled,
	}
}

// subscribeCommands wires the inbound topics through the shared router.
//
// The router replaces three hand-written pieces: the per-filter
// Subscribe loop, the `if msg.Retain` drop (now
// [hapub.CommandConfig.DeliverRetained]) and the topic arithmetic
// onCommand did by hand. It adds two this daemon did not have — the
// overlap refusal above, and MQTT 5.0 No Local, which stops the broker
// from echoing this process's own publishes into its own command
// handler at the source rather than after the fact.
//
// One wire-visible change: the subscriptions carried
// RetainHandling=DontSendRetained before and carry No Local now. The
// effect is the same or stronger — a retained command is dropped by
// policy instead of by a hand-written check — and no PUBLISH moves.
func (c *Coordinator) subscribeCommands(ctx context.Context) error {
	if !c.cfg.Controls.Enable || c.sub == nil {
		return nil
	}

	router := newCommandRouter(ctx, c.planeTransport(), c.log)
	for _, f := range c.commandFilters() {
		if err := router.Handle(f, c.onRoutedCommand); err != nil {
			return err
		}
	}
	// Checked here rather than trusted: the state plane refuses a
	// colliding publish one message at a time, while this refuses the
	// boot.
	//
	// What it gets to check is narrower than "every published state
	// topic", and the gap is worth naming. subscribeCommands runs before
	// the device, stats, clients and health loops do, so the published
	// set it hands over holds only what has gone out by then — the
	// `client/<key>/blocked` state topic, nearest neighbour of the
	// `client/+/blocked/set` filter, is typically not in it yet. The
	// static guarantee is the one below it: all six filters end in a
	// command suffix (`set`, `restart`, `authorize`, …) that no state
	// topic this catalogue renders carries, which is what makes a
	// self-echo impossible rather than merely unobserved. This check is
	// the cheap second opinion over whatever happens to be published at
	// boot, and TestNothingThisDaemonPublishesIsAlsoSubscribed is the
	// one that runs over the whole rendered surface.
	if err := router.CheckDisjoint(c.knownStateTopics()...); err != nil {
		return err
	}
	if err := router.Start(ctx); err != nil {
		return err
	}
	c.router = router
	c.log.Info("coordinator.commands_subscribed",
		slog.Int("filters", len(c.commandFilters())),
		slog.Bool("attributed", router.Attributed()))
	return nil
}

// knownStateTopics is every topic this daemon has published a state to,
// for the disjointness check. It is the published set rather than a
// re-derived catalogue on purpose: what matters is what actually went
// on the wire, and a second derivation could disagree with it.
func (c *Coordinator) knownStateTopics() []string {
	return append(c.pub.knownTopics(), c.AvailabilityTopic())
}

// onRoutedCommand is the router's handler. It parses and enqueues, and
// does nothing else — see rule 1 above. It runs on a router worker
// rather than the transport's read loop, which is what makes rule 1
// structural instead of a convention.
func (c *Coordinator) onRoutedCommand(_ context.Context, cmd hapub.Command) {
	c.onCommand(&mqtt.Message{Topic: cmd.Topic, Payload: cmd.Payload, Retain: cmd.Retained})
}

// onCommand is the MQTT message handler. It parses and enqueues, and
// does nothing else — see rule 1 above.
func (c *Coordinator) onCommand(msg *mqtt.Message) {
	if msg.Retain {
		// Rule 2: a retained command is a replay, not a request.
		c.log.Debug("coordinator.retained_command_dropped", slog.String("topic", msg.Topic))
		return
	}

	cmd, ok := c.parseCommand(msg)
	if !ok {
		return
	}

	select {
	case c.commands <- cmd:
	default:
		c.log.Warn("coordinator.command_queue_full",
			slog.String("topic", msg.Topic),
			slog.String("note", "something is publishing commands in a loop"))
	}
}

// parseCommand turns a topic and payload into a queued command.
func (c *Coordinator) parseCommand(msg *mqtt.Message) (command, bool) {
	// <root>/<site>/<kind>/<id>/<rest...>
	parts := strings.Split(strings.TrimPrefix(msg.Topic, c.topics.root+"/"+c.topics.site+"/"), "/")
	if len(parts) < 3 {
		return command{}, false
	}
	objectKind, id, rest := parts[0], parts[1], strings.Join(parts[2:], "/")
	payload := strings.TrimSpace(string(msg.Payload))

	switch objectKind {
	case "device":
		return c.parseDeviceCommand(msg.Topic, id, rest, payload)
	case "client":
		return c.parseClientCommand(msg.Topic, id, rest, payload)
	case "wlan":
		if rest == cmdWLANEnabled && c.cfg.Controls.WLANEnable {
			return command{kind: cmdKindWLAN, id: id, on: isOn(payload)}, true
		}
	}
	c.log.Debug("coordinator.unhandled_command", slog.String("topic", msg.Topic))
	return command{}, false
}

func (c *Coordinator) parseDeviceCommand(topic, id, rest, payload string) (command, bool) {
	mac, err := model.ParseMAC(id)
	if err != nil || mac.IsZero() {
		c.log.Warn("coordinator.command_bad_mac", slog.String("topic", topic))
		return command{}, false
	}

	switch {
	case rest == cmdRestart && c.cfg.Controls.DeviceRestart:
		return command{kind: cmdKindRestart, mac: mac}, true
	case rest == cmdLocateSet && c.cfg.Controls.DeviceLocate:
		return command{kind: cmdKindLocate, mac: mac, on: isOn(payload)}, true
	case strings.HasPrefix(rest, "port/") && strings.HasSuffix(rest, cmdPowerCycle):
		if !c.cfg.Controls.PortPowerCycle {
			return command{}, false
		}
		idxStr := strings.TrimSuffix(strings.TrimPrefix(rest, "port/"), "/"+cmdPowerCycle)
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			c.log.Warn("coordinator.command_bad_port", slog.String("topic", topic))
			return command{}, false
		}
		return command{kind: cmdKindPowerCycle, mac: mac, portIdx: idx}, true
	}
	return command{}, false
}

func (c *Coordinator) parseClientCommand(topic, id, rest, payload string) (command, bool) {
	switch {
	case rest == cmdBlockedSet && c.cfg.Controls.ClientBlock:
		mac, err := model.ParseMAC(id)
		if err != nil || mac.IsZero() {
			c.log.Warn("coordinator.command_bad_mac", slog.String("topic", topic))
			return command{}, false
		}
		return command{kind: cmdKindBlock, mac: mac, on: isOn(payload)}, true

	case rest == cmdAuthorize && c.cfg.Controls.GuestAuthorize:
		// Guests are keyed by whatever Key() returned, which is the MAC
		// for a wireless client and the UUID for VPN/Teleport.
		cmd := command{kind: cmdKindAuthorize, id: id}
		if mac, err := model.ParseMAC(id); err == nil && !mac.IsZero() {
			cmd.mac = mac
		}
		cmd.minutes = parseMinutes(payload)
		return cmd, true
	}
	return command{}, false
}

// parseMinutes reads an optional guest time limit. An empty or
// unparseable payload means "use the site default", which is what the
// button sends.
func parseMinutes(payload string) int {
	if payload == "" {
		return 0
	}
	if n, err := strconv.Atoi(payload); err == nil {
		return n
	}
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err == nil {
		return body.Minutes
	}
	return 0
}

// isOn reads a switch payload. Home Assistant sends ON/OFF by default;
// the other spellings cost nothing and save a support question.
func isOn(payload string) bool {
	switch strings.ToLower(payload) {
	case "on", "true", "1", "yes", "home":
		return true
	default:
		return false
	}
}

// commandLoop drains the queue, one command at a time.
//
// Serial on purpose: these are state-changing calls against a box that
// also routes traffic, and two concurrent restarts are never what
// anyone wanted.
func (c *Coordinator) commandLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cmd := <-c.commands:
			c.execute(ctx, cmd)
		}
	}
}

// execute performs one command and triggers the follow-up poll.
func (c *Coordinator) execute(ctx context.Context, cmd command) {
	log := c.log.With(
		slog.String("command", cmd.kind.String()),
		slog.String("target", cmd.targetLabel()),
	)

	if err := c.dispatch(ctx, cmd); err != nil {
		log.Error("coordinator.command_failed", slog.String("err", err.Error()))
		// No state is published here on purpose. The follow-up poll
		// below re-reads the console, so a Home Assistant switch that
		// optimistically flipped snaps back to the truth.
	} else {
		log.Info("coordinator.command_executed")
	}

	// Rule 3: the console decides what the state is now, not us.
	c.scheduleRefresh(cmd)
}

func (c *Coordinator) dispatch(ctx context.Context, cmd command) error {
	switch cmd.kind {
	case cmdKindRestart:
		id, ok := c.deviceIDFor(cmd.mac)
		if !ok {
			return errUnknownDevice
		}
		return c.src.RestartDevice(ctx, c.site.ID, id)

	case cmdKindPowerCycle:
		id, ok := c.deviceIDFor(cmd.mac)
		if !ok {
			return errUnknownDevice
		}
		return c.src.PowerCyclePort(ctx, c.site.ID, id, cmd.portIdx)

	case cmdKindLocate:
		return c.src.SetLocate(ctx, c.site.ID, cmd.mac, cmd.on)

	case cmdKindBlock:
		return c.src.SetClientBlocked(ctx, c.site.ID, cmd.mac, cmd.on)

	case cmdKindAuthorize:
		id, ok := c.clientIDFor(cmd)
		if !ok {
			return errUnknownClient
		}
		return c.src.AuthorizeGuest(ctx, c.site.ID, id, cmd.minutes)

	case cmdKindWLAN:
		return c.src.SetWLANEnabled(ctx, c.site.ID, cmd.id, cmd.on)

	default:
		return errUnknownCommand
	}
}

// Command dispatch errors.
var (
	errUnknownDevice  = errors.New("coordinator: no device with that MAC in the current poll")
	errUnknownClient  = errors.New("coordinator: no client with that key in the current poll")
	errUnknownCommand = errors.New("coordinator: unknown command")
)

// deviceIDFor resolves a MAC to the API's device UUID, which actuator
// calls need. Topics are keyed by MAC because UUIDs change on re-adopt
// (CONCEPT.md §3.4), so this lookup is the price of that choice.
func (c *Coordinator) deviceIDFor(mac model.MAC) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.details[mac]
	if !ok || d.ID == "" {
		return "", false
	}
	return d.ID, true
}

// clientIDFor resolves a client command to the API's client UUID.
func (c *Coordinator) clientIDFor(cmd command) (string, bool) {
	key := cmd.id
	if !cmd.mac.IsZero() {
		key = cmd.mac.String()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.clients[key]
	if !ok || st.client.ID == "" {
		return "", false
	}
	return st.client.ID, true
}

// scheduleRefresh nudges the loop that owns the affected object, so the
// new state is published from the console rather than assumed.
func (c *Coordinator) scheduleRefresh(cmd command) {
	var ch chan struct{}
	switch cmd.kind {
	case cmdKindBlock, cmdKindAuthorize:
		ch = c.nudgeClients
	case cmdKindRestart, cmdKindPowerCycle:
		ch = c.nudgeDevices
	// The locate LED and the SSID enable flag are both published by the
	// static loop — the device list and the WLAN catalogue the fast
	// loop sees carry neither — so nudging the device loop for them
	// republishes an unchanged snapshot and the entity stays on the old
	// value until the next hourly poll.
	case cmdKindLocate, cmdKindWLAN:
		ch = c.nudgeStatic
	default:
		return
	}

	select {
	case ch <- struct{}{}:
	default: // a refresh is already pending; one is enough
	}
}

func (cmd command) targetLabel() string {
	if !cmd.mac.IsZero() {
		if cmd.kind == cmdKindPowerCycle {
			return cmd.mac.Colon() + " port " + strconv.Itoa(cmd.portIdx)
		}
		return cmd.mac.Colon()
	}
	return cmd.id
}
