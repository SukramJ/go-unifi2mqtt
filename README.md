# go-unifi2mqtt

[![ci](https://github.com/SukramJ/go-unifi2mqtt/actions/workflows/ci.yml/badge.svg)](https://github.com/SukramJ/go-unifi2mqtt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A pure-Go daemon that bridges a **local UniFi Network installation** to
**MQTT**, with **Home Assistant** auto-discovery.

It talks to the console over the **official UniFi Network Integration
API** — an API key, no cloud account, no traffic to Ubiquiti's servers —
and optionally the classic controller API for the handful of things the
official surface does not expose.

## What you get

Devices appear in Home Assistant on their own, wired into your actual
network topology: `via_device` links each client to its access point,
each AP to its switch, each switch to the gateway.

| Area | Entities | Source |
| --- | --- | --- |
| **Devices** | state, reachable, uptime, CPU, memory, uplink TX/RX, firmware, update available | Integration API |
| **Ports** | link, speed, PoE state · PoE watts | Integration API · classic |
| **Radios** | channel, TX retries | Integration API |
| **Clients** | presence (`device_tracker`), IP · SSID, signal | Integration API · classic |
| **Site** | WAN connectivity, WAN IP, latency, throughput, client counts | classic |
| **Controls** | restart device, power-cycle PoE port, authorize guest · locate LED, block client, toggle WLAN | Integration API · classic |

Clients are **off by default** and filtered by connection type,
network/VLAN, SSID and MAC lists — a busy network would otherwise create
hundreds of entities. Controls are off by default too.

## Requirements

- A UniFi OS console (UDM / UDM-Pro / UDR / UCG / UX / UniFi OS Server)
  or a standalone software controller.
- **UniFi Network 10.5+** for the official Integration API. Older
  versions may work but are untested.
- An MQTT broker (e.g. Mosquitto).

## Getting an API key

UniFi Network → **Settings → Control Plane → Integrations** → create a
key. It is displayed exactly once and inherits the creating admin's
permissions — a **read-only** admin is sufficient unless you enable the
control entities.

## Quickstart

### Home Assistant add-on

Add `https://github.com/SukramJ/go-unifi2mqtt` under **Settings →
Add-ons → Add-on Store → ⋮ → Repositories**, install *go-unifi2mqtt*,
fill in `unifi_host` and `unifi_api_key`, start. Details in
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
[releases page](https://github.com/SukramJ/go-unifi2mqtt/releases):

```sh
mkdir -p ~/.config/unifi2mqtt
cp config-template.yaml ~/.config/unifi2mqtt/config.yaml
$EDITOR ~/.config/unifi2mqtt/config.yaml

# Check the connection and see what the console reports:
./unifi2mqtt --once

# Then run it for real:
./unifi2mqtt
```

`--once` connects, resolves the site and prints every device, network,
WLAN and client — including which VLAN each client maps to, so you can
check your filters before turning client publication on. It needs no
broker.

## Configuration

[`config-template.yaml`](config-template.yaml) documents every key
inline. Each is also settable as an environment variable with the
`UNIFI_` prefix (`UNIFI_HOST`, `UNIFI_MQTT_SERVER`, …); env always wins
over the file, so an env-only deployment needs no config file at all.

Search order: `--config <path>` →
`$XDG_CONFIG_HOME/unifi2mqtt/config.yaml` →
`~/.config/unifi2mqtt/config.yaml`.

Misconfiguration is rejected at startup with a message naming the key.
In particular, a filter or control that needs the classic API layer
while it is disabled is an error rather than a silently dead entity: an
SSID filter with no SSID data would match nothing and publish *every*
client.

## MQTT topic layout

Rooted at `MQTT_TOPIC` (default `unifi`), keyed by site and MAC:

```
unifi/bridge/status                            online | offline   (retained LWT)
unifi/bridge/info                              {"site":…,"application_version":…}
unifi/bridge/error                             last non-fatal failure

unifi/<site>/health/wan/state                  ok | warning | error
unifi/<site>/health/wan/ip                     203.0.113.7
unifi/<site>/health/wan/latency_ms             12
unifi/<site>/health/clients/total              47

unifi/<site>/device/<mac>/state                ONLINE | OFFLINE | …
unifi/<site>/device/<mac>/cpu_utilization      12.5
unifi/<site>/device/<mac>/memory_utilization   48.0
unifi/<site>/device/<mac>/uptime               864000
unifi/<site>/device/<mac>/uplink_tx_bps        1048576
unifi/<site>/device/<mac>/firmware             7.0.25
unifi/<site>/device/<mac>/update_available     ON | OFF
unifi/<site>/device/<mac>/attributes           {"model":…,"uplink_mac":…}
unifi/<site>/device/<mac>/port/3/state         UP | DOWN
unifi/<site>/device/<mac>/port/3/poe           ON | OFF
unifi/<site>/device/<mac>/port/3/poe/power_w   7.4
unifi/<site>/device/<mac>/radio/5g/channel     36

unifi/<site>/client/<mac>/state                home | not_home
unifi/<site>/client/<mac>/ip                   192.168.1.42
unifi/<site>/client/<mac>/signal               -52
unifi/<site>/client/<mac>/attributes           {"ssid":…,"vlan":…}

unifi/<site>/wlan/<id>/enabled                 ON | OFF
unifi/<site>/wlan/<id>/name                    HomeNet
```

Command topics, only with `CONTROLS.ENABLE`:

```
unifi/<site>/device/<mac>/cmd/restart            ← any payload
unifi/<site>/device/<mac>/cmd/locate/set         ← ON | OFF
unifi/<site>/device/<mac>/port/3/cmd/power_cycle ← any payload
unifi/<site>/client/<mac>/blocked/set            ← ON | OFF
unifi/<site>/client/<mac>/cmd/authorize          ← {"minutes":60} or empty
unifi/<site>/wlan/<id>/enabled/set               ← ON | OFF
```

Discovery configs are retained too, and stale ones are cleared on
start: the daemon reads back what is retained under the discovery
prefix and removes the entities it owns but no longer publishes. It
identifies its own by `unique_id` prefix *and* availability topic, so a
second instance on the same broker is left alone. `HASS_CLEANUP: false`
disables it.

State topics are retained, command topics are not. **Retained commands
are ignored on purpose**: a stale `mosquitto_pub -r` would otherwise
power-cycle a port every time the daemon starts.

The full specification, including why each choice was made, is in
[`CONCEPT.md`](CONCEPT.md#5-mqtt-topic-layout).

## Diagnostic web UI

`WEB_ENABLE: true` serves a small read-only page: the broker link, every
poll loop with its age, which classic capabilities are live, site
health, all devices with statistics and PoE draw, and the published
clients. In the Home Assistant add-on it is the sidebar panel.

It binds to `127.0.0.1:8080` by default. Set `WEB_USER` and
`WEB_PASSWORD` before binding it anywhere else.

## Security notes

- With controls off, a **read-only** UniFi admin is enough for the API
  key. That is the recommended setup.
- `VERIFY_TLS` is off by default because consoles serve a self-signed
  certificate on their LAN address. `CA_FILE` is the better fix: it
  keeps verification on and trusts the console's own certificate.
- **Whoever can publish to your broker can restart your network
  hardware** once controls are enabled. Use a broker ACL.
- Credentials never reach a log line, an error message, the web UI or an
  MQTT payload.

## Development

```sh
make setup   # tooling + git hooks
make check   # vet + gofumpt + golangci-lint + race tests
make build   # -> bin/unifi2mqtt
```

Contributions must pass the same gates regardless of origin — see
[`AI_POLICY.md`](AI_POLICY.md) for the rules on AI-assisted
contributions, and [`CONCEPT.md`](CONCEPT.md) for the design rationale
behind anything that looks arbitrary.

## Related projects

- [go-mtec2mqtt](https://github.com/SukramJ/go-mtec2mqtt) — the
  structural template for this repository (M-TEC inverter → MQTT).
- [go-mqtt](https://github.com/SukramJ/go-mqtt) — the dependency-free
  MQTT 3.1.1 / 5.0 client used here.

## License

MIT — see [LICENSE](LICENSE).

This project is not affiliated with, endorsed by, or sponsored by
Ubiquiti Inc. "UniFi" is a trademark of Ubiquiti Inc.
