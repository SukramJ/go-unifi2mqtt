// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package hass

import (
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	t.Parallel()

	if got, want := name("cpu_utilization", LangEN), "CPU utilization"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if got, want := name("cpu_utilization", LangDE), "CPU-Auslastung"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// A key added to the coordinator but forgotten here must produce a
// visibly odd entity name, not an empty one — a blank row in the UI
// hides the omission instead of surfacing it.
func TestUnknownKeyFallsBackToTheKey(t *testing.T) {
	t.Parallel()

	if got, want := name("not_translated_yet", LangDE), "not_translated_yet"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

func TestUnknownLanguageFallsBackToEnglish(t *testing.T) {
	t.Parallel()

	if got, want := name("uptime", "fr"), "Uptime"; got != want {
		t.Errorf("= %q, want the English %q", got, want)
	}
	if got, want := normaliseLang("fr"), LangEN; got != want {
		t.Errorf("normaliseLang(fr) = %q, want %q", got, want)
	}
	if got, want := normaliseLang("DE"), LangDE; got != want {
		t.Errorf("normaliseLang(DE) = %q, want %q", got, want)
	}
	if got, want := normaliseLang(""), LangEN; got != want {
		t.Errorf("normaliseLang(empty) = %q, want %q", got, want)
	}
}

func TestParameterisedNames(t *testing.T) {
	t.Parallel()

	if got, want := nameWith("port_link", LangEN, "3"), "Port 3 link"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if got, want := nameWith("port_link", LangDE, "3"), "Port 3 Verbindung"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
	if got, want := nameWith("radio_channel", LangDE, "5 GHz"), "Funk 5 GHz Kanal"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// Every key must exist in every supported language, and no translation
// may be empty: a missing German string would silently fall back to
// English, which is fine, but an empty one would produce a blank entity.
func TestTranslationTableIsComplete(t *testing.T) {
	t.Parallel()

	for key, entry := range names {
		for _, lang := range []string{LangEN, LangDE} {
			v, ok := entry[lang]
			if !ok {
				t.Errorf("key %q has no %s translation", key, lang)
				continue
			}
			if strings.TrimSpace(v) == "" {
				t.Errorf("key %q has an empty %s translation", key, lang)
			}
		}
	}
}

// A parameterised name has exactly one verb, and both languages must
// agree — a German string missing its %s would render "Port link" with
// no number and collide with its neighbours.
func TestParameterisedKeysAgreeAcrossLanguages(t *testing.T) {
	t.Parallel()

	for key, entry := range names {
		en := strings.Count(entry[LangEN], "%s")
		de := strings.Count(entry[LangDE], "%s")
		if en != de {
			t.Errorf("key %q has %d placeholders in English but %d in German", key, en, de)
		}
		if en > 1 {
			t.Errorf("key %q has %d placeholders, want at most 1", key, en)
		}
	}
}
