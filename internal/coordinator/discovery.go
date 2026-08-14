// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package coordinator

import (
	"context"
	"log/slog"
	"slices"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/hass"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
)

// Home Assistant discovery.
//
// Discovery configs are retained and published at QoS 1: they create
// entities, and a lost one leaves a device silently missing from Home
// Assistant with its state topics still arriving. State values, by
// contrast, are QoS 0 — they are republished on the next poll anyway.

// Subscriber is the inbound MQTT contract, narrowed to what the
// coordinator needs for the Home Assistant birth message.
type Subscriber interface {
	Subscribe(ctx context.Context, filter string, qos mqtt.QoS, handler mqtt.MessageHandler, opts ...mqtt.SubscribeOption) (mqtt.SubscribeResult, error)
}

// publishDiscovery announces one device's entities.
func (c *Coordinator) publishDiscovery(ctx context.Context, dev *model.Device) error {
	if c.hass == nil {
		return nil
	}
	entries, err := c.hass.Device(dev)
	if err != nil {
		return err
	}
	controls, err := c.hass.DeviceControls(dev, c.controlOptions())
	if err != nil {
		return err
	}
	entries = append(entries, controls...)

	topics := make([]string, 0, len(entries))
	for _, e := range entries {
		if err := c.pub.publishConfig(ctx, e.ConfigTopic, e.Payload); err != nil {
			return err
		}
		topics = append(topics, e.ConfigTopic)
	}

	// Remember what this device announced, so entities that stop being
	// generated — a port that disappeared, a radio that was disabled —
	// can be removed rather than lingering as unavailable forever.
	c.mu.Lock()
	previous := c.announced[dev.MAC]
	c.announced[dev.MAC] = topics
	c.mu.Unlock()

	return c.removeStale(ctx, previous, topics)
}

// removeStale clears discovery configs that were announced before but
// are not part of the current set.
func (c *Coordinator) removeStale(ctx context.Context, previous, current []string) error {
	if len(previous) == 0 {
		return nil
	}
	keep := make(map[string]bool, len(current))
	for _, t := range current {
		keep[t] = true
	}
	for _, t := range previous {
		if keep[t] {
			continue
		}
		c.log.Info("coordinator.discovery_removed", slog.String("topic", t))
		if err := c.clearConfig(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// clearConfig removes an entity from Home Assistant by publishing an
// empty retained payload to its config topic. An empty payload is how
// MQTT deletes a retained message, and how Home Assistant is told the
// entity is gone.
func (c *Coordinator) clearConfig(ctx context.Context, topic string) error {
	if err := c.pub.publishConfig(ctx, topic, nil); err != nil {
		return err
	}
	c.pub.forget(topic)
	return nil
}

// forgetDiscovery removes every entity a device announced. Called when
// the device disappears from the site.
func (c *Coordinator) forgetDiscovery(ctx context.Context, mac model.MAC) {
	c.mu.Lock()
	topics := c.announced[mac]
	delete(c.announced, mac)
	c.mu.Unlock()

	for _, t := range topics {
		if err := c.clearConfig(ctx, t); err != nil {
			c.log.Warn("coordinator.discovery_clear_failed",
				slog.String("topic", t), slog.String("err", err.Error()))
		}
	}
	if len(topics) > 0 {
		c.log.Info("coordinator.discovery_cleared",
			slog.String("mac", mac.Colon()), slog.Int("entities", len(topics)))
	}
}

// watchHomeAssistant subscribes to Home Assistant's status topic and
// republishes discovery when it comes back.
//
// Home Assistant forgets MQTT-discovered entities on restart and relies
// on integrations re-announcing them. Without this, every HA restart
// would leave the UniFi devices missing until the daemon happened to
// restart too.
//
// The grace period exists because HA publishes "online" before its MQTT
// integration is ready to process discovery; announcing immediately
// means the payloads are dropped (CONCEPT.md §6.5).
func (c *Coordinator) watchHomeAssistant(ctx context.Context, sub Subscriber) error {
	if c.hass == nil || sub == nil {
		return nil
	}

	topic := c.cfg.HASSBaseTopic + "/status"
	_, err := sub.Subscribe(ctx, topic, mqtt.QoS1, func(msg *mqtt.Message) {
		// The handler runs inline in the client's read loop, so it must
		// not block: anything slower than a channel send stalls
		// acknowledgement processing and the keep-alive watchdog.
		if string(msg.Payload) != "online" {
			return
		}
		select {
		case c.rediscover <- struct{}{}:
		default: // one pending re-announce is enough
		}
	})
	if err != nil {
		return err
	}
	c.log.Info("coordinator.hass_status_subscribed", slog.String("topic", topic))
	return nil
}

// rediscoverLoop waits for Home Assistant birth messages and
// re-announces every known device after the grace period.
func (c *Coordinator) rediscoverLoop(ctx context.Context) error {
	grace := c.cfg.HASSBirthGracetimeDuration()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.rediscover:
			c.log.Info("coordinator.hass_birth",
				slog.Duration("republish_in", grace))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(grace):
			}

			// Drop the remembered payloads for the config topics so the
			// republish is not suppressed by change detection: the
			// payloads are identical, which is exactly the case change
			// detection exists to skip.
			c.forgetAnnounced()
			if err := c.announceAll(ctx); err != nil {
				c.log.Warn("coordinator.rediscover_failed", slog.String("err", err.Error()))
			}
		}
	}
}

