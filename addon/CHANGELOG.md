# Version 0.1.0 (2026-08-14)

Initial project scaffold plus the console client (phase 1 of
[`CONCEPT.md`](CONCEPT.md) §12). The daemon connects to a UniFi console
and reports the site inventory; MQTT publication is phase 2.

## Added

- Project setup adopted from
  [go-mtec2mqtt](https://github.com/SukramJ/go-mtec2mqtt): `Makefile`,
  `.golangci.yaml` (golangci-lint v2), `.githooks/pre-commit`, distroless
  `Dockerfile`, `.dockerignore`, `AI_POLICY.md`, `CLAUDE.md`.
- GitHub project setup: `ci.yml` (lint / test matrix / build),
  `codeql.yml`, `dependabot.yml` + `dependabot-auto-merge.yml`,
  `release-on-tag.yml`, `docker-build-push.yml`, `addon-image.yml`,
  plus branch protection, repository topics and description.
- Home Assistant add-on packaging under `addon/` plus `repository.yaml`
  and `script/run.sh`.
- `CONCEPT.md` — the implementation concept: UniFi API strategy
  (official Network Integration API with an optional classic-API
  fallback), package layout, MQTT topic tree, Home Assistant entity
  model, client filtering, polling design and the phased roadmap.
- `internal/model` — API-neutral domain types. A dedicated `MAC` type
  with a single parsing entry point makes the normalised address the
  project's canonical identity, `ResolveNetwork` maps a client IP onto
  its VLAN by longest-prefix match, and `ResolveUplinks` turns the API's
  uplink UUIDs into MACs.
- `internal/config` — YAML loader, `UNIFI_*` env overlay, defaults and
  validation. Secrets use a `Secret` type that redacts through
  `fmt`, `encoding/json` and `log/slog`. Validation rejects filters and
  controls that need the classic API layer while it is disabled, since
  those would otherwise fail silently and invert the operator's intent.
- `internal/unifi` — shared HTTP layer: TLS handling (including
  `CA_FILE`), bounded retry with jitter, `Retry-After` support, error
  mapping onto sentinels, and URL stripping so nothing leaks into logs.
- `internal/unifi/integration` — the official Network Integration API v1
  client: console flavour probing, pagination, sites, devices, per-device
  statistics, clients, networks, WLANs and the three actuators the
  official API exposes.
- `unifi2mqtt --once` — connects, resolves the site and prints the full
  inventory including which VLAN each client maps to. Also the quickest
  way to verify credentials and filters against a real console.
- `script/capture-fixtures.sh` — captures API responses from an
  operator's own console as test fixtures, redacting MACs, IPs, names
  and UUIDs consistently so relationships survive.

## Notes

- The API surface is verified against the official OpenAPI documents of
  UniFi Network 10.5.67 and 10.6.90, which are identical for every
  schema this project reads. That fixes the supported floor at **10.5**;
  older consoles are warned about, not refused.
- Three assumptions from the first concept draft were corrected during
  that verification: the network/VLAN and WLAN catalogues *are*
  officially available, VLAN filtering therefore needs no classic API,
  and the device uplink is reported as a UUID rather than a MAC.

## Fixed during live verification

Running phase 1 against a real UniFi Network 10.5.67 console caught two
bugs that would have silently produced empty data rather than failing:

- **Networks came back without subnets**, making client→VLAN mapping
  resolve to nothing for every client. `GET /networks` does not return
  `ipv4Configuration`; only `GET /networks/{id}` does.
- **Devices came back without an uplink**, which would have collapsed
  Home Assistant's `via_device` topology into a flat list. `GET /devices`
  carries neither `uplink` nor `interfaces`.

Both list endpoints now fan out one detail call per object, bounded to 4
concurrent requests, and a failing detail call degrades to the overview
data instead of dropping the object. `CONCEPT.md` §2.2 and §8.2 record
the split and its cost: device and network details belong on the hourly
`static` loop, not the 60-second device loop.

## Added in phase 2 — MQTT publication

- `internal/coordinator` — the poll loops and the publication path.
  Devices, ports, radios and the WLAN catalogue are published as scalar
  topics under `unifi/<site>/…`, with a retained `unifi/bridge/status`
  wired to the MQTT will so Home Assistant sees the bridge go away.
- **Change detection.** Only values that actually changed are published,
  with a per-topic forced republish (`FORCE_REPUBLISH`, default 600 s)
  so a subscriber that missed a message cannot stay stale. Measured
  against the reference installation: 24 messages across three poll
  cycles instead of 1089.
- **Loop separation by change rate.** Device details (ports, radios,
  uplinks) and the network/WLAN catalogues sit on the hourly `static`
  loop; only the device list and per-device statistics run on the fast
  cadence. That turns a 1+2N request budget per minute into 1+N_online.
- `config.WithoutMQTT()` for `--once`, which never opens a broker
  connection.

### Fixed during live verification against a 10.5.67 console

- **Gateways were classified as switches.** The API returns display
  names with spaces (`UCG Fiber`), not the hyphenated product codes the
  documentation uses, so prefix matching on `UCG-` never fired. Model
  classification now matches the leading token regardless of separator.
- **`FORCE_REPUBLISH` republished exactly one topic per interval.** The
  deadline was global, so whichever publish ran first after it expired
  consumed it and every other topic stayed suppressed. It is now tracked
  per topic, which also staggers the forced traffic instead of bunching
  it.
- **A nil-pointer panic on the first broker connect.** The MQTT
  lifecycle invokes its connect hook from inside `Start()`, so the hook
  ran before the publisher was wired in. The wiring order is fixed and
  the publisher now returns an error rather than dereferencing nil.
- The bridge info topic no longer carries publish counters that always
  read 1, because it is written before anything has been published.

## Added in phase 3 — Home Assistant discovery

- `internal/hass` — discovery payload builder. Each device becomes one
  Home Assistant device carrying its state, uptime, CPU, memory, uplink
  rates, firmware and update flag, plus one set of entities per port
  (link, speed, PoE) and per radio (channel, TX retries). Verified
  against a live installation: 345 entities for a 12-device site.
- **Network topology in Home Assistant.** `via_device` reproduces the
  real hierarchy — client → AP → switch → gateway — instead of a flat
  device list. This is what the uplink UUID resolution from phase 1
  was for.
- **Localisation (`LANGUAGE: en|de`).** Display names follow the
  configured language while `unique_id`, `object_id`, config topics and
  state topics stay English. Switching language renames what the user
  sees and nothing else: no entity is re-created, no `entity_id`
  changes, no history is lost. Verified on a live installation by
  switching `de`→`en` and diffing all 345 payloads — every identifier
  byte-identical.
- **Two-stage availability.** Entities go unavailable when the bridge
  stops *or* when their device goes offline, so a switch that lost power
  does not sit there showing its last CPU reading as current. The
  entities that report the offline condition itself — state, reachable,
  firmware, update — deliberately opt out of the second stage.
- **Orphan cleanup.** A device removed from the site, or a port that
  disappears, has its entities cleared with an empty retained payload
  instead of leaving permanently unavailable ones behind.
- **Home Assistant birth message.** The daemon subscribes to
  `homeassistant/status` and re-announces everything after
  `HASS_BIRTH_GRACETIME`, because Home Assistant forgets
  MQTT-discovered entities on restart.

### Changed

- `model.BandSegment` replaces the copy that existed in the coordinator,
  and `internal/hass` receives the topic layout through an interface
  rather than rebuilding it from config. Both were duplication that
  would have produced entities pointing at topics nobody publishes.

## Added in phase 4 — clients and presence detection

- **Client filter engine.** Clients are off by default and filtered by
  connection type, network name, VLAN, SSID and MAC allow/deny lists.
  On the reference site this turns 121 clients into 7. Without it a
  first start would create several hundred Home Assistant entities.
- **Presence with a grace period.** A client stays `home` until it has
  been absent for `AWAY_TIMEOUT` (default 300 s). Wireless clients drop
  out of the client list for a cycle while roaming between access
  points, and power-saving phones disappear regularly — flipping on the
  first missed poll makes every presence automation flap.
- **`device_tracker` discovery.** Each published client becomes its own
  Home Assistant device with `source_type: router`, and `via_device`
  continues the topology: gateway → switch → AP → phone.
- Clients resolve their uplink UUID to a MAC exactly like devices do,
  and their VLAN from the network catalogue.

### Notes

- The evaluation order is fixed: `EXCLUDE_MACS` always wins,
  `INCLUDE_MACS` is an either/or that bypasses the other dimensions,
  then guests, then type/network/VLAN/SSID (AND across dimensions, OR
  within one), then the `MAX` cap.
- The client list is sorted before the cap is applied. Without that the
  API's ordering would decide which clients get entities, and it changes
  between polls — Home Assistant would see entities appear and disappear
  continuously.
- Reaching `MAX` logs a warning naming how many clients matched, rather
  than truncating silently.
- An away client keeps its `device_tracker` but drops its IP: a tracker
  that disappears makes automations referencing it error out, while a
  stale IP suggests the client is still reachable there.

## Added in phase 5 — the optional classic API layer

- `internal/unifi/classic` — cookie-session client for the legacy
  controller API, with layout probing (UniFi OS vs standalone
  controller), CSRF handling and automatic re-login on session expiry.
- `internal/unifi.Facade` — combines both API flavours behind one
  contract with a capability query, so the coordinator never announces
  an entity it cannot back with values.
- **Site health**: WAN status, WAN IP, latency, throughput and client
  counts, published under `unifi/<site>/health/…` and announced as a
  synthetic Home Assistant site device. This is the one area with no
  official equivalent — the Integration API's `/wans` endpoint carries
  an id and a name and nothing else.
- **Client enrichment**: SSID, signal strength, hostname and the blocked
  flag, which the Integration API does not report at all. The
  `CLIENTS.SSIDS` filter therefore becomes usable.
- **PoE power draw** in watts per port, likewise absent from the
  official API.
- `CLIENTS.SIGNAL_SENSOR` adds a signal-strength sensor per wireless
  client. Off by default: one more entity per client, and the value only
  means anything for wireless ones.

### Notes

- **A classic-layer failure degrades, it does not propagate.** A failed
  login or a broken endpoint switches the affected capabilities off and
  logs once; the official path keeps running. That is the difference
  between "site health is missing" and "the bridge is down", and it is
  why the split exists at all.
- A degraded capability stops being called, so a broken classic layer
  produces one warning rather than one per poll.
- The Integration API decides which clients exist; the classic layer
  only adds fields. A client missing from the classic response keeps its
  entity.

### Fixed during live verification

- **PoE wattage was enriched but never published.** The facade filled in
  `PoE.PowerW` and the coordinator dropped it on the floor, so the topic
  from the concept never existed. Ports drawing no power still publish
  nothing, rather than a flat 0 W that looks like a reading.
- **A missing WAN latency was published as `0`.** The reference console
  reports no latency at all for its WAN subsystem, and `0 ms` is a
  measurement rather than the absence of one. Optional health values are
  pointers now and publish empty, so Home Assistant shows "unknown".

## Added in phase 6 — actuators

Home Assistant can now write back: restart a device, power-cycle a PoE
port, toggle the locate LED, block a client, authorize a guest and
switch an SSID on or off. Every control is off by default and
individually enabled under `CONTROLS`.

Three rules shape the implementation, each because breaking it produces
a specific failure:

- **The MQTT handler never blocks.** It runs inline in the client's read
  loop, the same goroutine that decodes acknowledgements and feeds the
  keep-alive watchdog. It parses and enqueues; a bounded worker executes.
  A handler waiting on an HTTP round-trip would make the watchdog
  declare a healthy connection dead.
- **Retained commands are dropped.** The broker re-delivers the last
  retained message per filter on every reconnect, so a stale
  `mosquitto_pub -r` from an old test would power-cycle a real port on
  every daemon start.
- **No optimistic state.** After a command the affected object is
  re-polled out of band, and the published state comes from the console.
  A failed command therefore snaps the entity back instead of lying
  about what happened.

Controls are gated twice: on the operator enabling them, and on the
capability being available. A switch the daemon cannot serve is never
announced, because an entity that errors on click is worse than no
entity.

### Fixed

- **A rejected password was reported as the wrong problem.** UniFi OS
  answers a bad login with HTTP 403, not 401. Treating 403 as "wrong
  console layout" made the client try the standalone endpoint, get a 401
  because it does not exist there, and report *that* — pointing at
  `/api/login` while the real issue was the password. A failure that
  exhausts both layouts now names what each one said.

## Added in phase 7 — the diagnostic web UI

- `internal/state` — a thread-safe snapshot of what the daemon last
  read. Only allocated when `WEB_ENABLE` is on: without the UI a pure
  MQTT bridge should not carry a second copy of every device.
- `internal/web` — an embedded single-page UI answering one question:
  is the bridge doing what it should? It shows the broker link, the site
  and controller version, every poll loop with its age, the classic-layer
  capabilities, site health, all devices with live statistics and PoE
  draw, the filtered clients with presence, and the SSID catalogue.
  Read-only — the write path is MQTT, and a second one would be a second
  thing to secure.
- Optional HTTP basic auth, compared in constant time. The static assets
  sit behind the same gate as the API: an unauthenticated page that then
  fails to load data is a confusing half-open door.
- `GET /api/health` as a process-level liveness probe. It deliberately
  stays green when the console is unreachable — a restart cannot fix the
  console, so failing there would only produce a crash loop.

### Notes

- All asset and API references are relative, which is what lets the same
  page work unchanged behind the Home Assistant Ingress path prefix.
- The UI builds text nodes and never uses `innerHTML`: a device named
  with markup is a legal UniFi name.
- Statistics survive a device refresh. The two arrive from different
  loops on different cadences, so wiping them would make the table blink
  empty every minute. A device with no statistics at all is marked as
  such, so it is distinguishable from one genuinely idle at 0%.

### Fixed

- **The store was accepted and never assigned.** `Deps.Store` reached
  the constructor and was dropped on the floor, so the UI rendered an
  empty page while everything compiled and every unit test passed. Found
  by running it against the real console; a test now follows the data
  from the poll loops into the store.
