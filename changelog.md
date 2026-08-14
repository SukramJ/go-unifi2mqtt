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
