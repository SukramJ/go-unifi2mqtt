// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SukramJ/go-unifi2mqtt/internal/version"
)

// The Home Assistant add-on has four files that must agree about the
// same set of options, and every disagreement fails silently in a
// different way:
//
//	config.yaml options    what the user can set
//	config.yaml schema     an option missing here makes the Supervisor
//	                       reject the whole configuration
//	translations/*.yaml    a missing entry shows the raw key
//	                       ("clients_signal_sensor") on the settings page
//	script/run.sh          an option missing here is silently ignored
//	                       at runtime — the worst of the four, because
//	                       everything looks fine
//
// Phase 8 found 22 options unreachable from the add-on this way. These
// tests exist so the next one is caught before release rather than by a
// user wondering why a setting does nothing.

const addonDir = "../../addon"

type addonConfig struct {
	Version string         `yaml:"version"`
	Options map[string]any `yaml:"options"`
	Schema  map[string]any `yaml:"schema"`
}

type translation struct {
	Configuration map[string]struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	} `yaml:"configuration"`
}

func loadAddon(t *testing.T) addonConfig {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(addonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read add-on config: %v", err)
	}
	var cfg addonConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse add-on config: %v", err)
	}
	return cfg
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// missing returns everything in want that is absent from have.
func missing(want []string, have map[string]bool) []string {
	var out []string
	for _, k := range want {
		if !have[k] {
			out = append(out, k)
		}
	}
	return out
}

func TestAddonOptionsAndSchemaAgree(t *testing.T) {
	t.Parallel()

	cfg := loadAddon(t)
	inSchema := make(map[string]bool, len(cfg.Schema))
	for k := range cfg.Schema {
		inSchema[k] = true
	}
	inOptions := make(map[string]bool, len(cfg.Options))
	for k := range cfg.Options {
		inOptions[k] = true
	}

	if got := missing(keys(cfg.Options), inSchema); len(got) > 0 {
		t.Errorf("options without a schema entry (the Supervisor rejects the config): %v", got)
	}
	if got := missing(keys(cfg.Schema), inOptions); len(got) > 0 {
		t.Errorf("schema entries with no default: %v", got)
	}
}

// Without a translation the settings page shows the raw key, which is
// how "clients_signal_sensor" ends up in front of a user.
func TestAddonTranslationsCoverEveryOption(t *testing.T) {
	t.Parallel()

	cfg := loadAddon(t)
	for _, lang := range []string{"en", "de"} {
		raw, err := os.ReadFile(filepath.Join(addonDir, "translations", lang+".yaml"))
		if err != nil {
			t.Errorf("read %s translations: %v", lang, err)
			continue
		}
		var tr translation
		if err := yaml.Unmarshal(raw, &tr); err != nil {
			t.Errorf("parse %s translations: %v", lang, err)
			continue
		}

		have := make(map[string]bool, len(tr.Configuration))
		for k, v := range tr.Configuration {
			have[k] = true
			if strings.TrimSpace(v.Name) == "" {
				t.Errorf("%s: option %q has no name", lang, k)
			}
			if strings.TrimSpace(v.Description) == "" {
				t.Errorf("%s: option %q has no description", lang, k)
			}
		}

		if got := missing(keys(cfg.Options), have); len(got) > 0 {
			t.Errorf("%s: options shown as raw keys on the settings page: %v", lang, got)
		}
		for k := range tr.Configuration {
			if _, ok := cfg.Options[k]; !ok {
				t.Errorf("%s: translation for %q, which is not an option", lang, k)
			}
		}
	}
}

// An option the entrypoint never reads is silently ignored: the user
// sets it, the page accepts it, and nothing happens.
func TestRunScriptPassesEveryOption(t *testing.T) {
	t.Parallel()

	cfg := loadAddon(t)
	raw, err := os.ReadFile("../../script/run.sh")
	if err != nil {
		t.Fatalf("read run.sh: %v", err)
	}

	referenced := make(map[string]bool)
	for _, m := range regexp.MustCompile(`bashio::config '([a-z_]+)`).FindAllStringSubmatch(string(raw), -1) {
		referenced[m[1]] = true
	}

	if got := missing(keys(cfg.Options), referenced); len(got) > 0 {
		t.Errorf("options never read by run.sh (silently ignored at runtime): %v", got)
	}
	for k := range referenced {
		if _, ok := cfg.Options[k]; !ok {
			t.Errorf("run.sh reads %q, which is not an add-on option", k)
		}
	}
}

// The add-on version must equal the binary's: the Supervisor pulls
// ghcr.io/...-addon-{arch}:<version>, so a mismatch means it pulls an
// image that does not exist, or silently runs an older one.
func TestAddonVersionMatchesTheBinary(t *testing.T) {
	t.Parallel()

	if got := loadAddon(t).Version; got != version.Version {
		t.Errorf("add-on version %q != binary version %q", got, version.Version)
	}
}
