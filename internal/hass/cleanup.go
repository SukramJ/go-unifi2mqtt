// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
	"slices"
	"strings"
)

// Reconciling the broker's retained discovery configs against what this
// process announces.
//
// The per-device bookkeeping in the coordinator only knows what *this*
// process published. It cannot see a config left behind by an older
// version that named an entity differently, by a run with a different
// filter set, or by a device removed while the daemon was stopped.
// Those configs are retained, so Home Assistant recreates the entity on
// every start and it sits there unavailable forever, with nothing in
// any log to say why.
//
// The reconcile reads what is actually retained under the discovery
// prefix and clears the orphans among them. Ownership is the delicate
// part: the discovery prefix is shared with every other MQTT
// integration on the broker, and clearing someone else's config deletes
// their entity — silently, because Home Assistant reports nothing when
// an entity vanishes with its retained config.
//
// # Why ownership here is a claim list, not a predicate
//
// Every sibling bridge in this programme separates two instances of
// itself by a string in the payload: go-homeconnect2mqtt requires the
// state topic to sit under the publishing instance's own MQTT root
// (its PR #44), which go-mtec2mqtt's PR #54 established as the rule.
//
// That rule has nothing to key on here. A state topic in this bridge is
// "<root>/<site>/…". The root is MQTT_TOPIC, which is exactly what the
// availability-topic check already reads, and the site segment is
// Site.Internal, which is "default" on every UniFi console out of the
// box. Two instances bridging two *different consoles* to one broker
// with the shipped configuration publish byte-identical config topics,
// unique_ids, availability topics and state topics for the whole site
// plane. No predicate over today's strings separates them, and
// TestOwnershipCannotSeparateTwoConsolesOnOneRoot measured the
// consequence: one console's daemon deleted the other console's SSID
// switch out of Home Assistant.
//
// So ownership is not asked of the payload at all. It is *recorded*:
// the sweep may clear a retained config only if this process published
// that exact topic since it started ([Claims.Published]). Nothing else
// is ours, by construction, whatever it looks like. Everything that
// looks like ours but was never published by this process is reported
// and left alone — see [Discovery.OrphanConfigs].

// Class is the kind of object an owned discovery config belongs to.
//
// It exists so the sweep can be gated per kind: the client poll and the
// classic health poll become ready at different times, and a config
// whose source has not reported yet must not be mistaken for an orphan.
type Class uint8

// Classes of discovery config this package produces.
const (
	// ClassUnknown is a config whose unique_id this version does not
	// recognise — including one written by a future version. It is never
	// swept.
	ClassUnknown Class = iota
	// ClassDevice is an infrastructure device: its ports and radios too.
	ClassDevice
	// ClassClient is a network client.
	ClassClient
	// ClassSite is a site-health entity.
	ClassSite
	// ClassWLAN is an SSID switch.
	ClassWLAN
)

