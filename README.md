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

Since 2.0.0 the tree follows the
[mqtt-smarthome 2.0](https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md)
convention, `<name>/<function>/<item...>` (openccu-loom ADR 0083). The
instance name is `MQTT_TOPIC` (default `unifi`); below `status` and
`set` the items are keyed by site and MAC:

```
unifi/connected                                       0 | 1 | 2        (retained, Last Will 0)
unifi/info                                            {"name":"go-unifi2mqtt","version":…,"spec":"2.0",…}
unifi/status/bridge/error                             {"val":{"loop":…,"error":…},…}   (not retained)

unifi/status/<site>/health/wan/state                  ok | warning | error
unifi/status/<site>/health/wan/ip                     203.0.113.7
unifi/status/<site>/health/wan/latency_ms             12
unifi/status/<site>/health/clients/total              47

unifi/status/<site>/device/<mac>/state                ONLINE | OFFLINE | …
unifi/status/<site>/device/<mac>/online               true | false
unifi/status/<site>/device/<mac>/cpu_utilization      12.5
unifi/status/<site>/device/<mac>/memory_utilization   48
unifi/status/<site>/device/<mac>/uptime               864000
unifi/status/<site>/device/<mac>/uplink_tx_bps        1048576
unifi/status/<site>/device/<mac>/firmware             7.0.25
unifi/status/<site>/device/<mac>/update_available     true | false
unifi/status/<site>/device/<mac>/attributes           {"model":…,"uplink_mac":…}
unifi/status/<site>/device/<mac>/port/3/state         UP | DOWN
unifi/status/<site>/device/<mac>/port/3/poe           true | false
unifi/status/<site>/device/<mac>/port/3/poe/power_w   7.4
unifi/status/<site>/device/<mac>/radio/5g/channel     36

unifi/status/<site>/client/<mac>/state                home | not_home
unifi/status/<site>/client/<mac>/online               true | false
unifi/status/<site>/client/<mac>/ip                   192.168.1.42
unifi/status/<site>/client/<mac>/signal               -52
unifi/status/<site>/client/<mac>/attributes           {"ssid":…,"vlan":…}

unifi/status/<site>/wlan/<id>/enabled                 true | false
unifi/status/<site>/wlan/<id>/name                    HomeNet
```

The values above are each item's `val`. Every status item is published
as a status object, never as a plain value:

```json
{"val": 12.5, "ts": 1760000000000, "lc": 1759999940000}
```

