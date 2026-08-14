// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

// Command unifi2mqtt is the UniFi Network → MQTT bridge daemon.
//
// It talks to a local UniFi console (UDM / UCG / UniFi OS Server /
// software controller) through the official Network Integration API
// and — where the official surface has gaps — the classic controller
// API, and republishes sites, devices, clients and health data to an
// MQTT broker with optional Home Assistant auto-discovery.
//
// Build status: phase 1 of CONCEPT.md §12. Configuration, the console
// client and the domain model are in place, so `--once` connects, reads
// the whole site inventory and reports it — which is also the fastest
// way to check credentials and filters against a real console. The MQTT
// publication loop is phase 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/model"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi"
	"github.com/SukramJ/go-unifi2mqtt/internal/unifi/integration"
	"github.com/SukramJ/go-unifi2mqtt/internal/version"
)

// minAppVersion is the UniFi Network version this project is verified
// against. Older consoles are not refused — the Integration API may
// well work — but the operator is told they are off the tested path
// (CONCEPT.md §13).
const (
	minAppMajor = 10
	minAppMinor = 5
)

func main() {
	configPath := flag.String("config", "",
		"explicit config.yaml path (defaults to the standard search order)")
	once := flag.Bool("once", false,
		"connect, print the site inventory and exit (diagnostics)")
	showVersion := flag.Bool("version", false, "print build info and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("unifi2mqtt.boot", slog.String("build", version.String()))

	if err := run(*configPath, *once, logger); fatalErr(err) {
		logger.Error("unifi2mqtt.fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// fatalErr reports whether err is a genuine failure rather than the
// normal outcome of a cancelled run context. A SIGINT/SIGTERM landing
// during startup surfaces as context.Canceled; exiting 1 for that would
// make systemd record a clean `systemctl stop` as a unit failure.
func fatalErr(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled)
}

// run is the testable entry point: a non-nil error on any startup or
// runtime failure, nil on clean shutdown.
func run(configPath string, once bool, logger *slog.Logger) error {
	cfg, err := loadConfig(configPath, logger)
	if err != nil {
		return err
	}
	if cfg.Debug {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		slog.SetDefault(logger)
	}
	for _, w := range cfg.Warnings() {
		logger.Warn("unifi2mqtt.config_warning", slog.String("note", w))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, site, err := connect(ctx, cfg, logger)
	if err != nil {
		return err
	}

	if !once {
		// Refusing beats pretending: a daemon that connects and then
		// sits idle looks healthy to systemd while publishing nothing.
		return errors.New(
			"unifi2mqtt: the MQTT bridge loop is not implemented yet (phase 2 of CONCEPT.md); " +
				"run with --once to verify the console connection meanwhile",
		)
	}
	return inventory(ctx, client, cfg, site, logger)
}

// connect builds the console client, probes the API and resolves the
// configured site.
func connect(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*integration.Client, model.Site, error) {
	tr, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL:   cfg.BaseURL(),
		Timeout:   cfg.HTTPTimeoutDuration(),
		Retries:   cfg.HTTPRetries,
		VerifyTLS: cfg.VerifyTLS,
		CAFile:    cfg.CAFile,
		Logger:    logger,
	})
	if err != nil {
		return nil, model.Site{}, err
	}

	client := integration.New(integration.Config{
		Transport: tr,
		APIKey:    cfg.APIKey.Reveal(),
		Logger:    logger,
	})

	info, err := client.Probe(ctx)
	if err != nil {
		return nil, model.Site{}, fmt.Errorf("unifi2mqtt: connect to %s: %w", cfg.BaseURL(), err)
	}
	logger.Info("unifi2mqtt.console",
		slog.String("host", cfg.Host),
		slog.String("application_version", info.ApplicationVersion))
	warnOldVersion(info.ApplicationVersion, logger)

	site, err := client.ResolveSite(ctx, cfg.Site)
	if err != nil {
		return nil, model.Site{}, err
	}
	logger.Info("unifi2mqtt.site",
		slog.String("name", site.Name),
		slog.String("reference", site.Internal),
		slog.String("id", site.ID))

	return client, site, nil
}

// inventory reads everything the Integration API offers for the site
// and summarises it. This is phase 1's deliverable and doubles as the
// operator's "is my configuration right?" check.
func inventory(
	ctx context.Context,
	client *integration.Client,
	cfg *config.Config,
	site model.Site,
	logger *slog.Logger,
) error {
	devices, err := client.Devices(ctx, site.ID)
	if err != nil {
		return err
	}
	model.ResolveUplinks(devices)

	networks, err := client.Networks(ctx, site.ID)
	if err != nil {
		return err
	}
	wlans, err := client.WLANs(ctx, site.ID)
	if err != nil {
		return err
	}
	clients, err := client.Clients(ctx, site.ID)
	if err != nil {
		return err
	}

	logger.Info("unifi2mqtt.inventory",
		slog.Int("devices", len(devices)),
		slog.Int("clients", len(clients)),
		slog.Int("networks", len(networks)),
		slog.Int("wlans", len(wlans)))

	printDevices(devices)
	printNetworks(networks, wlans)
	printClients(clients, networks, cfg)
	return nil
}

func printDevices(devices []model.Device) {
	fmt.Printf("\nDEVICES (%d)\n", len(devices))
	for i := range devices {
		d := &devices[i]
		uplink := "—"
		if !d.UplinkMAC.IsZero() {
			uplink = d.UplinkMAC.Colon()
		}
		fmt.Printf("  %-18s %-20s %-16s %-10s fw %-10s uplink %s\n",
			d.MAC.Colon(), truncate(d.Name, 20), d.Model, d.State, d.Firmware, uplink)
	}
}

func printNetworks(networks []model.Network, wlans []model.WLAN) {
	fmt.Printf("\nNETWORKS (%d)\n", len(networks))
	for i := range networks {
		n := &networks[i]
		subnets := make([]string, 0, len(n.Subnets))
		for _, s := range n.Subnets {
			subnets = append(subnets, s.String())
		}
		if len(subnets) == 0 {
			subnets = append(subnets, "—")
		}
		fmt.Printf("  vlan %-5d %-20s %-10s %s\n",
			n.VLAN, truncate(n.Name, 20), n.Management, strings.Join(subnets, ", "))
	}

	fmt.Printf("\nWLANS (%d)\n", len(wlans))
	for _, w := range wlans {
		state := "disabled"
		if w.Enabled {
			state = "enabled"
		}
		fmt.Printf("  %-24s %s\n", truncate(w.Name, 24), state)
	}
}

// printClients also reports how the configured VLAN/network filters
// would land, which is the point of running this before phase 4 turns
// clients into Home Assistant entities.
func printClients(clients []model.Client, networks []model.Network, cfg *config.Config) {
	fmt.Printf("\nCLIENTS (%d)\n", len(clients))

	byNetwork := map[string]int{}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Key() < clients[j].Key() })

	for i := range clients {
		c := &clients[i]
		netName, vlan := "—", "—"
		if n, ok := model.ResolveNetwork(c.IP, networks); ok {
			netName, vlan = n.Name, strconv.Itoa(n.VLAN)
			byNetwork[n.Name]++
		} else {
			byNetwork["(unmapped)"]++
		}

		id := c.MAC.Colon()
		if id == "" {
			id = truncate(c.ID, 18)
		}
		guest := ""
		if c.IsGuest {
			guest = " guest"
		}
		fmt.Printf("  %-18s %-20s %-9s %-15s vlan %-5s %s%s\n",
			id, truncate(c.Name, 20), c.Type, c.IP, vlan, netName, guest)
	}

	fmt.Printf("\nCLIENTS BY NETWORK\n")
	names := make([]string, 0, len(byNetwork))
	for n := range byNetwork {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %-24s %d\n", n, byNetwork[n])
	}

	if !cfg.Clients.Enable {
		fmt.Printf("\n  CLIENTS.ENABLE is off — no client would be published.\n")
	}
}

