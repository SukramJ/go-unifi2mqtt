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