// String names the class for logs.
func (c Class) String() string {
	switch c {
	case ClassDevice:
		return "device"
	case ClassClient:
		return "client"
	case ClassSite:
		return "site"
	case ClassWLAN:
		return "wlan"
	case ClassUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// ConfigFilter is the MQTT filter matching every discovery config topic
// this daemon writes: <prefix>/<platform>/<object>/<key>/config.
//
// # It is not subscribed to by anything, and that is deliberate
//
// The orphan sweep does *not* use this filter. It opens a go-hamqtt
// snapshot window over `<prefix>/#` (see collectRetainedConfigs in the
// coordinator), which is strictly wider — it also replays
// `<prefix>/status` and any four-segment device bundle. Nothing in the
// production path calls this function, so an audit that greps for
// callers finds none, and CONCEPT.md described it as the subscription
// until that was corrected.
//
// What it is instead is the one machine-readable statement of the
// *form* this daemon publishes in: exactly five segments, ending in
// `config`. Three tests hold the real renderers to it —
// TestConfigFilterMatchesEveryConfigTopic here and in the coordinator's
// surface invariants, and TestHamqttBundleWouldBeInvisibleToItsOwnReconcile,
// which records the other half of measurement finding F5: a device
// bundle is four segments and would fall outside this form entirely.
// Retyping "<prefix>/+/+/+/config" in each of them would be a third and
// fourth spelling of a shape the sweep's rebuild step
// ([ConfigTopicFor]) also depends on, which is the kind of untied twin
// this file already has a story about.
//
// So: keep it, do not subscribe to it, and if the published form ever
// gains a fourth segment or a bundle, change it here and read what
// fails.
func (d *Discovery) ConfigFilter() string {
	return d.baseTopic + "/+/+/+/config"
}

// IsOwnConfig reports whether a retained discovery config has the shape
// this daemon publishes: a unique_id in this project's namespace, and
// this bridge's availability topic — which embeds the configured MQTT
// root, so an instance under a *different* root reads as someone
// else's.
//
// It is a shape test, and shape is all it can be. It does not
// distinguish this instance from a second one bridging another console
// under the same root, because nothing in the payload does; that is
// what the claim list in [Discovery.OrphanConfigs] is for, and this
// predicate must never again be used on its own to decide a retraction.
//
// It is still worth having, in two places. It keeps another
// integration's configs out of the sweep's judgement entirely, and it
// is the second half of the retraction test: the claim list says this
// process published that topic, and this says what is retained there
// now is still a config of ours rather than something another writer
// put on top of it.
func (d *Discovery) IsOwnConfig(payload []byte) bool {
	cfg, ok := parseOwnership(payload)
	if !ok {
		return false
	}
	if !strings.HasPrefix(cfg.UniqueID, idPrefix+"_") {
		return false
	}
	want := d.topics.AvailabilityTopic()
	for _, a := range cfg.Availability {
		if a.Topic == want {
			return true
		}
	}
	return false
}

// Claims is what this process knows, first-hand, about its own
// discovery publications. It is the whole of the sweep's ownership
// evidence — see the note at the head of this file for why nothing in
// the payload can supply the rest.
type Claims struct {
	// Published is every config topic this process has *successfully*
	// published since it started — what the broker accepted, never what
	// was merely attempted. It only ever grows: a topic this process
	// announced and later retracted stays claimed, which is precisely
	// what lets a retraction that did not stick be retried.
	Published map[string]bool
	// Announced is the subset still claimed as a live entity. A topic in
	// Published but not in Announced is one this process published and
	// has since given up — the only kind of orphan it may clear.
	Announced map[string]bool
}

// OrphanConfigs sorts the retained discovery configs into the ones this
// daemon may clear and the two kinds it may only talk about.
//
// orphans are retractable: this process published that exact topic
// since it started, it no longer announces it, the payload still has
// this daemon's shape, and the class's source has reported. All four
// must hold. The first is what makes the answer safe against a second
// console on the same root — a config this process never published is
// not ours to delete, however much it looks like ours.
//
// unclaimed are the configs that carry this daemon's shape, were not
// published by this process, and whose class *has* reported: a leftover
// from an earlier run whose catalogue differed, or a sibling console's
// live entity. The two are indistinguishable — that is the finding — so
// neither is touched and the caller reports them, with the advice that
// an operator who runs no second console can clear them by hand.
//
// unready are the same shape with the opposite provenance: their class
// has *not* reported in this run, so the reason they are missing from
// Announced may simply be that their source never answered. Handing an
// operator the same "safe to clear" advice for these is how a
// three-minute console outage turns into a fleet deleted by hand — with
// the classic layer down, none of the site-health configs is in
// Published and every one of them looks exactly like an orphan of an
// earlier run. They are reported separately, and as *not* clearable.
//
// ready reports which classes have completed a successful cycle. A
// class that is not ready never yields an orphan: its absence from
// Announced means "not polled yet", not "gone".
func (d *Discovery) OrphanConfigs(
	retained map[string][]byte,
	claims Claims,
	ready map[Class]bool,
) (orphans, unclaimed, unready []string) {
	for topic, payload := range retained {
		if len(payload) == 0 || claims.Announced[topic] {
			continue // already cleared, or still a current entity
		}
		if !d.IsOwnConfig(payload) {
			continue // another integration's entity — never touch it
		}
		cfg, _ := parseOwnership(payload)
		sourceReported := ready[ClassOf(cfg.UniqueID)]
		if !claims.Published[topic] {
			// Not ours to delete either way — see the head of this file —
			// but which of the two lists it lands in decides what the
			// operator is told to do about it.
			if sourceReported {
				unclaimed = append(unclaimed, topic)
			} else {
				unready = append(unready, topic)
			}
			continue
		}
		if !sourceReported {
			continue // its source has not reported yet
		}
		orphans = append(orphans, topic)
	}
	slices.Sort(orphans)
	slices.Sort(unclaimed)
	slices.Sort(unready)
	return orphans, unclaimed, unready
}

// ClassOf classifies one of this project's unique_ids.
//
// The scheme is "unifi_<scope>_…", where the scope is a literal word
// for clients, site health and WLANs, and a bare MAC for infrastructure
// devices. An id that matches none of those shapes is [ClassUnknown]
// and is therefore never swept — which is what makes a downgrade safe:
// an older binary leaves a newer one's entities alone rather than
// deleting what it does not understand.
func ClassOf(uniqueID string) Class {
	rest, ok := strings.CutPrefix(uniqueID, idPrefix+"_")
	if !ok {
		return ClassUnknown
	}
	scope, tail, ok := strings.Cut(rest, "_")
	if !ok {
		return ClassUnknown // "unifi_<something>" with no key after it
	}
	switch scope {
	case "client":
		return ClassClient
	case "site":
		return ClassSite
	case "wlan":
		return ClassWLAN
	}
	if tail != "" && isMACToken(scope) {
		return ClassDevice
	}
	return ClassUnknown
}

// isMACToken reports whether s is a bare 12-digit hex MAC, the form
// [model.MAC.String] produces and every device unique_id embeds.
func isMACToken(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

// ownership is the slice of a discovery payload the sweep reads.
type ownership struct {
	UniqueID     string              `json:"unique_id"`
	Availability []availabilityEntry `json:"availability"`
}

// parseOwnership extracts the ownership fields from a config payload.
// ok is false when the payload is not a JSON object.
func parseOwnership(payload []byte) (ownership, bool) {
	var cfg ownership
	if json.Unmarshal(payload, &cfg) != nil {
		return ownership{}, false
	}
	return cfg, true
}
