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

// subscribeCommands wires the inbound topics.
//
// One wildcard subscription per shape rather than one per object: a
// site with 120 clients would otherwise need 120 subscriptions, and
// each new client would need another at runtime.
func (c *Coordinator) subscribeCommands(ctx context.Context) error {
	if !c.cfg.Controls.Enable || c.sub == nil {
		return nil
	}

	root := c.topics.root
	site := c.topics.site
	filters := []string{
		root + "/" + site + "/device/+/" + cmdRestart,
		root + "/" + site + "/device/+/" + cmdLocateSet,
		root + "/" + site + "/device/+/port/+/" + cmdPowerCycle,
		root + "/" + site + "/client/+/" + cmdBlockedSet,
		root + "/" + site + "/client/+/" + cmdAuthorize,
		root + "/" + site + "/wlan/+/" + cmdWLANEnabled,
	}

	for _, f := range filters {
		// DontSendRetained is belt-and-braces alongside the retain check
		// in the handler: brokers that honour it never deliver the stale
		// message in the first place.
		if _, err := c.sub.Subscribe(ctx, f, mqtt.QoS1, c.onCommand,
			mqtt.WithRetainHandling(mqtt.DontSendRetained)); err != nil {
			return err
		}
	}
	c.log.Info("coordinator.commands_subscribed", slog.Int("filters", len(filters)))
	return nil
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
	case cmdKindRestart, cmdKindPowerCycle, cmdKindLocate, cmdKindWLAN:
		ch = c.nudgeDevices
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
