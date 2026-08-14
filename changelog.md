# Version 1.0.1 (2026-08-14)

A fix release for two things a German Home Assistant install surfaced,
plus the cleanup that makes stale entities disappear on their own.

## Entity ids stay English

Discovery seeded the entity_id with `object_id` alone. Home Assistant
Core deprecated that key in 2025.10 and removed it in 2026.4, so on a
current release nothing seeded the id and HA derived it from the
display name — which is translated. A `de` install ended up with
`sensor.cpu_auslastung`.

Both `object_id` and `default_entity_id` are published now, because
neither works everywhere on its own: the first is gone in current
releases, the second is not honoured reliably in older ones. The seed
is also readable rather than a MAC —
`sensor.unifi_sw_har_cpu_utilization`, not
`sensor.unifi_f492bf8394ba_cpu_utilization` — and slugs the way Home
Assistant itself does, so an umlaut loses its diaeresis (`Süd` → `sud`)
instead of expanding to `sued` and disagreeing with the id HA would
derive for the same name.

`unique_id` is unchanged, so the update orphans nothing. It also means
entities that already exist keep the ids they were given: Home
Assistant keys them by `unique_id`, so no update can rename them.
Delete the affected entities under Settings → Devices & Services →
MQTT and restart to have them recreated with English ids.

## Orphaned entities are removed

Until now the daemon could only clear entities it had seen appear and
disappear within one run. A discovery config left behind by an earlier
version, by a device removed while the daemon was stopped, or by a
filter that no longer matches, stayed retained on the broker — so Home
Assistant recreated the entity on every start and it sat there
unavailable forever, with nothing to say where it came from.

On start the daemon now reads the retained configs under the discovery
prefix and clears the ones it owns but no longer publishes. Ownership
requires two independent signals to agree: the `unique_id` has to be in
this project's namespace **and** the payload has to name this bridge's
availability topic. A second instance bridging another console to the
same broker under its own `MQTT_TOPIC` is therefore correctly seen as
somebody else's, and another integration's entities are never touched.

The sweep waits for each source to report before it will remove
anything of that kind. An empty announced set means "not polled yet"
until the device, client, WLAN or health poll says otherwise —
publishing nothing because a poll failed must never read as "these
entities are gone". A source that is switched off is the exception, and
deliberately so: turning `CLIENTS.ENABLE` off is a decision that those
entities should disappear.

`HASS_CLEANUP: false` (add-on: **Remove orphaned entities**) turns it
off.

## Add-on options are labelled

The add-on configuration page rendered every option as its raw
identifier — `controls_port_power_cycle` with no hint of what it does —
because `config.yaml` carries no labels and no translation files
existed. All 40 options now have a name and a description in English
and German.

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
