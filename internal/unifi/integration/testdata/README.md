# Integration API fixtures

Golden responses for the UniFi Network Integration API v1, replayed by
`httptest.Server` in `client_test.go` so the decoders are exercised
without a live console.

## Provenance

These are **hand-written against the official OpenAPI document**, not
captured from hardware. Every field, enum value and nesting level is
transcribed from the schema shipped by Network application 10.5.67 and
10.6.90 — which are byte-identical for all schemas used here (see
`CONCEPT.md` §2.5). Addresses, names and identifiers are synthetic and
use documentation ranges (RFC 5737 / RFC 7042).

**Their shape is cross-checked against a live 10.5.67 console.** That
check is what caught the split between list and detail responses: the
schema alone reads as though `/networks` returns `ipv4Configuration` and
`/devices` returns `uplink`, but neither list does. `networks.json` and
`devices.json` therefore deliberately omit those fields, and the
`network_*.json` / `device_*.json` files carry them. Removing that
distinction would make the tests pass while the daemon silently loses
VLAN mapping and the device topology.

Replacing them with real captured output is a drop-in operation: run
`script/capture-fixtures.sh` against your own console and overwrite the
files. The script redacts MACs, IPs, names and serials on the way out.

## Files

| File                  | Endpoint                                                      |
| --------------------- | ------------------------------------------------------------- |
| `info.json`           | `GET /v1/info`                                                |
| `sites.json`          | `GET /v1/sites`                                               |
| `devices.json`        | `GET /v1/sites/{siteId}/devices` — no `uplink`, no `interfaces` |
| `device_gateway.json` | `GET /v1/sites/{siteId}/devices/{deviceId}` (gateway)         |
| `device_switch.json`  | `GET /v1/sites/{siteId}/devices/{deviceId}` (PoE switch)      |
| `device_ap.json`      | `GET /v1/sites/{siteId}/devices/{deviceId}` (access point)    |
| `device_stats.json`   | `GET /v1/sites/{siteId}/devices/{deviceId}/statistics/latest` |
| `clients.json`        | `GET /v1/sites/{siteId}/clients` (all four client types)      |
| `networks.json`       | `GET /v1/sites/{siteId}/networks` — no `ipv4Configuration`    |
| `network_*.json`      | `GET /v1/sites/{siteId}/networks/{networkId}` (with subnets)  |
| `wlans.json`          | `GET /v1/sites/{siteId}/wifi/broadcasts`                      |
