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
// Build status: phase 2 of CONCEPT.md §12. The daemon polls the console
// and publishes devices, ports, radios and the WLAN catalogue to MQTT
// with change detection and a retained availability topic. Home
// Assistant discovery is phase 3 and clients are phase 4, so entities
// still have to be wired up by hand for now.
//
// `--once` skips all of that and just reports the site inventory, which
// is the fastest way to check credentials and filters against a real
// console.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/SukramJ/go-mqtt"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
	"github.com/SukramJ/go-unifi2mqtt/internal/coordinator"
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

const (
	// keepAlive is the MQTT keep-alive interval.
	keepAlive = 60 * time.Second
	// shutdownTimeout bounds the graceful disconnect so a hung broker
	// cannot hold up a systemd stop.
	shutdownTimeout = 3 * time.Second
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
	// A --once run never opens a broker connection, so demanding
	// MQTT_SERVER would only push operators towards a placeholder that
	// later gets forgotten in a real config.
	var opts []config.Option
	if once {
		opts = append(opts, config.WithoutMQTT())
	}
	cfg, err := loadConfig(configPath, logger, opts...)
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

	client, site, info, err := connect(ctx, cfg, logger)
	if err != nil {
		return err
	}

	if once {
		return inventory(ctx, client, cfg, site, logger)
	}
	return bridge(ctx, cfg, client, site, info, logger)
}

// connect builds the console client, probes the API and resolves the
// configured site.
func connect(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
) (*integration.Client, model.Site, model.ControllerInfo, error) {
	tr, err := unifi.NewTransport(unifi.TransportConfig{
		BaseURL:   cfg.BaseURL(),
		Timeout:   cfg.HTTPTimeoutDuration(),
		Retries:   cfg.HTTPRetries,
		VerifyTLS: cfg.VerifyTLS,
		CAFile:    cfg.CAFile,
		Logger:    logger,
	})
	if err != nil {
		return nil, model.Site{}, model.ControllerInfo{}, err
	}

	client := integration.New(integration.Config{
		Transport: tr,
		APIKey:    cfg.APIKey.Reveal(),
		Logger:    logger,
	})

	info, err := client.Probe(ctx)
	if err != nil {
		return nil, model.Site{}, model.ControllerInfo{}, fmt.Errorf("unifi2mqtt: connect to %s: %w", cfg.BaseURL(), err)
	}
	logger.Info("unifi2mqtt.console",
		slog.String("host", cfg.Host),
		slog.String("application_version", info.ApplicationVersion))
	warnOldVersion(info.ApplicationVersion, logger)

	site, err := client.ResolveSite(ctx, cfg.Site)
	if err != nil {
		return nil, model.Site{}, model.ControllerInfo{}, err
	}
	logger.Info("unifi2mqtt.site",
		slog.String("name", site.Name),
		slog.String("reference", site.Internal),
		slog.String("id", site.ID))

	return client, site, info, nil
}