// forgetAnnounced drops the change-detection memory for every
// discovery config topic.
func (c *Coordinator) forgetAnnounced() {
	c.mu.RLock()
	macs := make([]model.MAC, 0, len(c.announced))
	for mac := range c.announced {
		macs = append(macs, mac)
	}
	c.mu.RUnlock()

	for _, mac := range macs {
		c.mu.RLock()
		topics := slices.Clone(c.announced[mac])
		c.mu.RUnlock()
		for _, t := range topics {
			c.pub.forget(t)
		}
	}
}

// announceAll republishes discovery for every device in the current
// detail snapshot.
func (c *Coordinator) announceAll(ctx context.Context) error {
	c.mu.RLock()
	devices := make([]*model.Device, 0, len(c.details))
	for mac := range c.details {
		d := c.details[mac]
		devices = append(devices, &d)
	}
	c.mu.RUnlock()

	for i := range devices {
		if err := c.publishDiscovery(ctx, devices[i]); err != nil {
			return err
		}
	}
	c.log.Info("coordinator.discovery_announced", slog.Int("devices", len(devices)))
	return nil
}

// controlOptions reports which control entities to announce.
//
// Gated twice on purpose: on the operator having enabled the control,
// and on the capability actually being available. A switch the daemon
// cannot serve produces an entity that errors on click, which is worse
// than not offering it (CONCEPT.md §3.3).
func (c *Coordinator) controlOptions() hass.ControlOptions {
	if !c.cfg.Controls.Enable {
		return hass.ControlOptions{}
	}
	ctl := c.cfg.Controls
	return hass.ControlOptions{
		// Available on the official API, so config alone decides.
		DeviceRestart:  ctl.DeviceRestart,
		PortPowerCycle: ctl.PortPowerCycle,
		GuestAuthorize: ctl.GuestAuthorize,
		// Classic-only: the capability has to be live too.
		DeviceLocate: ctl.DeviceLocate && c.caps.Has(unifi.CapDeviceLocate),
		ClientBlock:  ctl.ClientBlock && c.caps.Has(unifi.CapClientBlock),
		WLANEnable:   ctl.WLANEnable && c.caps.Has(unifi.CapWLANToggle),
	}
}

// DiscoveryConfig builds the hass configuration for this coordinator's
// topic layout. Exposed so main can construct the builder without
// duplicating the topic rules.
func (c *Coordinator) DiscoveryConfig(baseTopic, language string) hass.Config {
	return hass.Config{
		BaseTopic: baseTopic,
		Topics:    c,
		Site:      c.site.Internal,
		Language:  language,
	}
}

// publishHealthDiscovery announces the site health entities.
//
// Called only once the classic layer has actually answered: announcing
// them without it would create entities whose topics never receive a
// value, leaving them unavailable forever (CONCEPT.md §3.3).
func (c *Coordinator) publishHealthDiscovery(ctx context.Context) error {
	if c.hass == nil || c.healthAnnounced.Load() {
		return nil
	}
	entries, err := c.hass.Health(c.site.Name)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := c.pub.publishConfig(ctx, e.ConfigTopic, e.Payload); err != nil {
			return err
		}
	}
	c.healthAnnounced.Store(true)
	c.log.Info("coordinator.health_discovery_announced", slog.Int("entities", len(entries)))
	return nil
}
