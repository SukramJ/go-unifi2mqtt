// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"fmt"
	"strings"
)

// Localisation of entity display names.
//
// The rule this file implements, from CONCEPT.md §6.2:
//
//	unique_id       never localised
//	object_id       never localised (it seeds the entity_id)
//	name            localised
//	topic segments  never localised
//
// Switching LANGUAGE therefore renames what the user sees and nothing
// else. It never re-creates an entity, never changes an entity_id, and
// never orphans history — which is the whole point, because a language
// change is a display preference and must not cost a year of recorded
// data.
//
// A missing translation falls back to English rather than to an empty
// label: a German user seeing one English name is a small blemish; an
// entity called "" is a bug report.

// Supported languages.
const (
	LangEN = "en"
	LangDE = "de"
)

// names maps a stable entity key to its display name per language.
//
// The keys are exactly the topic suffixes from internal/coordinator —
// that is deliberate. One key means one lookup, so a renamed topic
// cannot silently drift away from its translation and its unique_id.
var names = map[string]map[string]string{
	// --- device ---
	"state": {
		LangEN: "State",
		LangDE: "Status",
	},
	"reachable": {
		LangEN: "Reachable",
		LangDE: "Erreichbar",
	},
	"uptime": {
		LangEN: "Uptime",
		LangDE: "Betriebszeit",
	},
	"cpu_utilization": {
		LangEN: "CPU utilization",
		LangDE: "CPU-Auslastung",
	},
	"memory_utilization": {
		LangEN: "Memory utilization",
		LangDE: "Speicherauslastung",
	},
	"uplink_tx_bps": {
		LangEN: "Uplink upload",
		LangDE: "Uplink Senderate",
	},
	"uplink_rx_bps": {
		LangEN: "Uplink download",
		LangDE: "Uplink Empfangsrate",
	},
	"firmware": {
		LangEN: "Firmware",
		LangDE: "Firmware",
	},
	"update_available": {
		LangEN: "Update available",
		LangDE: "Update verfügbar",
	},

	// --- port ---
	"port_link": {
		LangEN: "Port %s link",
		LangDE: "Port %s Verbindung",
	},
	"port_speed": {
		LangEN: "Port %s speed",
		LangDE: "Port %s Geschwindigkeit",
	},
	"port_poe": {
		LangEN: "Port %s PoE",
		LangDE: "Port %s PoE",
	},

	// --- radio ---
	"radio_channel": {
		LangEN: "Radio %s channel",
		LangDE: "Funk %s Kanal",
	},
	"radio_tx_retries": {
		LangEN: "Radio %s TX retries",
		LangDE: "Funk %s Sendewiederholungen",
	},

	// --- client ---
	"client_presence": {
		LangEN: "Presence",
		LangDE: "Anwesenheit",
	},
	"client_ip": {
		LangEN: "IP address",
		LangDE: "IP-Adresse",
	},
	"client_signal": {
		LangEN: "Signal strength",
		LangDE: "Signalstärke",
	},
	"client_blocked": {
		LangEN: "Blocked",
		LangDE: "Blockiert",
	},

	// --- WLAN ---
	"wlan_enabled": {
		LangEN: "Enabled",
		LangDE: "Aktiviert",
	},
}

// name returns the display name for key in lang, falling back to
// English and finally to the key itself.
//
// The last fallback matters: a key added to the coordinator but
// forgotten here should produce a visibly odd but usable entity name,
// not an empty one, so the omission is obvious in the UI instead of
// hiding as a blank row.
func name(key, lang string) string {
	entry, ok := names[key]
	if !ok {
		return key
	}
	if s, ok := entry[lang]; ok && s != "" {
		return s
	}
	return entry[LangEN]
}

// nameWith returns a parameterised display name, e.g. "Port 3 link".
func nameWith(key, lang, arg string) string {
	return fmt.Sprintf(name(key, lang), arg)
}

// normaliseLang maps an operator-supplied language onto a supported
// one, defaulting to English.
func normaliseLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case LangDE:
		return LangDE
	default:
		return LangEN
	}
}