`ts` is when the value was observed and `lc` when it last changed, both
integer milliseconds. Booleans are JSON booleans, numbers JSON numbers,
states their English token. A value is published when it changes and
again, unchanged, after every broker reconnect — never periodically. An
empty retained payload means "no value" (an absent client's `ip`).

`unifi/connected` is `0` when the daemon is gone (Last Will, and on a
clean stop), `1` while it is connected to the broker but the console
does not answer its device poll, and `2` while it does. A device's or
client's own reachability is its `online` item. Home Assistant entities
are available while `connected` is 2 and, where the entity has one,
the object's `online` is true.

Command items, only with `CONTROLS.ENABLE`, on the same item path under
`set`. A plain value or `{"val": …}` is accepted; switches take
`true/false`, `on/off`, `1/0` or `yes/no` in any case and reject
anything else; the `cmd/…` actions fire on any non-empty payload.
Empty and retained messages are ignored, and a rejected or failed
request is logged at warn with its topic and payload:

```
unifi/set/<site>/device/<mac>/cmd/restart             ← any non-empty payload
unifi/set/<site>/device/<mac>/cmd/locate              ← true | false
unifi/set/<site>/device/<mac>/port/3/cmd/power_cycle  ← any non-empty payload
unifi/set/<site>/client/<mac>/blocked                 ← true | false
unifi/set/<site>/client/<mac>/cmd/authorize           ← {} or {"minutes":60}
unifi/set/<site>/wlan/<id>/enabled                    ← true | false
```

`bridge` sits beside the sites as a literal item, so a site whose
reference is `bridge`, `alarm`, `security`, `system` or one of the
function names (`connected`, `status`, `set`, `get`, `info`, `meta`,
`maintenance`) is refused at start.

### `MQTT_TOPIC`: the instance name

`MQTT_TOPIC` is the only thing that keeps two instances apart on one
broker, and nothing checks it. Two consoles — or two sites of one
console — bridged to the same broker need two different names, or they
overwrite each other's `connected`, `info` and status items. The
default `unifi` is also the default instance name of hobbyquaker's
Node.js adapter [unifi2mqtt](https://github.com/hobbyquaker/unifi2mqtt);
running both on one broker needs one of them renamed.

### Upgrading from 1.x

2.0.0 is a clean break, with no compatibility switch. Home Assistant
users need to do nothing: `unique_id`s, device identifiers and discovery
config topics are unchanged, and the entities re-point to the new
topics by themselves. Anything that reads the raw topics — Node-RED
flows, dashboards, scripts — has to move:

| 1.x | 2.0 |
|---|---|
| `unifi/<site>/…/<key>` = `12.5` | `unifi/status/<site>/…/<key>` = `{"val":12.5,"ts":…,"lc":…}` |
| `ON` / `OFF` | `true` / `false` |
| `unifi/bridge/status` = `online` / `offline` | `unifi/connected` = `0` / `1` / `2` |
| `unifi/bridge/info` | `unifi/info` (`bridge_version` → `version`, `host` → `console_host`) |
| `unifi/bridge/error` | `unifi/status/bridge/error` (still not retained) |
| — | `unifi/status/<site>/device/<mac>/online`, `…/client/<mac>/online` |
| `…/device/<mac>/cmd/restart` | `unifi/set/<site>/device/<mac>/cmd/restart` |
| `…/device/<mac>/cmd/locate/set` | `unifi/set/<site>/device/<mac>/cmd/locate` |
| `…/device/<mac>/port/<p>/cmd/power_cycle` | `unifi/set/<site>/device/<mac>/port/<p>/cmd/power_cycle` |
| `…/client/<mac>/blocked/set` | `unifi/set/<site>/client/<mac>/blocked` |
| `…/client/<mac>/cmd/authorize` (empty payload allowed) | `unifi/set/<site>/client/<mac>/cmd/authorize` (needs `{}` or `{"minutes": n}`) |
| `…/wlan/<id>/enabled/set` | `unifi/set/<site>/wlan/<id>/enabled` |
| every value republished every `FORCE_REPUBLISH` s | on change and on reconnect only; the key is ignored |

On every start the daemon clears what 1.x left retained under its own
name: `unifi/bridge/status`, `unifi/bridge/info`, the site's health
items, and the 1.x items of every device, client and SSID it has polled
from its own console in this run — exact 1.x shapes only, never a
prefix, never a topic whose second level is a function name. An object
this run does not see keeps its old topics until a later start does.
A site whose reference is now reserved (see above) stops the daemon
with a message naming `SITE`.

### Maintenance topics

On by default (`MQTT_MAINTENANCE: false` turns them off), per spec §7:

```
unifi/maintenance/set/loglevel   ← error | warn | info | debug   (not persisted)
unifi/maintenance/set/restart    ← any                            (graceful stop, exit 0)
unifi/maintenance/stats          {"rss":…,"heapUsed":…,"cpu":…,"uptime":…,"ts":…}  every MQTT_STATS_INTERVAL s (0 = off)
```

`restart` exits cleanly and relies on something starting the daemon
again, so it is honoured only when the daemon knows it is supervised:
`UNIFI_SUPERVISED=1` (or `0` to refuse), otherwise detected — systemd,
Kubernetes, a container. A container without a restart policy is
detected as supervised too, and a restart then stops it for good; set
`UNIFI_SUPERVISED=0` there. The Home Assistant add-on refuses it (see
its documentation).

**Security.** Anyone allowed to publish on the broker can change the
log level and restart the daemon. Give the daemon's broker user an ACL
for `unifi/#` and the discovery prefix and nothing else, give consumers
publish rights only where they need them, and on a broker that cannot
be secured set `MQTT_MAINTENANCE: false`.

### Discovery

Discovery configs are retained too. On start the daemon reads back what
is retained under the discovery prefix and clears the orphans among
them — but only ones **it published itself during this run** and has
since given up. A retained config it did not publish is never cleared,
however exactly it matches this daemon's own shape: two instances
bridging two different UniFi consoles to one broker under one
`MQTT_TOPIC` emit byte-identical config topics, `unique_id`s,
availability topics and state topics for the whole site plane, so
"looks like mine" would delete the other console's entities. Those
configs are logged with their topics, in one of two lines. A config
whose source reported on this run is logged as
`coordinator.reconcile_unclaimed`, for an operator who runs no second
console to clear with one empty retained publish each. A config whose
source did **not** report — a console unreachable at start-up, rotated
credentials, a controller mid-upgrade — is logged as a warning,
`coordinator.reconcile_unclaimed_unready`, and is explicitly **not**
safe to clear: it may be this daemon's own live entity, missing from the
announced set only because nothing announced it yet. Clearing that one
deletes a live entity and its history. `HASS_CLEANUP: false` disables the
sweep entirely.

One retained config per entity is a deliberate choice, not a leftover:
Home Assistant also accepts a single "device bundle" config per device,
and this bridge measured that form, built it and declined to publish it
— two consoles that cannot be told apart would replace each other's
whole entity set instead of overwriting it entity by entity. The
reasoning, and the condition under which that could change, is in
[`notes/adr0070-phase9-measurement.md`](notes/adr0070-phase9-measurement.md).

Status items are retained, command items are not. **Retained commands
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
  hardware** once controls are enabled, and restart this daemon or
  change its log level through the maintenance topics. Use a broker
  ACL; see [Maintenance topics](#maintenance-topics).
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
