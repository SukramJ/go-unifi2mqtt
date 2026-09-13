// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"strings"

	"github.com/SukramJ/go-hamqtt/publisher"
	hatopic "github.com/SukramJ/go-hamqtt/topic"
)

// What this package tells the go-hamqtt runtime — ADR 0070 phase 9,
// step 5.
//
// The rendering experiment in hamqtt.go stayed unwired on purpose. The
// three strings below are the parts of it the *publishing* planes need,
// and they are here rather than in the coordinator for the same reason
// [Topics] is an interface: a second spelling of any of them is a
// divergence nothing would report.

// NewLayout exposes this bridge's topic tree as a [hatopic.Layout].
//
// It is the same unexported layout the rendering path uses, and it is
// exported because three independent readers now need it to agree: the
// discovery configs' availability entries, [publisher.Config.Layout] —
// which derives the runtime's status topic from [hatopic.Layout.Bridge]
// and refuses a [publisher.Config.StatusTopic] that disagrees with it —
// and the Last Will the MQTT client is built with. One typo there greys
// out the whole fleet under `availability_mode: "all"`, and the only
// evidence is every entity being unavailable at once.
func NewLayout(t Topics) hatopic.Layout { return hamqttLayout{topics: t} }

// publishedPlatforms are the five Home Assistant platforms this daemon
// emits, of the 32 the catalogue declares.
//
// Held as a closed set because it is half of [OwnsConfigTopic]: a
// retained config on a platform this daemon has never published cannot
// be one of ours, whatever else its topic looks like, and a shared
// discovery tree carries plenty of them.
var publishedPlatforms = map[string]bool{
	"binary_sensor":  true,
	"button":         true,
	"device_tracker": true,
	"sensor":         true,
	"switch":         true,
}

// OwnsConfigTopic reports whether a retained discovery config topic has
// the shape this daemon publishes.
//
// It is [publisher.SweepRequest.Owns]: the only thing a sweep window
// can decide from the topic alone, and deliberately **not** the whole
// answer. It scopes the window — it keeps another integration's
// configs, a device document and the node-id-less form out of the
// judgement entirely — and everything that survives it is then judged
// on the claim list, which is the only evidence that separates this
// process from a second console's daemon on the same root. See the head
// of cleanup.go and [Discovery.OrphanConfigs].
//
// The node-id guard is the load-bearing one. This bridge publishes the
// five-segment form exclusively, so a node id is always present and is
// always `device.identifiers[0]` — which always starts with the
// compile-time `unifi_` namespace. The bundle and empty-field guards
// are redundant against that today and are kept as an upgrade
// tripwire: [publisher.ParseConfigTopic] has grown a form before.
func OwnsConfigTopic(t publisher.ConfigTopic) bool {
	if t.Bundle || t.Platform == "" || t.NodeID == "" || t.ObjectID == "" {
		return false
	}
	if !publishedPlatforms[t.Platform] {
		return false
	}
	return strings.HasPrefix(t.NodeID, idPrefix+"_")
}

// ConfigTopicFor rebuilds the config topic a parsed [publisher.ConfigTopic]
// came from, in the five-segment form this daemon publishes.
//
// The sweep hands its [publisher.SweepRequest.Inspect] hook a parsed
// topic and a body, and the claim list is keyed on the topic string, so
// the string has to come back. It is rendered through
// [publisher.LegacyTopicWithNodeID] rather than joined here, because
// that is the same function step 6's retraction list will be rendered
// by: a second spelling would let the sweep and the bundle migration
// disagree about which topic an entity is on.
func ConfigTopicFor(prefix string, t publisher.ConfigTopic) string {
	return publisher.LegacyTopicWithNodeID(publisher.LegacyEntity{
		Prefix:   prefix,
		Platform: t.Platform,
		NodeID:   t.NodeID,
		ObjectID: t.ObjectID,
	})
}

// LegacyConfigTopicForms states which per-entity config topic form this
// daemon's installed fleet is on, for [publisher.Config.LegacyEntityTopics].
//
// The five-segment, node-id-bearing form, measured at **315 of 315** at
// step 4 — and [publisher.LegacyTopicByUniqueID] at **0 of 315**, which
// is why naming the wrong one here would turn a working retraction into
// none at step 6.
//
// It is the library's own default, so stating it changes nothing today.
// It is stated anyway because [publisher.Config.LegacyEntityTopics]
// *replaces* the default rather than extending it: the day a second
// form is added, the reader has to see that the first one was a
// decision and not an omission.
func LegacyConfigTopicForms() []publisher.LegacyTopicFunc {
	return []publisher.LegacyTopicFunc{publisher.LegacyTopicWithNodeID}
}