// warnOldVersion compares the console's application version against the
// floor this project is verified against. It only warns: an older
// console may work fine, it is simply untested, and refusing to start
// would be a worse answer than saying so.
func warnOldVersion(v string, logger *slog.Logger) {
	major, minor, ok := parseMajorMinor(v)
	if !ok {
		logger.Debug("unifi2mqtt.version_unparsed", slog.String("application_version", v))
		return
	}
	if major > minAppMajor || (major == minAppMajor && minor >= minAppMinor) {
		return
	}
	logger.Warn("unifi2mqtt.console_version_untested",
		slog.String("application_version", v),
		slog.String("verified_against", fmt.Sprintf("%d.%d+", minAppMajor, minAppMinor)),
		slog.String("note", "the Integration API may still work; report anything that does not"))
}

func parseMajorMinor(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// loadConfig finds and parses the daemon's YAML config. An explicit
// --config flag overrides the standard search order.
//
// A missing file is not fatal: the Home Assistant add-on and env-only
// `docker run` deployments drive every setting through UNIFI_* and ship
// no file at all. Validation still enforces the required values.
func loadConfig(explicit string, logger *slog.Logger) (*config.Config, error) {
	env := config.OSEnv{}
	path := explicit
	if path == "" {
		if located, ok := config.Locate(env); ok {
			path = located
		}
	}
	if path == "" {
		cfg, err := config.Load(strings.NewReader(""), env)
		if err != nil {
			return nil, err
		}
		logger.Info("unifi2mqtt.config_loaded", slog.String("path", "(environment only)"))
		return cfg, nil
	}
	logger.Info("unifi2mqtt.config_loaded", slog.String("path", path))
	return config.LoadFile(path, env)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
