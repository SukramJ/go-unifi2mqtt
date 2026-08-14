# CLAUDE.md — AI Assistant Guide for go-unifi2mqtt

## Project Overview

**go-unifi2mqtt** is a pure-Go daemon that bridges a **local UniFi Network
installation** (UDM / UDM-Pro / UDR / UCG / UX / UniFi OS Server, or a
standalone software controller) to an **MQTT broker**, with optional
**Home Assistant** MQTT auto-discovery.

It polls the console's **official Network Integration API**
(`/proxy/network/integration/v1`, API-key authenticated) for sites,
devices, per-device statistics and clients, and — where that API has
gaps — an optional **classic controller API** client
(`/proxy/network/api/s/<site>/`, cookie session) for site health, client
blocking and WLAN toggles. Values are published as MQTT topics plus HA
discovery payloads; inbound HA commands (restart device, power-cycle a
PoE port, block a client, …) are written back to the console.

**Read [`CONCEPT.md`](./CONCEPT.md) before touching anything** — it is
the implementation concept: API strategy, package layout, MQTT topic
tree, HA entity model, client filtering, polling design and the phased
roadmap. Phases 0–3 are done: the daemon polls the console, publishes
devices, ports, radios and the WLAN catalogue, and announces them to
Home Assistant via MQTT discovery. Clients (phase 4) are next.

## Key Characteristics

- **Language**: Go 1.26+ (see `go.mod` / CI `GO_VERSION`).
- **Module path**: `github.com/SukramJ/go-unifi2mqtt`.
- **License: MIT.** Every Go source file starts with:
  ```go
  // SPDX-License-Identifier: MIT
  // Copyright (C) 2026 SukramJ
  ```
  Note this differs from the sibling project `go-mtec2mqtt`, which is
  LGPL-3.0-or-later because it derives from Christian Rödel's
  `MTECmqtt`. Do **not** copy LGPL headers from there into this repo.
- **Deployment**: one static binary (`CGO_ENABLED=0`), the `unifi2mqtt`
  daemon. Also ships as a Docker image (distroless runtime) and a Home
  Assistant add-on (`addon/`).
- **Dependencies are deliberately minimal**: `golang.org/x/sync`
  (errgroup), `gopkg.in/yaml.v3`, and `github.com/SukramJ/go-mqtt`
  (the shared MQTT client, MIT — MQTT 5.0 by default, 3.1.1 selectable
  via `TCPConfig.ProtocolVersion`). The UniFi API clients are
  hand-rolled on `net/http` — no third-party UniFi SDK.
- **Config**: YAML (`config-template.yaml` is the annotated reference),
  overridable per-key via `UNIFI_<KEY>` env vars. Loaded from
  `--config`, then `$XDG_CONFIG_HOME/unifi2mqtt/config.yaml` (or
  `$APPDATA` on Windows), then `~/.config/unifi2mqtt/config.yaml`.

## Repository Structure

```
cmd/unifi2mqtt/          daemon entry point (main.go)
internal/config/         YAML loader + UNIFI_* env overlay + validation
internal/unifi/          shared HTTP transport: TLS, auth, retry, rate limiting
internal/unifi/integration/  official Network Integration API v1 client
internal/unifi/classic/  classic controller API client (cookie session + CSRF)
internal/model/          API-neutral domain types (Site, Device, Client, Health)
internal/coordinator/    orchestration: poll loops, command queue, reconcile
internal/hass/           Home Assistant discovery payload builder
internal/state/          thread-safe live-value cache shared with the web UI
internal/web/            optional diagnostic web UI / HA add-on Ingress panel
internal/version/        build-info package (Version/Commit/BuildDate, ldflags)
addon/                   Home Assistant add-on packaging (Dockerfile, config.yaml, DOCS.md)
script/                  run.sh (add-on entrypoint), capture-fixtures.sh, extract-release-notes.sh
config-template.yaml     annotated reference config
CONCEPT.md               implementation concept — the design source of truth
.github/workflows/       ci.yml, docker-build-push.yml, addon-image.yml, release-on-tag.yml, codeql.yml, dependabot-auto-merge.yml
```

`model`, `config`, `unifi`, `unifi/integration`, `coordinator` and
`hass` exist as of phase 3; `state`, `web` and `unifi/classic` are
created as the later phases in `CONCEPT.md` land. The layout above is
the target shape, not a claim about what exists today.

The MQTT transport is not part of this tree: it comes from the external
`github.com/SukramJ/go-mqtt` module as a regular `go.mod` dependency,
not an `internal/` package.

## Development Commands

All defined in the `Makefile` (`make help` lists them):

```sh
make build          # build the daemon into bin/
make run            # build then run against ./config.yaml
make test           # go test -race -count=1 -timeout=60s ./... (CGO_ENABLED=1 for race detector)
make test-cover     # tests + coverage report (coverage.out)
make vet            # go vet ./...
make fmt            # gofumpt -w . && goimports -w -local <module> .
make fmt-check      # fail if gofumpt would rewrite anything (CI gate)
make lint           # golangci-lint run ./...
make vuln           # govulncheck ./...
make licenses       # go-licenses check, forbids GPL/AGPL/LGPL(reciprocal)/MPL deps
make check          # vet + fmt-check + lint + test — the pre-commit/pre-push gate
make docker         # build a tagged container image
make release        # cross-compile linux/amd64, linux/arm64, darwin/arm64 archives into dist/
make setup          # install gofumpt/goimports/golangci-lint/govulncheck/go-licenses + git hooks
make tidy           # go mod tidy
make clean          # remove bin/, dist/, coverage.out
```

Run a single package's tests directly with `go test ./internal/unifi/...`
etc. — no special test runner beyond `go test`.

