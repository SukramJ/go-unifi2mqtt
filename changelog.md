# Version 1.0.0 (2026-08-14)

First release. A pure-Go daemon bridging a local UniFi Network
installation to MQTT, with Home Assistant auto-discovery.

Verified end to end against UniFi Network 10.5.67 on a 12-device site
with 121 clients.

## What it does

- **Devices** — state, reachability, uptime, CPU, memory, uplink rates,
  firmware and update-available, per-port link/speed/PoE, per-radio
  channel and TX retries.
- **Clients** — presence as `device_tracker`, IP, network and VLAN.
  Filtered by connection type, network, VLAN, SSID and MAC lists.
- **Site health** — WAN status, WAN IP, latency, throughput and client
  counts.
- **Controls** — restart a device, power-cycle a PoE port, authorize a
  guest, toggle the locate LED, block a client, switch an SSID.
- **Home Assistant discovery** — entities appear on their own, wired
  into the real topology: `via_device` links client → AP → switch →
  gateway.
- **Diagnostic web UI** — read-only status page, doubling as the
  add-on's Ingress panel.

Ships as a static binary, a distroless Docker image and a Home Assistant
add-on.

## Design decisions worth knowing

- **The official API comes first.** Everything runs on the documented
  Network Integration API (`X-API-KEY`, Network 10.5+). The undocumented
  classic API is opt-in and fills only the gaps it must: site health,
  per-client SSID and signal, PoE wattage, client blocking, WLAN
  toggles. If it fails, those capabilities switch off and the daemon
  keeps running — the difference between "site health is missing" and
  "the bridge is down".
- **Safe by default.** Clients, controls and the web UI are all off
  until you turn them on. A read-only UniFi admin is enough for the API
  key unless you enable controls.
- **The MAC is the identity.** Topics and Home Assistant `unique_id`s
  are keyed on the normalised MAC, never the API's UUIDs — those change
  on re-adopt or a controller restore and would orphan every entity
  along with its history.
- **Language changes nothing but labels.** `LANGUAGE: de` renames what
  you see; `entity_id`, `unique_id` and every topic stay English, so
  switching never re-creates an entity or loses history. Both
  `object_id` and `default_entity_id` are published, because Home
  Assistant removed the first in 2026.4 and does not honour the second
  reliably before that — with only one, some release derives the
  entity_id from the translated name.
- **Entity ids read like the network.** The seed is
  `<device>_<key>`, so an automation references
  `sensor.unifi_sw_har_cpu_utilization` rather than a MAC address.
- **Presence has a grace period.** A client stays `home` until absent
  for `AWAY_TIMEOUT` (default 300 s). Wireless clients vanish for a
  cycle while roaming between access points; flipping immediately makes
  every presence automation flap.
- **No optimistic state.** After a command the affected object is
  re-polled and the published state comes from the console, so a failed
  command snaps the entity back instead of lying.
- **Retained commands are ignored.** A stale `mosquitto_pub -r` would
  otherwise power-cycle a port every time the daemon starts.
- **Only changed values are published**, with a per-topic forced
  republish (`FORCE_REPUBLISH`, default 600 s). Measured on the
  reference site: 24 messages across three poll cycles instead of 1089.

## Requirements

- UniFi OS console or software controller, **Network 10.5+**.
- An MQTT broker.
- For the optional classic layer: a **local** UniFi admin account —
  not a Ubiquiti SSO login, and 2FA must be off.

## Notes for operators

- `unifi2mqtt --once` prints the full site inventory including each
  client's VLAN, without touching the broker. Use it to check filters
  before enabling client publication.
- `VERIFY_TLS` defaults to off because consoles serve a self-signed
  certificate on their LAN address. If you reach yours through a
  hostname with a trusted certificate, turn it on; `CA_FILE` is the
  other way to keep verification.
- Enabling controls means anyone who can publish to your broker can
  restart your network hardware. Use a broker ACL.
- Clients with no IP — about 14% on the reference site, mostly wired
  devices the console has not seen an address for — cannot match a VLAN
  filter and are skipped when one is set.

## Documentation

- [`README.md`](README.md) — quickstart, topic layout, security notes.
- [`config-template.yaml`](config-template.yaml) — every setting,
  documented inline.
- [`addon/DOCS.md`](addon/DOCS.md) — the Home Assistant add-on. Its
  configuration page is fully labelled in English and German.
- [`CONCEPT.md`](CONCEPT.md) — the design rationale, including why the
  API surface is split the way it is.