// bridge runs the daemon proper: it opens the broker session, wires the
// availability topic to the MQTT will, and hands both transports to the
// coordinator.
func bridge(
	ctx context.Context,
	cfg *config.Config,
	client *integration.Client,
	site model.Site,
	info model.ControllerInfo,
	logger *slog.Logger,
) error {
	// The coordinator owns the topic layout, so it also decides where
	// availability lives — main only needs the string to build the will.
	// The MQTT client is built after the coordinator (it needs the will
	// topic the coordinator owns), so the subscriber is handed over with
	// SetSubscriber once it exists.
	c := coordinator.New(coordinator.Deps{
		Cfg:    cfg,
		Site:   site,
		Source: client,
		Info:   info,
		Logger: logger,
	})
	lwtTopic := c.AvailabilityTopic()

	var tlsConfig *tls.Config
	if cfg.MQTTSSL {
		tlsConfig = mqtt.NewClientTLSConfig(cfg.MQTTServer, cfg.MQTTSSLInsecure)
	}
	mqttClient := mqtt.NewTCPClient(mqtt.TCPConfig{
		BrokerURL:  cfg.MQTTBrokerURL(),
		ClientID:   cfg.ClientID(),
		Username:   cfg.MQTTLogin,
		Password:   cfg.MQTTPassword.Reveal(),
		KeepAlive:  keepAlive,
		CleanStart: true,
		// The will covers ungraceful death; the OnConnect hook below
		// publishes the matching birth, and the shutdown path
		// re-publishes "offline" because a clean DISCONNECT suppresses
		// the will. Without the birth, one network blip would leave the
		// retained topic stuck at "offline" for the rest of the run.
		Will: &mqtt.Will{
			Topic:   lwtTopic,
			Payload: []byte("offline"),
			Retain:  true,
		},
		TLSConfig: tlsConfig,
		Logger:    logger,
	})

	// The circuit breaker addresses the case the reconnect loop cannot
	// see: TCP up, but the broker stopped acknowledging. Without it
	// every publish would block for the full ack timeout, and a poll
	// cycle publishing hundreds of topics would stall for minutes.
	breaker := mqtt.NewBreaker(mqttClient, mqtt.BreakerConfig{
		OnStateChange: func(from, to mqtt.BreakerState) {
			logger.Warn("unifi2mqtt.mqtt_breaker_state",
				slog.String("from", from.String()),
				slog.String("to", to.String()))
		},
	})
	// Must happen before Start: the lifecycle calls OnConnect from
	// inside its first connect, and that hook publishes.
	c.SetPublisher(breaker)
	// Subscriptions go to the client directly rather than through the
	// breaker: they are startup-path calls with their own SUBACK-bounded
	// wait, and must not be rejected during a publish-side brownout.
	c.SetSubscriber(mqttClient)

	lifecycle := mqtt.NewLifecycle(mqtt.DefaultLifecycle(), mqttClient)
	lifecycle.OnConnect(c.OnConnect)

	if err := startMQTT(ctx, lifecycle, time.Second, 30*time.Second, logger); err != nil {
		return fmt.Errorf("unifi2mqtt: mqtt start: %w", err)
	}
	logger.Info("unifi2mqtt.mqtt_connected",
		slog.String("broker", cfg.MQTTBrokerURL()),
		slog.String("client_id", cfg.ClientID()),
		slog.String("topic_root", cfg.MQTTTopic))

	//nolint:contextcheck // disconnect deliberately uses a fresh context;
	// the run context is already cancelled when this defer fires.
	defer disconnect(c, lifecycle, logger)

	return c.Run(ctx)
}

// disconnect announces the bridge offline and closes the broker session.
//
// It deliberately builds a fresh context instead of taking the run
// context: by the time this runs the run context is already cancelled,
// so reusing it would abort both calls before they reach the broker and
// leave the retained availability topic stuck at "online" until the
// keep-alive expires. The fresh context is bounded so a hung broker
// cannot hold up a systemd stop either.
func disconnect(c *coordinator.Coordinator, lc *mqtt.Lifecycle, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	c.AnnounceOffline(ctx)
	if err := lc.Stop(ctx); err != nil {
		logger.Debug("unifi2mqtt.mqtt_stop", slog.String("err", err.Error()))
	}
}

// mqttStarter is the subset of *mqtt.Lifecycle that startMQTT drives,
// narrowed so tests can inject first-connect failures.
type mqttStarter interface {
	Start(ctx context.Context) error
}

// startMQTT retries the lifecycle's synchronous first connect with
// bounded backoff.
//
// Lifecycle.Start makes exactly one attempt and only runs its reconnect
// loop after that first success, so a broker that is still booting —
// the power-outage case, where the daemon and the broker start together
// — must be retried here instead of being treated as a fatal
// configuration error.
func startMQTT(ctx context.Context, lc mqttStarter, backoff, maxBackoff time.Duration, logger *slog.Logger) error {
	for {
		err := lc.Start(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn("unifi2mqtt.mqtt_start_retry",
			slog.String("err", err.Error()),
			slog.Duration("retry_in", backoff))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
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
	// The detail-aware call: the device list alone carries no uplink,
	// ports or radios, so a plain Devices() here would report every
	// device as having no uplink.
	devices, err := client.DevicesWithDetails(ctx, site.ID)
	if err != nil {
		return err
	}

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
			id, truncate(c.Name, 20), c.Type, addr(c.IP), vlan, netName, guest)
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
func loadConfig(explicit string, logger *slog.Logger, opts ...config.Option) (*config.Config, error) {
	env := config.OSEnv{}
	path := explicit
	if path == "" {
		if located, ok := config.Locate(env); ok {
			path = located
		}
	}
	if path == "" {
		cfg, err := config.Load(strings.NewReader(""), env, opts...)
		if err != nil {
			return nil, err
		}
		logger.Info("unifi2mqtt.config_loaded", slog.String("path", "(environment only)"))
		return cfg, nil
	}
	logger.Info("unifi2mqtt.config_loaded", slog.String("path", path))
	return config.LoadFile(path, env, opts...)
}

// addr renders an address for the inventory table. netip prints an
// unset address as "invalid IP", which reads like a malformed value
// rather than "the console reported none".
func addr(a netip.Addr) string {
	if !a.IsValid() {
		return "—"
	}
	return a.String()
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