## Code Conventions

- **License header** on every `.go` file: `SPDX-License-Identifier: MIT`
  + `Copyright (C) 2026 SukramJ`.
- **No CGo** in the default build (`CGO_ENABLED=0`); CGo is only
  re-enabled transiently in CI/Makefile to get the race detector during
  `make test`.
- **`golangci-lint` v2** config (`.golangci.yaml`) enables: `bodyclose`,
  `contextcheck`, `copyloopvar`, `errcheck`, `errorlint`, `exhaustive`,
  `gocritic`, `gosec`, `govet`, `intrange`, `makezero`, `nilerr`,
  `noctx`, `prealloc`, `reassign`, `revive`, `sloglint`, `staticcheck`,
  `thelper`, `tparallel`, `unconvert`, `unparam`, `unused`,
  `usestdlibvars`, `wastedassign`.
- **Formatting**: `gofumpt` (stricter gofmt) + `goimports -local
  github.com/SukramJ/go-unifi2mqtt` for import grouping.
- **Structured logging**: `log/slog` (enforced by `sloglint`).
- **Package layout mirrors the data flow**: config → unifi → model →
  coordinator → mqtt/hass/web, each as its own `internal/` package with
  colocated `_test.go` files.
- **Never log secrets.** The API key, the classic password and the MQTT
  password must not reach a log line, an error message, the web UI or an
  MQTT payload. Redact them in any `String()`/diagnostic output.
- **HTTP correctness is tested against golden fixtures**: UniFi API
  responses live as JSON under `internal/unifi/integration/testdata/`
  and are replayed via `httptest.Server`, so the decoders are anchored
  without a live console. `script/capture-fixtures.sh` replaces them
  with redacted output from a real console; see the testdata README for
  the provenance rules.
- **Dependency licensing is enforced by tooling**, not just policy —
  `make licenses` (`go-licenses check --disallowed_types=forbidden,
  restricted,reciprocal`) blocks GPL/AGPL/LGPL/MPL-style dependencies
  from entering the tree.
- **Git hooks**: `make setup` (or `make hooks`) points `core.hooksPath`
  at `.githooks/`, which blocks direct commits on `main`/`master`.
- **Commit style**: Conventional Commits with a scope, e.g.
  `feat(unifi): add integration API device client`, `fix(addon): ...`,
  `chore(deps): ...`.
- **Release bookkeeping — three files move together.** A version bump
  touches `internal/version/version.go` and `addon/config.yaml`, and
  every `changelog.md` entry must ALWAYS be mirrored into
  `addon/CHANGELOG.md` (the file Home Assistant renders in the add-on
  UI's Changelog tab) — keep the two changelog files identical.
- **CI** (`.github/workflows/ci.yml`) runs three jobs: `lint` (go vet +
  gofumpt check), `test` (matrix across ubuntu/macos/windows with the
  race detector), `build` (compiles the binary and checks the
  `--version` banner). Separate workflows publish the Docker image, the
  HA add-on image, and run CodeQL + Dependabot auto-merge.

## Two Rules That Are Easy To Break

- **List endpoints are not detail endpoints.** `GET /networks` omits the
  subnets and `GET /devices` omits `uplink`/`interfaces`. Both fan out a
  per-object detail call. Collapsing that back would compile, pass a
  naive test, and silently lose VLAN mapping and the device topology.
- **Topic suffixes are an API.** Every key in `internal/coordinator/topics.go`
  doubles as the Home Assistant entity key and the translation-table
  lookup. Renaming one orphans the entity and its history in every
  existing installation. The same goes for `unique_id` and `object_id`
  in `internal/hass` — `TestIdentifiersAreStable` pins the exact strings.
- **Never rebuild a topic in a second place.** `internal/hass` receives
  the topic layout through the `Topics` interface rather than
  reconstructing it from config. Two copies drifting apart produce
  entities that stay "unavailable" forever with nothing visibly wrong in
  either package.

## UniFi API Notes

- **Two surfaces, one facade.** `internal/unifi/integration` is the
  officially supported client and the default path;
  `internal/unifi/classic` is opt-in (`CLASSIC_ENABLE`) and fills the
  documented gaps. The coordinator only ever sees the combined facade
  and the `internal/model` types — never a raw API DTO.
- **The classic API is undocumented** and can break on any controller
  update. Every feature it powers must degrade gracefully: if the
  classic client is disabled or failing, the daemon keeps running on the
  official surface with those entities absent, never crashing.
- **Rate limits are real.** Honour `429` + `Retry-After`; do not fan out
  a per-device statistics call for every device on every tick without a
  bounded worker pool.
- **`_id` vs MAC.** The Integration API keys objects by UUID, the
  classic API by MongoDB `_id`, and both carry the MAC. The MAC is the
  stable cross-API identity and the basis of MQTT topics and HA
  `unique_id`s — see `CONCEPT.md` for the exact rule.

## When in Doubt

- [`CONCEPT.md`](./CONCEPT.md) is the design source of truth.
- [`README.md`](./README.md) documents the MQTT topic layout, config
  keys and quickstart paths (Docker, HA add-on, plain binary).
- [`changelog.md`](./changelog.md) has the release history.
- [`config-template.yaml`](./config-template.yaml) documents every
  config field inline.
- [`addon/README.md`](./addon/README.md) and
  [`addon/DOCS.md`](./addon/DOCS.md) cover the Home Assistant add-on.
- The sibling project [`go-mtec2mqtt`](https://github.com/SukramJ/go-mtec2mqtt)
  is the structural template for this repo — reuse its coordinator /
  hass / state / web patterns, but not its LGPL headers.
