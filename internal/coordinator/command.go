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
//  2. Retained messages are dropped, and so are empty ones. On every
//     (re)connect the broker re-delivers the last retained message per
//     filter. Without this check a stale `mosquitto_pub -r` from a test
//     months ago power-cycles a real port on every daemon start; an
//     empty payload is what clearing such a topic looks like.
//
//  3. No optimistic state updates. After a command the affected object
//     is re-polled and the published state comes from the console. A
//     failed command therefore snaps the Home Assistant entity back
//     instead of leaving it lying about what happened (CONCEPT.md §9).

// Command items, under `<name>/set/<site>/…` (mqtt-smarthome 2.0 §3.3).
//
// The switches share their item path with the status item they
// control — `blocked`, `enabled`, and `cmd/locate` beside its read-back
// `locate` — and the three `cmd/…` items are actions with no status of
// their own: any non-empty payload fires them.
const (
	cmdRestart     = "cmd/restart"
	cmdLocate      = "cmd/locate"
	cmdPowerCycle  = "cmd/power_cycle"
	cmdBlocked     = keyClientBlocked
	cmdAuthorize   = "cmd/authorize"
	cmdWLANEnabled = keyWLANEnabled
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
	// topic and payload are the request as it arrived, for the warning
	// a failed command is logged with (spec §3.3).
	topic   string
	payload string
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
// Disjointness is a property of these six and the maintenance filter
// `<name>/maintenance/set/#`, which lives under another function and
// cannot meet them. This daemon holds two other subscriptions — Home
// Assistant's status topic and the sweep's transient `<prefix>/#`
// window — and those two do overlap each other; see watchHomeAssistant
// for why that is benign.
//
// It is also used as [hapub.StateConfig.CommandFilters], which refuses
// a state publish that would land inside this process's own
// subscription. Every filter is under `<name>/set/` and every status
// item under `<name>/status/`, so the guard is inert by construction,
// and the inertness is asserted, not assumed — see
// TestNothingThisDaemonPublishesIsAlsoSubscribed.
func (c *Coordinator) commandFilters() []string { return commandFilters(c.topics) }

// commandFilters is [Coordinator.commandFilters] over a bare topic
// builder, so the state plane can be given the list at construction —
// before there is a Coordinator to ask.
//
// The wildcard levels are joined on by hand: [hatopic.SmartHome.Set]
// makes every segment topic-safe, `+` included, which is right for a
// topic and wrong for a filter.
func commandFilters(b topicBuilder) []string {
	device, client, wlan := b.set(b.site, "device"), b.set(b.site, "client"), b.set(b.site, "wlan")
	return []string{
		device + "/+/" + cmdRestart,
		device + "/+/" + cmdLocate,
		device + "/+/port/+/" + cmdPowerCycle,
		client + "/+/" + cmdBlocked,
		client + "/+/" + cmdAuthorize,
		wlan + "/+/" + cmdWLANEnabled,
	}
}

// subscribeCommands wires the inbound topics through the shared router.
//
// The router owns the per-filter Subscribe loop, the retained drop
// ([hapub.CommandConfig.DeliverRetained]), the overlap refusal and MQTT
// 5.0 No Local; [hapub.CommandRouter.HandleSet] adds spec §5.3 on top:
// `{"val": …}` is unwrapped, other JSON arrives as parameters, an empty
// payload never reaches the handler and malformed JSON is logged at
// warn with its topic and payload.
//
// It is built whenever an inbound client exists, because the
// maintenance commands ride it too ([hapub.Instance.Register]); the six
// command routes are registered only with CONTROLS.ENABLE on.
func (c *Coordinator) subscribeCommands(ctx context.Context) error {
	if c.sub == nil {
		return nil
	}

	router := newCommandRouter(ctx, c.planeTransport(), c.log)
	if c.cfg.Controls.Enable {
		for _, f := range c.commandFilters() {
			if err := router.HandleSet(f, c.onRoutedCommand); err != nil {
				return err
			}
		}
	}
	if err := c.instance.Register(router); err != nil {
		return err
	}
	if len(router.Filters()) == 0 {
		return nil
	}
	// Checked here rather than trusted: the state plane refuses a
	// colliding publish one message at a time, while this refuses the
	// boot. Under mqtt-smarthome no status item can fall inside a `set`
	// filter at all; this is the cheap second opinion over whatever
	// happens to be published at boot, and
	// TestNothingThisDaemonPublishesIsAlsoSubscribed is the one that
	// runs over the whole rendered surface.
	if err := router.CheckDisjoint(c.knownStateTopics()...); err != nil {
		return err
	}
	if err := router.Start(ctx); err != nil {
		return err
	}
	c.router = router
	c.log.Info("coordinator.commands_subscribed",
		slog.Int("filters", len(router.Filters())),
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
func (c *Coordinator) onRoutedCommand(_ context.Context, cmd hapub.Command, v hapub.SetValue) {
	c.onCommand(cmd.Topic, string(cmd.Payload), cmd.Retained, v)
}

// onCommand parses and enqueues one normalised request, and does
// nothing else — see rule 1 above.
func (c *Coordinator) onCommand(topic, payload string, retained bool, v hapub.SetValue) {
	if retained {
		// Rule 2: a retained command is a replay, not a request. The
		// router already drops it; this is the second line.
		c.log.Debug("coordinator.retained_command_dropped", slog.String("topic", topic))
		return
	}

	cmd, ok := c.parseCommand(topic, payload, v)
	if !ok {
		return
	}
	cmd.topic, cmd.payload = topic, payload

	select {
	case c.commands <- cmd:
	default:
		c.log.Warn("coordinator.command_queue_full",
			slog.String("topic", topic),
			slog.String("payload", payload),
			slog.String("note", "something is publishing commands in a loop"))
	}
}

// rejectCommand logs a request that cannot be carried out at warn, with
// its topic and payload, as spec §3.3 requires of a rejected `set`.
func (c *Coordinator) rejectCommand(topic, payload, reason string) {
	c.log.Warn("coordinator.command_rejected",
		slog.String("topic", topic),
		slog.String("payload", payload),
		slog.String("reason", reason))
}

// parseCommand turns a topic and its normalised payload into a queued
// command.
func (c *Coordinator) parseCommand(topic, payload string, v hapub.SetValue) (command, bool) {
	// <name>/set/<site>/<kind>/<id>/<rest...>
	rest, ok := strings.CutPrefix(topic, c.topics.set(c.topics.site)+"/")
	if !ok {
		c.log.Debug("coordinator.unhandled_command", slog.String("topic", topic))
		return command{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return command{}, false
	}
	objectKind, id, item := parts[0], parts[1], strings.Join(parts[2:], "/")

	switch objectKind {
	case "device":
		return c.parseDeviceCommand(topic, payload, id, item, v)
	case "client":
		return c.parseClientCommand(topic, payload, id, item, v)
	case "wlan":
		if item == cmdWLANEnabled && c.cfg.Controls.WLANEnable {
			on, ok := c.boolCommand(topic, payload, v)
			return command{kind: cmdKindWLAN, id: id, on: on}, ok
		}
	}
	c.log.Debug("coordinator.unhandled_command", slog.String("topic", topic))
	return command{}, false
}

// boolCommand reads a switch request with spec §5.3's boolean
// conversions — true/false, 1/0, on/off, yes/no, in any case — and
// rejects anything else rather than reading it as "off".
func (c *Coordinator) boolCommand(topic, payload string, v hapub.SetValue) (on, ok bool) {
	on, err := v.Bool()
	if err != nil {
		c.rejectCommand(topic, payload, err.Error())
		return false, false
	}
	return on, true
}

func (c *Coordinator) parseDeviceCommand(topic, payload, id, item string, v hapub.SetValue) (command, bool) {
	mac, err := model.ParseMAC(id)
	if err != nil || mac.IsZero() {
		c.rejectCommand(topic, payload, "not a device MAC")
		return command{}, false
	}

	switch {
	case item == cmdRestart && c.cfg.Controls.DeviceRestart:
		// An action: any non-empty payload fires it.
		return command{kind: cmdKindRestart, mac: mac}, true
	case item == cmdLocate && c.cfg.Controls.DeviceLocate:
		on, ok := c.boolCommand(topic, payload, v)
		return command{kind: cmdKindLocate, mac: mac, on: on}, ok
	case strings.HasPrefix(item, "port/") && strings.HasSuffix(item, "/"+cmdPowerCycle):
		if !c.cfg.Controls.PortPowerCycle {
			return command{}, false
		}
		idxStr := strings.TrimSuffix(strings.TrimPrefix(item, "port/"), "/"+cmdPowerCycle)
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			c.rejectCommand(topic, payload, "not a port number")
			return command{}, false
		}
		return command{kind: cmdKindPowerCycle, mac: mac, portIdx: idx}, true
	}
	return command{}, false
}

func (c *Coordinator) parseClientCommand(topic, payload, id, item string, v hapub.SetValue) (command, bool) {
	switch {
	case item == cmdBlocked && c.cfg.Controls.ClientBlock:
		mac, err := model.ParseMAC(id)
		if err != nil || mac.IsZero() {
			c.rejectCommand(topic, payload, "not a client MAC")
			return command{}, false
		}
		on, ok := c.boolCommand(topic, payload, v)
		return command{kind: cmdKindBlock, mac: mac, on: on}, ok

	case item == cmdAuthorize && c.cfg.Controls.GuestAuthorize:
		// Guests are keyed by whatever Key() returned, which is the MAC
		// for a wireless client and the UUID for VPN/Teleport.
		cmd := command{kind: cmdKindAuthorize, id: id}
		if mac, err := model.ParseMAC(id); err == nil && !mac.IsZero() {
			cmd.mac = mac
		}
		cmd.minutes = parseMinutes(v)
		return cmd, true
	}
	return command{}, false
}

// parseMinutes reads an optional guest time limit from an authorize
// request: `{"minutes": n}`, or a bare number. Anything else — `{}`,
// Home Assistant's PRESS — means "use the site default". The request
// itself is never empty: an empty payload is dropped before it gets
// here.
func parseMinutes(v hapub.SetValue) int {
	if v.Structured() {
		var body struct {
			Minutes int `json:"minutes"`
		}
		if err := json.Unmarshal(v.Params, &body); err == nil {
			return body.Minutes
		}
		return 0
	}
	if n, err := strconv.Atoi(v.Text); err == nil {
		return n
	}
	return 0
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
		// Warn, with the request as it arrived (spec §3.3 and §11: a
		// failed `set` is logged with topic, payload and the reason).
		log.Warn("coordinator.command_failed",
			slog.String("topic", cmd.topic),
			slog.String("payload", cmd.payload),
			slog.String("err", err.Error()))
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
