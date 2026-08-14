// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"encoding/json"
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
// The reconcile closes that gap by reading what is actually retained
// under the discovery prefix and clearing what this daemon owns but no
// longer announces. Ownership is the delicate part: the discovery
// prefix is shared with every other MQTT integration on the broker, and
// clearing someone else's config deletes their entity.

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
// It spans the whole discovery prefix rather than just this daemon's
// configs, because ownership lives in the payload's unique_id, not in
// the topic — there is no wildcard that expresses "mine". Scoping is
// therefore done in code, in [Discovery.OrphanConfigs].
func (d *Discovery) ConfigFilter() string {
	return d.baseTopic + "/+/+/+/config"
}

// IsOwnConfig reports whether a retained discovery config was published
// by this daemon.
//
// Two independent signals must agree. The unique_id has to sit in this
// project's namespace, and the payload has to name this bridge's
// availability topic — which embeds the configured MQTT root, so a
// second instance bridging another console to the same broker under a
// different root is correctly seen as someone else's.
//
// Requiring both matters because neither alone is enough: another
// integration could coincidentally use an "unifi_" id, and availability
// topics are not unique to a single entity. A config that fails either
// test is left strictly alone.
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

// OrphanConfigs returns the retained config topics this daemon owns,
// no longer announces, and is entitled to clear right now.
//
// published is the set of config topics currently announced; ready
// reports which classes have completed a successful cycle. A class that
// is not ready is skipped entirely: its absence from published means
// "not polled yet", not "gone", and sweeping on that reading would
// delete live entities together with their history.
//
// Configs belonging to another integration, to a future version this
// one cannot classify, and already-cleared (empty) payloads are never
// returned, so the caller can clear everything it gets back.
func (d *Discovery) OrphanConfigs(
	retained map[string][]byte,
	published map[string]bool,
	ready map[Class]bool,
) []string {
	out := make([]string, 0, len(retained))
	for topic, payload := range retained {
		if len(payload) == 0 || published[topic] {
			continue // already cleared, or still a current entity
		}
		if !d.IsOwnConfig(payload) {
			continue // another integration's entity — never touch it
		}
		cfg, _ := parseOwnership(payload)
		if !ready[ClassOf(cfg.UniqueID)] {
			continue // its source has not reported yet
		}
		out = append(out, topic)
	}
	return out
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
