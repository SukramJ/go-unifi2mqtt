# go-unifi2mqtt

[![ci](https://github.com/SukramJ/go-unifi2mqtt/actions/workflows/ci.yml/badge.svg)](https://github.com/SukramJ/go-unifi2mqtt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A pure-Go daemon that bridges a **local UniFi Network installation** to
**MQTT**, with optional **Home Assistant** MQTT auto-discovery.

It talks to the console over the **official UniFi Network Integration API**
(API-key authenticated, no cloud account involved) and — optionally — the
classic controller API for the handful of things the official surface does
not expose.

> **Status: early development (phase 2 of 9).** The daemon polls the
> console and publishes devices, ports, radios and the WLAN catalogue to
> MQTT with change detection and a retained availability topic. Home
> Assistant discovery is phase 3 and clients are phase 4, so entities
> still have to be configured by hand for now. The roadmap and the design
> rationale live in [`CONCEPT.md`](CONCEPT.md), the design source of truth.

## What it publishes

| Area              | Contents                                                                                                   | Source            |
| ----------------- | ---------------------------------------------------------------------------------------------------------- | ----------------- |
| **Devices**       | State, model, firmware + update-available, uptime, CPU / memory, uplink rates, per-port link + PoE, radios | Integration API   |
| **Clients**       | Presence (`home` / `not_home`), IP, hostname, connection type, SSID / VLAN / network, signal, uplink device | Integration API   |
| **Site health**   | WAN up/down, WAN IP, latency, throughput, client counts, per-subsystem status                              | Classic API       |
| **Controls**      | Restart device, power-cycle PoE port, locate LED, block client, toggle WLAN, authorize guest               | both              |

Clients are **off by default** and filtered by connection type, network/VLAN,
SSID and MAC allow/denylist — a busy network would otherwise create hundreds
of Home Assistant entities. See [`config-template.yaml`](config-template.yaml).

## Requirements

- A UniFi OS console (UDM / UDM-Pro / UDR / UCG / UX / UniFi OS Server) or a
  standalone software controller.
- **UniFi Network 10.5+** for the official Integration API (older versions may work but are untested).
- An MQTT broker (e.g. Mosquitto).

## Getting an API key

UniFi Network → **Settings → Control Plane → Integrations** → create a key.
It is displayed exactly once and inherits the creating admin's permissions —
a **read-only** admin is sufficient unless you enable the control entities.

## Quickstart

### Home Assistant add-on

Add `https://github.com/SukramJ/go-unifi2mqtt` under **Settings → Add-ons →
Add-on Store → ⋮ → Repositories**, install *go-unifi2mqtt*, fill in
`unifi_host` and `unifi_api_key`, start. Details in
[`addon/DOCS.md`](addon/DOCS.md).

### Docker

```sh
docker run -d --name unifi2mqtt \
  -e UNIFI_HOST=192.168.1.1 \
  -e UNIFI_API_KEY=... \
  -e UNIFI_MQTT_SERVER=192.168.1.10 \
  -e UNIFI_HASS_ENABLE=true \
  ghcr.io/sukramj/go-unifi2mqtt:latest
```

Or mount a config file:

```sh
docker run -d --name unifi2mqtt \
  -v "$PWD/my-config:/config:ro" \
  ghcr.io/sukramj/go-unifi2mqtt:latest
```

### Plain binary

Download a release archive from the
[releases page](https://github.com/SukramJ/go-unifi2mqtt/releases), then:

```sh
mkdir -p ~/.config/unifi2mqtt
cp config-template.yaml ~/.config/unifi2mqtt/config.yaml
$EDITOR ~/.config/unifi2mqtt/config.yaml

# Verify the connection and see what the console reports:
./unifi2mqtt --once
```

`--once` connects, resolves the site and prints every device, network,
WLAN and client — including which VLAN each client maps to, so you can
check `CLIENTS.VLANS` / `CLIENTS.NETWORKS` before turning client
publication on.

## Configuration

[`config-template.yaml`](config-template.yaml) documents every key inline.
Each key is also settable as an environment variable with the `UNIFI_`
prefix (`UNIFI_HOST`, `UNIFI_MQTT_SERVER`, …); env always wins over the file,
so an env-only deployment needs no config file at all.

The search order is `--config <path>` →
`$XDG_CONFIG_HOME/unifi2mqtt/config.yaml` →
`~/.config/unifi2mqtt/config.yaml`.

## MQTT topic layout

The topic tree is rooted at `MQTT_TOPIC` (default `unifi`) and keyed by site
and MAC address. The full specification lives in
[`CONCEPT.md`](CONCEPT.md#5-mqtt-topic-layout); the shape is:

```
unifi/bridge/status                              online | offline (retained LWT)
unifi/<site>/health/wan/state                    ok | warning | error
unifi/<site>/device/<mac>/state                  ONLINE | OFFLINE | ...
unifi/<site>/device/<mac>/cpu_utilization        42.5
unifi/<site>/device/<mac>/port/3/poe/state       on | off
unifi/<site>/device/<mac>/port/3/poe/set    <-   power_cycle
unifi/<site>/client/<mac>/state                  home | not_home
unifi/<site>/client/<mac>/blocked/set       <-   ON | OFF
```

## Development

```sh
make setup   # tooling + git hooks
make check   # vet + gofumpt + golangci-lint + race tests
make build   # -> bin/unifi2mqtt
```

Contributions must pass the same gates regardless of origin — see
[`AI_POLICY.md`](AI_POLICY.md) for the rules on AI-assisted contributions.

## Related projects

- [go-mtec2mqtt](https://github.com/SukramJ/go-mtec2mqtt) — the structural
  template for this repository (M-TEC inverter → MQTT).
- [go-mqtt](https://github.com/SukramJ/go-mqtt) — the dependency-free MQTT
  3.1.1 / 5.0 client used here.

## License

MIT — see [LICENSE](LICENSE).

This project is not affiliated with, endorsed by, or sponsored by Ubiquiti
Inc. "UniFi" is a trademark of Ubiquiti Inc.
