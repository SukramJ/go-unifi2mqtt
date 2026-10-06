# Concept: go-unifi2mqtt

Implementation concept for bridging a local UniFi Network installation to
MQTT. This document is the project's design reference — code follows it, not
the other way around.

**Last updated:** 2026-08-14 · **Status:** 1.0.0 released (phases 0–8); phase 9 optional

---

## 1. Goal and scope

### Goal

A single statically linked Go daemon that polls a local UniFi Network
installation and publishes its state as MQTT topics — including Home
Assistant auto-discovery and a write-back channel for commands.

### In scope

- **Devices** (adopted switches, APs, gateways): state, firmware, uptime,
  CPU/memory, uplink rates, port and PoE state, radio channels
- **Clients**: presence (`home`/`not_home`) plus metadata, filtered by
  connection type, network/VLAN, SSID and MAC lists
- **Site health**: WAN status, WAN IP, latency, throughput, client counts,
  subsystem status
- **Controls**: restart device, power-cycle a PoE port, locate LED, block a
  client, toggle a WLAN, authorize a guest

### Out of scope

- The Ubiquiti cloud (`api.ui.com`, Site Manager API, Connector Proxy). The
  daemon talks only to the local console. The client abstraction (§3) is cut
  so a cloud transport could be added later, but that is not a goal here.
- Configuration management (creating networks/WLANs, firewall rules, port
  forwards). This is a monitoring and actuator bridge, not a provisioning
  tool.
- UniFi Protect, Access, Talk. The Network application only.

### Robustness as a non-goal boundary

The daemon must **never** die because of a UniFi-side problem. An
unreachable console, an expired session cookie, a changed response schema or
a rate limit are expected conditions that lead to degraded operation — not to
process exit.

---

## 2. The UniFi API landscape and access strategy

### 2.1 Available surfaces

| Flavour                     | Path                                              | Auth              | Status                          |
| --------------------------- | ------------------------------------------------- | ----------------- | ------------------------------- |
| **Network Integration API** | `https://<console>/proxy/network/integration/v1/` | `X-API-KEY`       | Official, Network 10.5+ (§13.1) |
| **Classic controller API**  | `https://<console>/proxy/network/api/s/<site>/`   | Cookie + CSRF     | Unofficial, but stable for years |
| Site Manager API (cloud)    | `https://api.ui.com/v1/`                          | `X-API-KEY`       | Official — **out of scope**     |
| Connector Proxy (cloud)     | `https://api.ui.com/v1/connector/...`             | `X-API-KEY`       | Official — **out of scope**     |

On a **standalone software controller** (no UniFi OS) the `/proxy/network`
prefix is absent: the classic API sits directly under `/api/s/<site>/` on
port 8443, and login goes to `/api/login` instead of `/api/auth/login`. The
HTTP layer (§3.2) detects the flavour at startup with a probe request and
sets the path prefix accordingly.

### 2.2 Chosen strategy: Integration API first, classic optional

**Primary path — Integration API.** Everything officially retrievable comes
from there. Benefits: a static API key instead of session handling, a
documented schema, versioning, no CSRF mechanics, no breakage on controller
updates.

**Fallback layer — classic API.** Enabled with `CLASSIC_ENABLE`, off by
default. It supplies exactly what the official API lacks.

The table below is verified against the **OpenAPI specification of Network
applications 10.5.67 and 10.6.90** and, for the fields that matter, against
a live console (see §2.5) — not against secondary sources.

| Capability                   | Integration API                                        | Classic API                    |
| ---------------------------- | ------------------------------------------------------ | ------------------------------ |
| Sites, devices, clients      | ✅                                                     | ✅                             |
| Device statistics            | ✅ `/devices/{id}/statistics/latest`                   | ✅ (inline in `/stat/device`)  |
| Restart device               | ✅ `POST /devices/{id}/actions`                        | ✅ `/cmd/devmgr`               |
| PoE port power-cycle         | ✅ `POST /devices/{id}/interfaces/ports/{idx}/actions` | ✅ `/cmd/devmgr`               |
| Authorize guest              | ✅ `POST /clients/{id}/actions`                        | ✅ `/cmd/stamgr`               |
| **Network / VLAN catalogue** | ✅ `/networks` + one detail call per network ⁵          | ✅ `/rest/networkconf`         |
| **WLAN catalogue (SSIDs)**   | ✅ `/wifi/broadcasts`                                  | ✅ `/rest/wlanconf`            |
| **Site / WAN health**        | ❌ ¹                                                   | ✅ `/stat/health`              |
| **Client → SSID / signal**   | ❌ ²                                                   | ✅ `/stat/sta`                 |
| **PoE power draw (W)**       | ❌ ³                                                   | ✅ `/stat/device`              |
| **Block / kick a client**    | ❌                                                     | ✅ `/cmd/stamgr`               |
| **Toggle a WLAN**            | ❌ ⁴                                                   | ✅ `PUT /rest/wlanconf/{id}`   |
| **Locate LED**               | ❌                                                     | ✅ `/cmd/devmgr` `set-locate`  |
| **Realtime events**          | ❌                                                     | ✅ `wss://.../wss/s/<site>/events` |

¹ `GET /sites/{siteId}/wans` exists but returns only `id` and `name` — no
connection state, no WAN IP, no latency.
² The client schema is exactly `type`, `id`, `name`, `connectedAt`,
`ipAddress`, `macAddress`, `uplinkDeviceId`, `access`. The detail endpoint
returns no more than that.
³ `Port PoE overview` carries only `standard`, `type`, `enabled`, `state` —
no power field.
⁴ `PUT /wifi/broadcasts/{id}` exists but expects the complete configuration
object. Safely toggling just `enabled` would be a read-modify-write over a
large schema — too risky for phase 6, so classic for now.
⁵ The list carries `vlanId` but **not** the subnets; those need
`GET /networks/{id}`. See the paragraph below.

**Consequence for client filtering** (§6.2), corrected against the first
draft:

| Filter dimension       | Without the classic layer                                                                                      |
| ---------------------- | -------------------------------------------------------------------------------------------------------------- |
| `TYPES`                | ✅ straight from `type`                                                                                         |
| `INCLUDE/EXCLUDE_MACS` | ✅ straight from `macAddress`                                                                                    |
| `EXCLUDE_GUESTS`       | ✅ from `access.type == "GUEST"`                                                                                 |
| `VLANS`, `NETWORKS`    | ✅ **indirectly**: match the client's `ipAddress` against the subnets from `/networks` (`ipv4Configuration.hostIpAddress` + `prefixLength`) |
| `SSIDS`                | ❌ classic layer only                                                                                            |

The IP-subnet detour for VLAN/network is why `/networks` is loaded in the
`static` loop (§8.1). It is exact as long as the networks have disjoint
subnets — the normal case. Where subnets overlap, the longest prefix wins.

**Two list endpoints do not carry what their detail counterparts do.**
Verified against a live 10.5.67 console, because the OpenAPI schema makes
this easy to misread (the detail schemas inherit from the overview ones,
so both look plausible):

| Endpoint    | The list omits                      | Consequence                                    |
| ----------- | ----------------------------------- | ---------------------------------------------- |
| `/networks` | `ipv4Configuration` (the subnets)   | no VLAN mapping at all without a per-network call |
| `/devices`  | `uplink`, `interfaces` (ports/radios) | every device reports no uplink, so `via_device` collapses |

Both therefore fan out one detail call per object, bounded to 4 concurrent
requests. This is the single biggest cost driver in the polling design and
is what §8.2 is built around. A failing detail call degrades to the
overview data rather than dropping the object.

**Not every client has an IP.** On the reference installation 17 of 121
clients (14%) come back with no `ipAddress` — mostly wired devices the
console has not seen an address for. They cannot be mapped to a VLAN by
definition, so a client that maps to no network is **dropped** when a
VLAN or network filter is set, and reported once per cycle at debug
level. Operators who need those clients anyway have to name them in
`INCLUDE_MACS`, which bypasses the network filters entirely.

A filter on an unavailable dimension is **validated hard at startup**
("SSID filter set but CLASSIC_ENABLE=false") instead of silently letting
every client through.

### 2.3 Why not classic-only

The classic API can do everything, but it is undocumented. Ubiquiti has
reshaped it repeatedly (UniFi OS prefix, mandatory CSRF, `/v2/api`
endpoints). A project standing solely on it potentially breaks with every
controller update. The chosen split keeps core operation (devices, clients,
presence) on the official surface and isolates the breakage risk in an
optional module that can be switched off.

### 2.4 Realtime events: deliberately deferred

The classic API offers a WebSocket event stream delivering client join/leave
and device state changes without polling — attractive for presence latency.
It is nonetheless **not part of phases 1–4**:

- The event schema is even less documented than the REST endpoints.
- It does not replace polling, it complements it (reconnect gaps have to be
  closed by a full reconciliation anyway).
- It duplicates the state logic while polling is not yet in place.

It is scheduled as **phase 9**: purely as an accelerator that triggers an
immediate poll of the affected object ("event as trigger, REST as truth").
That keeps the data model uniform, and a failing stream degrades cleanly to
the normal polling cadence.

### 2.5 Verification status

The original verification caveat is **resolved**. The basis is the official
OpenAPI specification that every console serves itself:

```
GET https://<console>/proxy/network/integration/openapi/document.json
```

Cross-checked against the archived specs of Network application **10.5.67**
(the reference installation for this project) and **10.6.90** (collection
[beezly/unifi-apis](https://github.com/beezly/unifi-apis)). Both agree on the
complete endpoint list and are byte-identical for every schema §4 depends on,
which is what fixes the supported floor at 10.5 (§13.1). Every field name in
§4 comes from there, not from secondary sources.

Three assumptions of the first draft were wrong and are corrected:

| First draft                                    | Actually                                                     |
| ---------------------------------------------- | ------------------------------------------------------------ |
| Network/VLAN and WLAN catalogue is classic-only | Officially available (`/networks`, `/wifi/broadcasts`)       |
| VLAN filtering requires the classic layer       | Works officially via IP-subnet mapping                       |
| `uplink` carries the uplink device's MAC        | Carries `uplinkDeviceId` (UUID) — needs resolution (§3.4)    |

`script/capture-fixtures.sh` (§11.2) also pulls the spec from the operator's
**own** console, so version differences surface during setup.

---

## 3. Architecture

### 3.1 Data flow

```
                    ┌──────────────────────────────────────────┐
                    │            cmd/unifi2mqtt                │
                    │  flags, logger, signals, wiring          │
                    └────────────────────┬─────────────────────┘
                                         │
        ┌────────────────────────────────┼────────────────────────────────┐
        │                                │                                │
┌───────▼────────┐            ┌──────────▼──────────┐          ┌──────────▼────────┐
│ internal/config│            │ internal/coordinator│          │  internal/web     │
│ YAML + UNIFI_* │───────────▶│  poll loops         │◀────────▶│  diagnostic UI    │
│ + validation   │            │  command queue      │          │  (optional)       │
└────────────────┘            │  reconcile          │          └───────────────────┘
                              └───┬──────────┬──────┘
                                  │          │
                  ┌───────────────▼───┐  ┌───▼──────────────────┐
                  │ internal/unifi    │  │  github.com/         │
                  │  .Facade          │  │  SukramJ/go-mqtt     │
                  └───┬───────────┬───┘  │  TCPClient+Lifecycle │
                      │           │      │  +Breaker            │
       ┌──────────────▼──┐   ┌────▼──────────────┐ └────┬───────┘
       │ unifi/integration│   │  unifi/classic    │      │
       │  X-API-KEY, v1   │   │  cookie + CSRF    │      │
       └──────────────────┘   └───────────────────┘      │
                      │           │                      │
                      └─────┬─────┘             ┌────────▼─────────┐
                            │                   │  internal/hass   │
                   ┌────────▼────────┐          │  discovery payl. │
                   │ internal/model  │          └──────────────────┘
                   │  Site, Device,  │
                   │  Client, Health │          ┌──────────────────┐
                   └─────────────────┘          │ internal/state   │
                                                │  live cache      │
                                                └──────────────────┘
```

### 3.2 Package responsibilities

| Package                      | Responsibility                                                                                                                                                |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/config`            | Load YAML, overlay `UNIFI_*` env, apply defaults, validate. Result is one typed `Config`.                                                                       |
| `internal/unifi`             | Shared HTTP layer: TLS config, timeouts, retry with backoff, `429`/`Retry-After`, console flavour detection (UniFi OS vs standalone), redaction on logging. Plus the `Facade` combining both clients. |
| `internal/unifi/integration` | Integration API v1 client. Knows pagination (`offset`/`limit`/`totalCount`) and the filter syntax.                                                              |
| `internal/unifi/classic`     | Classic API client. Login, session cookie, CSRF token, automatic re-login on `api.err.LoginRequired`.                                                           |
| `internal/model`             | API-neutral domain types. **No package outside `internal/unifi/*` ever sees a raw API DTO.**                                                                    |
| `internal/coordinator`       | Poll loops per cadence, command queue for write-back, discovery reconcile, orphan detection.                                                                    |
| `internal/hass`              | Build discovery payloads. No I/O — returns `Entry` values the coordinator publishes.                                                                            |
| `internal/state`             | Thread-safe live cache; only allocated when the web UI is enabled.                                                                                              |
| `internal/web`               | Optional diagnostic SPA, embedded via `go:embed`, doubling as the HA Ingress panel.                                                                             |
| `internal/version`           | Build metadata via `-ldflags`.                                                                                                                                 |

### 3.3 The facade as the only contact point

```go
// internal/unifi
type Facade struct {
    integration *integration.Client
    classic     *classic.Client // nil when CLASSIC_ENABLE=false
}

func (f *Facade) Sites(ctx context.Context) ([]model.Site, error)
func (f *Facade) Devices(ctx context.Context, site string) ([]model.Device, error)
func (f *Facade) DeviceStats(ctx context.Context, site, deviceID string) (model.DeviceStats, error)
func (f *Facade) Clients(ctx context.Context, site string) ([]model.Client, error)
func (f *Facade) Networks(ctx context.Context, site string) ([]model.Network, error)
func (f *Facade) WLANs(ctx context.Context, site string) ([]model.WLAN, error)

// Classic layer only; without it: ErrCapabilityUnavailable
func (f *Facade) Health(ctx context.Context, site string) (model.Health, error)

// Actuators
func (f *Facade) RestartDevice(ctx context.Context, site, deviceID string) error
func (f *Facade) PowerCyclePort(ctx context.Context, site, deviceID string, portIdx int) error
func (f *Facade) SetLocate(ctx context.Context, site, mac string, on bool) error
func (f *Facade) SetClientBlocked(ctx context.Context, site, mac string, blocked bool) error
func (f *Facade) SetWLANEnabled(ctx context.Context, site, wlanID string, on bool) error
func (f *Facade) AuthorizeGuest(ctx context.Context, site, mac string, d time.Duration) error

// Capability query — the coordinator uses it to decide which entities to
// offer in the first place.
func (f *Facade) Has(c Capability) bool
```

`ErrCapabilityUnavailable` is a sentinel error. The coordinator queries
`Has(...)` **before** building discovery and never creates entities it cannot
serve — rather than offering them and failing on click.

### 3.4 Identity: the MAC is the key

The two surfaces key objects differently:

| Surface         | Device key           | Client key           |
| --------------- | -------------------- | -------------------- |
| Integration API | UUID (`id`)          | UUID (`id`)          |
| Classic API     | Mongo `_id`          | Mongo `_id`          |
| both            | `macAddress` / `mac` | `macAddress` / `mac` |

**Rule: the normalised MAC (lowercase, no separators) is the canonical
identity** — it forms the MQTT topic segment and the core of the HA
`unique_id`. The API-native IDs are carried along in the `model` type because
actuator calls need them, but they never appear in a topic or a `unique_id`.
Reason: a re-adopt or a controller restore hands out fresh UUIDs, and HA
would orphan every entity and its history.

For display (`name` in HA) the MAC is formatted with colons; in the topic it
stays separator-free, because while `:` is legal in MQTT topics it is
awkward in many tools.

**Uplink resolution.** The API reports `uplink.deviceId` — a UUID, not a MAC.
The coordinator builds an ID→MAC map from the device list on every poll and
resolves `UplinkMAC` from it. Clients carry `uplinkDeviceId` the same way.
This map is what makes HA's `via_device` topology (§6.1) possible.

---

## 4. Data model

All types live in `internal/model`. Timestamps are `time.Time` (UTC),
durations `time.Duration`, percentages `float64` in the range 0–100. Every
field name in a comment refers to the verified OpenAPI schema (§2.5).

```go
type Site struct {
    ID       string // API UUID
    Name     string // "Default"
    Internal string // internalReference, e.g. "default" — the classic API's identifier
}

type Device struct {
    MAC          MAC        // canonical identity (from macAddress)
    ID           string     // Integration API UUID (for actuators)
    ClassicID    string     // Mongo _id (for classic actuators), may be empty
    Name         string
    Model        string
    Type         DeviceType // Gateway | Switch | AccessPoint | Other
    IP           netip.Addr
    State        DeviceState
    Supported    bool       // supported: false ⇒ the console manages it only rudimentarily
    Firmware     string     // firmwareVersion
    UpdateAvail  bool       // firmwareUpdatable
    AdoptedAt    time.Time
    UplinkID     string     // uplink.deviceId — a UUID, NOT the MAC (§3.4)
    UplinkMAC    MAC        // resolved by the coordinator; empty on the gateway
    Features     []string   // "switching", "accessPoint" — drives which entities make sense
    Ports        []Port
    Radios       []Radio
}

// The nine states from the spec plus UNKNOWN as a catch-all for values
// Ubiquiti adds later.
type DeviceState string // ONLINE OFFLINE PENDING_ADOPTION UPDATING
                        // GETTING_READY ADOPTING DELETING
                        // CONNECTION_INTERRUPTED ISOLATED
                        // U5G_INCORRECT_TOPOLOGY UNKNOWN

type DeviceStats struct {
    Uptime        time.Duration // uptimeSec
    CPUPct        float64       // cpuUtilizationPct
    MemoryPct     float64       // memoryUtilizationPct
    LoadAvg1      float64       // loadAverage1Min
    UplinkTxBps   uint64        // uplink.txRateBps
    UplinkRxBps   uint64        // uplink.rxRateBps
    LastHeartbeat time.Time     // lastHeartbeatAt
    RadioTxRetry  map[float64]float64 // interfaces.radios[].txRetriesPct, keyed by frequencyGHz
}

type Port struct {
    Idx          int
    State        PortState // UP | DOWN | UNKNOWN
    Connector    string    // RJ45 | SFP | SFPPLUS | SFP28 | QSFP28
    SpeedMbps    int
    MaxSpeedMbps int
    PoE          *PoEState // nil when the port has no PoE
}

type PoEState struct {
    Enabled  bool
    Standard string  // "802.3af" | "802.3at" | "802.3bt"
    Type     int     // 1..4
    State    string  // UP | DOWN | LIMITED | UNKNOWN
    PowerW   float64 // classic layer ONLY — the Integration API has no
                     // power field (§2.2 footnote 3)
}

type Radio struct {
    FrequencyGHz float64 // 2.4 | 5 | 6 | 60
    Channel      int
    ChannelWidth int     // channelWidthMHz
    Standard     string  // 802.11a/b/g/n/ac/ax/be
    TxRetriesPct float64 // from statistics/latest, not from the device
}

// Client covers the four API variants (discriminator `type`). The
// Integration API is deliberately sparse here: everything from SSID
// downwards is only populated with the classic layer active (§2.2).
type Client struct {
    MAC         MAC        // empty for VPN/Teleport — they have no MAC
    ID          string
    ClassicID   string
    Name        string
    IP          netip.Addr
    Type        ClientType // Wired | Wireless | VPN | Teleport
    UplinkID    string     // uplinkDeviceId (UUID), empty for VPN/Teleport
    UplinkMAC   MAC        // resolved like on Device
    IsGuest     bool       // access.type == "GUEST"
    Authorized  bool       // access.authorized, only meaningful for guests
    ConnectedAt time.Time

    // Derived from /networks by IP-subnet mapping (§2.2):
    Network string
    VLAN    int // 0 = untagged / not mappable

    // Classic layer only:
    Hostname  string
    SSID      string
    SignalDBm int
    LastSeen  time.Time
    Blocked   bool
}

// Network catalogue from /networks — the basis of VLAN mapping.
type Network struct {
    ID         string
    Name       string
    VLAN       int
    Enabled    bool
    Default    bool
    Management string         // UNMANAGED | GATEWAY | SWITCH
    Subnets    []netip.Prefix // from ipv4Configuration; empty for UNMANAGED
}

// WLAN catalogue from /wifi/broadcasts.
type WLAN struct {
    ID        string
    Name      string // the SSID
    Enabled   bool
    NetworkID string // network.networkId, empty when type == NATIVE
}

type Health struct {
    WAN        SubsystemHealth
    LAN        SubsystemHealth
    WLAN       SubsystemHealth
    VPN        SubsystemHealth
    WANIP      netip.Addr
    LatencyMs  int
    UptimeSec  int64
    RxBps      uint64
    TxBps      uint64
    NumUser    int
    NumGuest   int
    NumIoT     int
    NumAP      int
    NumSwitch  int
    NumGateway int
}

type SubsystemHealth struct {
    Status string // "ok" | "warning" | "error" | "unknown"
}
```

**`MAC` is its own type**, not a `string`, with `ParseMAC` as the only
constructor. That forces normalisation into exactly one place and makes it
impossible to accidentally write a raw MAC from an API response into a topic.

**`netip.Addr` instead of `string`** for IPs: comparable, validated, and
`IsValid()` cleanly separates "no address" from "empty string".
`netip.Prefix` for subnets gives `Contains()` for free — which is exactly the
VLAN mapping from §2.2.

---

## 5. MQTT topic layout

Since 2.0.0 the tree follows mqtt-smarthome 2.0
(<https://github.com/mqtt-smarthome/mqtt-smarthome/blob/master/SPEC.md>),
as openccu-loom ADR 0083 decided for all six projects of the family:
`<name>/<function>/<item...>`. `<name>` is `MQTT_TOPIC` (default `unifi`,
sanitised to one segment), the function is `connected`, `status`, `set`,
`info` or `maintenance`, and below `status` and `set` the items are keyed
by site and MAC address. `<site>` is the site reference (`default`),
`<mac>` the normalised MAC without separators. The 1.x column names what
each topic was before 2.0.0; there is no compatibility switch.

`MQTT_TOPIC` is the only thing that keeps two instances on one broker
apart, and nothing checks it: two consoles or sites need two names. The
default `unifi` is also hobbyquaker's Node.js `unifi2mqtt` default; the
two on one broker need one renamed.

### 5.1 Bridge level

| Topic                       | Retain | Content                                                   | 1.x |
| --------------------------- | ------ | --------------------------------------------------------- | --- |
| `unifi/connected`           | ✅     | `0` (Last Will, clean stop) \| `1` (broker up, console not answering the device poll) \| `2` (operational) | `unifi/bridge/status` `online`/`offline` |
| `unifi/info`                | ✅     | JSON (spec §6): `name` `go-unifi2mqtt`, `version`, `spec`, `go`, `host`, `pid`, `started`, `maintenance`, plus `site`, `site_id`, `application_version`, `console_host` | `unifi/bridge/info` |
| `unifi/status/bridge/error` | ❌     | status object, `val` = `{"loop","error"}`: a non-fatal poll error | `unifi/bridge/error` |
| `unifi/maintenance/set/loglevel` | — (in) | `error` \| `warn` \| `info` \| `debug`, not persisted | — |
| `unifi/maintenance/set/restart`  | — (in) | any: graceful stop, `connected` 0, exit 0 — only when supervised (`UNIFI_SUPERVISED`, else systemd/Kubernetes/container detection) | — |
| `unifi/maintenance/stats`   | ✅     | process statistics every `MQTT_STATS_INTERVAL` s (0 = off) | — |

`connected` and `info` deliberately live **under our own name**, not under
`homeassistant/` — the discovery prefix belongs to Home Assistant. `bridge`
is a literal first-level item beside `<site>`, so a site whose reference is
`bridge`, `alarm`, `security`, `system` or a function name (`connected`,
`status`, `set`, `get`, `info`, `meta`, `maintenance`) is refused at start
(the last also keeps the migration sweep's "second level is a function ⇒
new topic" rule sound).

The maintenance topics are on by default (`MQTT_MAINTENANCE`). Anyone who
may publish on the broker can then restart the daemon or raise its log
level; the only gate is the broker's ACLs, so on a broker that cannot be
secured they belong off.

### 5.2 Site health

All under `unifi/status/<site>/health/`; 1.x: `unifi/<site>/health/…`.

| Item                | Retain | `val`                        |
| ------------------- | ------ | ---------------------------- |
| `wan/state`         | ✅     | `ok` \| `warning` \| `error` |
| `wan/ip`            | ✅     | `"203.0.113.7"`              |
| `wan/latency_ms`    | ✅     | `12` (cleared when unknown)  |
| `wan/rx_bps`        | ✅     | `18400000`                   |
| `wan/tx_bps`        | ✅     | `2100000`                    |
| `wlan/state`        | ✅     | `ok` \| `warning` \| `error` |
| `lan/state`         | ✅     | ditto                        |
| `vpn/state`         | ✅     | ditto                        |
| `clients/total`     | ✅     | `47`                         |
| `clients/guest`     | ✅     | `3`                          |
| `attributes`        | ✅     | JSON aggregate for HA        |

### 5.3 Devices

Status under `unifi/status/<site>/device/<mac>/`, commands under
`unifi/set/<site>/device/<mac>/`; 1.x: both under `unifi/<site>/device/<mac>/`.

| Item                            | Direction | Retain | `val` / payload                       | 1.x |
| ------------------------------- | --------- | ------ | ------------------------------------- | --- |
| `state`                         | out       | ✅     | `ONLINE` \| `OFFLINE` \| …            | same item |
| `online`                        | out       | ✅     | `true` \| `false` (state is `ONLINE`) | new |
| `uptime`                        | out       | ✅     | seconds                               | |
| `cpu_utilization`               | out       | ✅     | `12.5`                                | |
| `memory_utilization`            | out       | ✅     | `48`                                  | `48.0` |
| `uplink_tx_bps`                 | out       | ✅     | `1048576`                             | |
| `uplink_rx_bps`                 | out       | ✅     | `2097152`                             | |
| `firmware`                      | out       | ✅     | `"7.0.25"`                            | |
| `update_available`              | out       | ✅     | `true` \| `false`                     | `ON`/`OFF` |
| `locate`                        | out       | ✅     | `true` \| `false` (classic)           | `ON`/`OFF` |
| `attributes`                    | out       | ✅     | JSON: model, IP, type, uplink, adoption time | |
| `port/<idx>/state`              | out       | ✅     | `UP` \| `DOWN`                        | |
| `port/<idx>/speed`              | out       | ✅     | Mbit/s                                | |
| `port/<idx>/poe`                | out       | ✅     | `true` \| `false`                     | `ON`/`OFF` |
| `port/<idx>/poe/power_w`        | out       | ✅     | `7.4` (classic layer only)            | |
| `radio/<band>/channel`          | out       | ✅     | `36`                                  | |
| **`cmd/restart`**               | **in**    | —      | any non-empty payload → restart       | same |
| **`cmd/locate`**                | **in**    | —      | boolean (§5.6)                        | `cmd/locate/set` |
| **`port/<idx>/cmd/power_cycle`**| **in**    | —      | any non-empty payload                 | same |

### 5.4 Clients

Status under `unifi/status/<site>/client/<mac>/`, commands under
`unifi/set/<site>/client/<mac>/`. Clients without a MAC (VPN, Teleport)
are keyed by their API id instead.

| Item                | Direction | Retain | `val` / payload                                             | 1.x |
| ------------------- | --------- | ------ | ----------------------------------------------------------- | --- |
| `state`             | out       | ✅     | `home` \| `not_home`                                        | same |
| `online`            | out       | ✅     | `true` \| `false` (`state` is `home`)                       | new |
| `ip`                | out       | ✅     | `"192.168.1.42"`, cleared while away                        | |
| `signal`            | out       | ✅     | dBm (classic layer only)                                    | |
| `attributes`        | out       | ✅     | JSON: type, SSID, VLAN, network, uplink device, connected since | |
| `blocked`           | out       | ✅     | `true` \| `false`                                           | `ON`/`OFF` |
| **`blocked`**       | **in**    | —      | boolean (§5.6)                                              | `blocked/set` |
| **`cmd/authorize`** | **in**    | —      | `{}` for the site default, `{"minutes": 60}` or `60`        | empty payload allowed |

The ADR names `online` for infrastructure devices only. Clients get one
too, for the same job: their `ip` and `signal` sensors went unavailable
with the client in 1.x by reading `state` through a template, and an
`online` boolean lets every second availability entry have the one shape
spec §8 describes. It is published with every client publish, from the
first one on, so no entity can wait for it.

### 5.5 WLANs

| Item (`unifi/{status,set}/<site>/wlan/<id>/`) | Direction | Retain | `val` / payload   | 1.x |
| ------------------------------------------- | --------- | ------ | ----------------- | --- |
| `enabled`                                   | out       | ✅     | `true` \| `false` | `ON`/`OFF` |
| `name`                                      | out       | ✅     | the SSID          | |
| **`enabled`**                               | **in**    | —      | boolean (§5.6)    | `enabled/set` |

### 5.6 Conventions

- **One status item per entity value** instead of JSON blobs — one
  `state_topic` per sensor reading `{{ value_json.val }}`.
- **Every status item is a status object** (spec §5.2):
  `{"val": …, "ts": …, "lc": …}`, `ts` the observation and `lc` the last
  change, integer milliseconds. Booleans are JSON booleans, numbers JSON
  numbers, states their English token. An empty retained payload means "no
  value".
- **`attributes` items** carry a JSON object as `val`, which HA binds as
  `json_attributes_topic` through `{{ value_json.val | tojson }}`. That is
  where things go which are useful to see but pointless as their own entity.
- **Every status item is `retain: true`**, no command is; `bridge/error` is
  an event and not retained either.
- **Commands** (spec §3.3, §5.3) take a plain value or `{"val": …}`;
  booleans read `true/false`, `1/0`, `on/off`, `yes/no` in any case and
  anything else is rejected; actions fire on any non-empty payload. Empty
  payloads and retained messages are dropped: on (re)subscribe the broker
  re-delivers the last retained command, and a stale `mosquitto_pub -r`
  would otherwise trigger a real port power-cycle on every daemon start. A
  rejected or failed command is logged at warn with topic and payload.
- **QoS 0 for status, QoS 1 for the command subscriptions, discovery and
  `connected`.** A lost status value is corrected by the next change or the
  next reconnect's replay; a lost command is a visible failure.

### 5.7 Migration from 1.x

On every start of 2.x the daemon clears the retained topics 1.x left under
its own name, after the first poll cycles have said what it owns. It reads
`unifi/<site>/#` and `unifi/bridge/+` for a few seconds and clears, with an
empty retained payload, only exact 1.x shapes: `bridge/status`,
`bridge/info`, the site's health items, and the 1.x items of every device,
client and SSID this run polled from its own console. Never a prefix match,
never a topic whose second level is a function name. Another name, another
site and an unknown MAC are never touched, so a second console bridged
under the same name keeps its leftovers. The sweep is idempotent and goes
with 3.0.

Before the sweep, once each class's source has reported, the daemon reads
the retained discovery configs back and settles every one it wrote in the
1.x layout that the first poll cycles did not replace (2.0.1,
`internal/coordinator/repoint.go`). Ownership is exact: a node id and
`unique_id` this bridge mints, an availability list that is exactly
`<name>/bridge/status` (plus, for a device or client entity, its own 1.x
`state` with the 1.x template), and state, command and attributes topics
that are 1.x items of the same object under this name and site. Such a
config is re-pointed when the configuration produces the entity — from the
polled object, or for an absent client from the identity the config and
its retained 1.x items carry, with `state` `not_home` and `online` false
published for it — and retracted, with `coordinator.migration_retracted`,
when it does not: a device or SSID the console no longer has, a port or
radio that is gone, an entity whose option or source is switched off.
1.3.0 kept no client list and never retracted a client config; an absent
client lived on in its retained config and `bridge/status`, which 2.0.0
removed. The re-point goes with the sweep.

---

## 6. Home Assistant integration

### 6.1 Device and entity model

One **HA device per UniFi device**, another per site (carrying the health
entities), and optionally one per client.

```
Device "UniFi USW-Pro-24-PoE (Basement)"
  identifiers: unifi_<mac>
  connections: [["mac", "aa:bb:cc:dd:ee:ff"]]
  manufacturer: "Ubiquiti"
  model: "USW-Pro-24-PoE"
  sw_version: "7.0.25"
  via_device: unifi_<uplink-mac>      ← reproduces the topology inside HA
```

`via_device` is why `UplinkMAC` exists in the model: it lets HA draw the real
network hierarchy (client → AP → switch → gateway), which makes the device
page far more readable.

| UniFi object | HA platform      | Entities                                                    |
| ------------ | ---------------- | ------------------------------------------------------------ |
| Device       | `sensor`         | state, uptime, CPU, memory, uplink TX/RX, firmware           |
| Device       | `binary_sensor`  | reachable (`connectivity`), update available (`update`)      |
| Device       | `button`         | restart                                                      |
| Device       | `switch`         | locate LED                                                   |
| Port         | `binary_sensor`  | link up/down                                                 |
| Port (PoE)   | `sensor`         | PoE power (W) — classic layer only                           |
| Port (PoE)   | `button`         | power-cycle                                                  |
| Client       | `device_tracker` | presence                                                     |
| Client       | `sensor`         | signal strength — classic layer only                         |
| Client       | `switch`         | blocked — classic layer only                                 |
| Site         | `sensor`         | WAN IP, latency, throughput, client counts                   |
| Site         | `binary_sensor`  | WAN connectivity                                             |
| WLAN         | `switch`         | enabled                                                      |

`device_class` and `state_class` are set where they genuinely apply —
`data_rate`/`measurement` for throughput, `duration` for uptime,
`signal_strength` for dBm. `total_increasing` is used nowhere: UniFi reports
rates, not counters that stay monotonic across a reboot.

### 6.2 Naming and localisation

This follows the same rule as the sibling projects, and it exists to protect
entity history:

| Artefact                             | Source                                             | Localised? |
| ------------------------------------ | -------------------------------------------------- | ---------- |
| `unique_id`                          | `unifi_<mac>_<stable_english_key>`                 | **never**  |
| `default_entity_id`                  | `<platform>.slug(device name)_slug(english key)`   | **never**  |
| `name` (friendly)                    | the display name for `LANGUAGE` (`en` / `de`)      | **yes**    |
| MQTT topic segments                  | stable English keys                                | **never**  |
| Add-on configuration page            | `addon/translations/{en,de}.yaml`                  | **yes**    |

**`default_entity_id` is the only seed published.** Home Assistant's
MQTT discovery schemas are `extra=REMOVE_EXTRA`: a key a platform does
not declare is dropped on arrival, with nothing on the wire and nothing
in any log to say so. Measured against the schemas of Home Assistant
2026.9, `object_id` is accepted by 0 of the 32 MQTT platforms and
`default_entity_id` by 28, so publishing `object_id` accomplishes
nothing and only misleads whoever reads a retained config. Omitting
`default_entity_id` is exactly how German entity_ids appear.

The seed never renames an existing entity: Home Assistant tracks entities
by `unique_id`, so a seed only shapes the id an entity receives when it
is first created. Renaming a device in UniFi therefore changes the seed
for new entities and leaves established ones alone.

Consequence: switching `LANGUAGE` renames what the user sees in the UI but
never re-creates an entity, never changes an `entity_id`, and never orphans
history. A German user gets `sensor.usw_pro_24_cpu_utilization` displaying as
"CPU-Auslastung".

Every entity therefore carries a **stable key** (e.g. `cpu_utilization`,
`update_available`, `port_3_poe`) that is simultaneously the topic suffix,
part of the `unique_id`, and the lookup key into the translation table.
Translations live in a single table per language inside `internal/hass`; a
missing German string falls back to English rather than to an empty label.

### 6.3 Client filtering

The critical point: without filters an average home network produces 80–200
entities, a corporate network thousands. Filtering is therefore **mandatory
machinery**, not a convenience.

**Evaluation order** (first matching rule decides):

1. `EXCLUDE_MACS` — matches → never publish. Highest priority.
2. `INCLUDE_MACS` non-empty → **only** these MACs, all other filters are
   skipped. The explicit allowlist case.
3. `EXCLUDE_GUESTS` and the client is a guest → drop.
4. `TYPES`, `NETWORKS`, `VLANS`, `SSIDS`: every **non-empty** list must match
   (AND across dimensions, OR within one). An empty list means no restriction
   on that dimension.
5. `MAX` — hard cap. Once reached, further clients are skipped with a
   **single warning per poll cycle**, not silently dropped.

Example — "every wireless client on the IoT VLAN, but never the printer":

```yaml
CLIENTS:
  ENABLE: true
  TYPES: [WIRELESS]
  VLANS: [20]
  EXCLUDE_MACS: ["aa:bb:cc:11:22:33"]  # printer
  MAX: 50
```

Adding a single phone that lives on another VLAN needs the VLAN list widened
— `INCLUDE_MACS` would switch the VLAN rule off entirely. `INCLUDE_MACS` is
an either/or, not an additive. This is deliberately kept simple; a rule
engine with expressions would be overkill for a configuration field.

**Sort stability under `MAX`:** the client list is sorted by MAC before
truncation. Without that, API ordering would decide which 50 of 80 clients
get entities — and it changes on every poll, which HA answers with entities
constantly appearing and disappearing.

### 6.4 Presence semantics

`device_tracker` knows `home` and `not_home`. The mapping:

- Client appears in the poll → `home`
- Client missing from the poll → **not immediately** `not_home`, but only
  after `AWAY_TIMEOUT` (default 300 s).

Reason: wireless clients vanish for a cycle while roaming between APs, and
power-saving smartphones regularly drop out of the client list. Without a
grace period every presence automation flaps. An `AWAY_TIMEOUT` below
`2 × REFRESH_CLIENTS` is flagged with a warning during validation.

### 6.5 Availability and orphans

- Every entity's availability list starts with `unifi/connected`, available
  at `2` (`{{ 'online' if value | int(0) >= 2 else 'offline' }}`). If the
  daemon dies or loses the console, entities go `unavailable` in HA instead
  of freezing.
- **Two-stage availability:** device and client entities additionally read
  their object's `online` item (`{{ value_json.val | lower }}`, `true`
  available), `availability_mode: all`, so a switch that went offline does
  not sit there showing stale CPU figures. Each list entry carries only
  `topic`, `value_template`, `payload_available` and
  `payload_not_available` (spec §8).
- **Orphaned discovery configs** are reconciled on two levels, because the
  cheap one cannot see the interesting case.

  *Within a run*, the coordinator remembers which `config` topics each object
  published and clears (empty retained message) those whose object
  disappeared — a device removed from the site, a port that vanished.

  *Across runs*, that memory does not exist. A config written by an earlier
  version under a different key, by a device unplugged while the daemon was
  stopped, or by a filter that used to match more clients, stays retained on
  the broker: HA recreates the entity on every start and it sits `unavailable`
  forever with nothing to explain it. So on start the daemon opens a snapshot
  window on `<HASS_BASE_TOPIC>/#`, collects what the broker replays, and clears
  what it *claimed* but no longer publishes. `HASS_CLEANUP: false` disables it.

  **Ownership is recorded, not inferred.** The sweep does not ask the payload
  whose entity it is. It may clear a retained config only if *this process*
  published that exact topic since it started — `hass.Claims{Published,
  Announced}`, where `Published` is every config topic the broker accepted and
  `Announced` the subset still claimed as a live entity. `Published` only ever
  grows: a retraction does not un-claim, which is what lets a retraction that
  did not stick be retried. A config this process never published is **not ours
  to delete, whatever it looks like.**

  This is not belt-and-braces caution; it is the correction of a measured
  defect. The rule this section used to state — that ownership needs the
  `unifi_` id namespace *and* this bridge's availability topic to agree — was
  removed in PR #24 because it does not separate two instances of this daemon.
  A state topic here is `<name>/status/<site>/…`; the name is `MQTT_TOPIC`, which is
  exactly what the availability check already reads, and the site segment is
  `default` on every UniFi console out of the box. Two consoles bridged to one
  broker with the shipped configuration publish byte-identical config topics,
  `unique_id`s, availability topics and state topics for the whole site plane.
  `TestOwnershipCannotSeparateTwoConsolesOnOneRoot` drove it: one console's
  daemon deleted the other console's SSID switch out of Home Assistant. **An
  operator who read the old rule as a protection was reading a description of
  the mechanism that deleted their entities.**

  The payload shape test survives, but only as a *narrowing* half, never as a
  verdict: `Discovery.IsOwnConfig` keeps another integration's configs out of
  the sweep's judgement entirely, and confirms that what is retained under a
  claimed topic is still a config of ours rather than something another writer
  put on top of it. It compares the availability topic for **exact equality**,
  not by prefix, so an instance rooted at `unifi/kitchen` is not claimed by one
  rooted at `unifi`. It must never again decide a retraction on its own.

  **The cost is stated, not hidden.** A config genuinely left behind by an
  *earlier run of this daemon* — an older topic shape, a device unplugged while
  the daemon was stopped — was also not published by *this* process, so it is
  no longer cleared either. It is unreachable to the sweep and reported instead:

  - `coordinator.reconcile_unclaimed` (Info) — this daemon's shape, not
    claimed by this process, and the class that would have published it **has**
    reported. An operator who runs no second console can clear these by hand,
    one empty retained publish each; the log line says so.
  - `coordinator.reconcile_unclaimed_unready` (Warn, since PR #29) — the same
    shape with the opposite provenance: the class that would have published it
    has **not** reported in this run, so its absence from `Announced` may mean
    only that its source is down. With the classic layer failing, every
    site-health config this daemon publishes lands here. These are named as
    explicitly **not** safe to clear — otherwise a three-minute console outage
    becomes a fleet deleted by hand.

  A stale entity an operator can see and delete is a better outcome than a
  neighbour's fleet deleted silently, which is what the alternative did.

  **The sweep is gated per class** (device / client / site / WLAN) on that
  class's source having reported. An empty announced set means "not polled
  yet" until the corresponding poll succeeds, and sweeping on the other
  reading would delete every live entity along with its history. A device poll
  that returns *nothing* does not count as having reported — an empty list is
  far more often a permission problem than an empty site. A source that is
  switched off, by contrast, is ready immediately: turning `CLIENTS.ENABLE`
  off is a decision that those entities should go. A source that never
  succeeds (classic health against a console that rejects the login) stops
  holding the sweep back after a timeout, so the classes that did report are
  still tidied.
- **HA birth message:** the daemon subscribes to `homeassistant/status` and
  republishes the whole discovery `HASS_BIRTH_GRACETIME` seconds after HA
  announces `online`.

---

## 7. Configuration

### 7.1 Layers

```
defaults  →  config.yaml  →  UNIFI_* env  →  validation  →  typed Config
```

Env always wins, so the HA add-on path and `docker run -e ...` work with no
file at all. File search order: `--config`, then
`$XDG_CONFIG_HOME/unifi2mqtt/config.yaml`, then
`~/.config/unifi2mqtt/config.yaml`.

`config-template.yaml` is the annotated reference and ships with every
release.

### 7.2 Validation rules

Validation is where misconfiguration surfaces **at startup** rather than in
production:

| Rule                                                                     | Behaviour |
| ------------------------------------------------------------------------ | --------- |
| `HOST` and `API_KEY` set                                                 | error     |
| `MQTT_SERVER` set                                                        | error     |
| `CLASSIC_ENABLE` ⇒ `CLASSIC_USERNAME`/`CLASSIC_PASSWORD` set             | error     |
| `CLIENTS.SSIDS` non-empty ⇒ `CLASSIC_ENABLE`                             | error     |
| `CLIENTS.VLANS`/`NETWORKS` non-empty                                     | fine — works officially via IP-subnet mapping (§2.2) |
| `CONTROLS.CLIENT_BLOCK`/`WLAN_ENABLE`/`DEVICE_LOCATE` ⇒ `CLASSIC_ENABLE` | error     |
| `MQTT_SSL_INSECURE` without `MQTT_SSL`                                   | warning   |
| `VERIFY_TLS: false`                                                      | warning (once at startup) |
| `CLIENTS.AWAY_TIMEOUT < 2 × REFRESH_CLIENTS`                             | warning   |
| any `REFRESH_*` below 5 s                                                | error (rate-limit protection) |
| `CLIENTS.MAX` > 500                                                      | warning   |

The hard coupling "filter dimension ⇒ classic layer" matters: an SSID filter
without classic data would let every client through, because the field stays
empty — the opposite of what the user asked for.

### 7.3 Handling secrets

- `API_KEY`, `CLASSIC_PASSWORD`, `MQTT_PASSWORD`, `WEB_PASSWORD` are
  `config.Secret` — a type whose `String()` returns `***` and which redacts
  in `MarshalJSON`. No `slog.Any("cfg", cfg)` and no web UI endpoint can leak
  them by accident.
- Env overrides are the recommended path for secrets; the YAML variant is
  documented with a note about file permissions.
- The web UI only ever shows redacted configuration.

---

## 8. Polling design

### 8.1 Cadences

| Loop      | Default | Content                                                                    |
| --------- | ------- | -------------------------------------------------------------------------- |
| `clients` | 30 s    | client list → presence. Sets presence latency.                             |
| `devices` | 60 s    | device list (state, firmware, update available) + per-device statistics    |
| `health`  | 60 s    | site health (classic layer only)                                           |
| `static`  | 3600 s  | controller info, site list, network catalogue **with subnets**, WLAN catalogue, **device details** (ports, radios, uplink) |

Each loop is its own goroutine under an `errgroup`, as in `go-mtec2mqtt`. An
error in one loop does **not** stop the others; only an unrecoverable startup
failure aborts.

### 8.2 The N+1 problem, twice over

Three list endpoints need per-object follow-ups (§2.2), which dominates
the request budget:

| Data              | Requests            | Changes            | Belongs in    |
| ----------------- | ------------------- | ------------------ | ------------- |
| Device list       | 1                   | constantly (state) | `devices`     |
| Device details    | 1 per device        | rarely (cabling)   | `static`      |
| Device statistics | 1 per online device | constantly         | `device_stats`|
| Network list      | 1                   | rarely             | `static`      |
| Network details   | 1 per network       | rarely             | `static`      |

Naively polling everything on the device cadence would cost `1 + 2N`
requests per cycle — 25 per minute on the 12-device reference
installation, before clients. The split above brings the per-minute cost
down to `1 + N_online`, with the expensive-but-static parts on the hourly
loop.

Further mitigations:

- **Bounded worker pool** (4 concurrent) for every fan-out, instead of
  sequential or unbounded. A console also routes the household's traffic;
  opening 25 connections at once is impolite.
- **Skip offline devices** for the statistics call — they return nothing
  useful anyway.
- **Optional decoupling:** `REFRESH_DEVICE_STATS` may be set larger than
  `REFRESH_DEVICES` when the console is under load. The device list (cheap)
  stays fast, the statistics (expensive) go slower.
- **Classic optimisation:** with the classic layer active, a single
  `GET /stat/device` returns list, details *and* statistics. The facade
  takes that route automatically when available — one request instead of
  `1 + 2N`. The Integration API remains the fallback.

`firmwareVersion` and `firmwareUpdatable` are already in the device
**overview**, so the update sensor needs no detail call — which is why
device details can sit on the slow loop without making the most
interesting sensor stale.

### 8.3 Change detection

Publishing happens **only on value change** — the gate compares the status
object's `val`, never its timestamps — and again, unchanged with its
original `ts`, after every broker reconnect. Reason: a broker with many
subscribers and 200 clients × 5 topics every 30 s is pointless load when
nothing changed, and mqtt-smarthome 2.0 §3.2 says an adapter must not
republish unchanged state. 1.x also forced a full republish every
`FORCE_REPUBLISH` seconds; 2.0.0 removed it (ADR 0083): a subscriber that
missed a message has the retained value, and the reconnect replay restores
a broker that lost its retained store. The key is ignored if still set.

### 8.4 Rate limits and backoff

- `429` → honour `Retry-After`, pause the loop, warn.
- `5xx`/network errors → exponential backoff (1 s → 60 s) with jitter, then
  back to the normal cadence.
- `401` on the Integration API → **fatal for that loop**, logged loudly: the
  API key is invalid and retrying will not help.
- `401`/`api.err.LoginRequired` on the classic API → one re-login attempt,
  backoff if that fails too.

---

## 9. Error handling and resilience

| Situation                     | Behaviour                                                                                                                                   |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| Console unreachable           | backoff retry, publish `unifi/bridge/error`, entities keep their last (retained) value, bridge status stays `online`                         |
| Console gone for > 5 min      | additionally set every device availability to `offline`                                                                                     |
| API key invalid               | fatal at startup; at runtime: loud warning + stop that loop                                                                                 |
| Classic login fails           | warning, disable classic capabilities, the official path keeps running                                                                       |
| Unknown response field        | ignore (`json.Decoder` without `DisallowUnknownFields`)                                                                                     |
| Expected field missing        | zero value, debug log — not an error. Ubiquiti adds and removes fields.                                                                     |
| MQTT broker gone              | the `go-mqtt` lifecycle reconnects; the `Breaker` keeps publishes from hanging on the ack timeout                                            |
| MQTT broker degraded          | `mqtt.Breaker` opens after 5 failures, publishes fail fast                                                                                   |
| Actuator call fails           | log the error, re-read state from the console (no optimistic update)                                                                        |
| Unknown `DeviceState`         | pass through as `UNKNOWN`, keep the raw value in the `attributes` JSON                                                                       |

**No optimistic state updates:** when HA flips a switch, the command is
issued and then an out-of-band poll of the affected object is triggered. The
published state always comes from the console. A failed command therefore
lets the entity snap back to its old state in HA instead of lying
permanently.

---

## 10. Security

- **Least privilege:** with `CONTROLS.ENABLE: false` a read-only admin
  suffices for the API key. That is the documented recommendation.
- **TLS to the console:** `VERIFY_TLS: false` is the default because consoles
  serve a self-signed certificate on their LAN IP. The daemon warns about it
  once at startup. `CA_FILE` is the documented better alternative: trust the
  console's own certificate as an additional root and keep full verification.
- **No secrets in logs, topics or the web UI** (§7.3). Even HTTP-layer error
  messages are filtered before logging — a `*url.Error` can contain the full
  URL including its query.
- **Command topics carry no authorisation:** whoever may write to the broker
  can restart devices. That is inherent — the control belongs in the broker
  ACL, and the README says so.
- **Web UI binds to `127.0.0.1` by default** with optional basic auth. The
  add-on binds `0.0.0.0` but is only reachable through the Ingress proxy.
- **The container runs as `nonroot`** in the distroless image.
- `gosec` and CodeQL run in CI, `govulncheck` via `make vuln`.

---

## 11. Test strategy

### 11.1 Levels

| Level         | Tooling                                | Covers                                          |
| ------------- | -------------------------------------- | ------------------------------------------------ |
| Decoders      | golden JSON + `httptest.Server`        | response parsing, pagination, error formats      |
| HTTP layer    | `httptest.Server` with fault injection | retry, backoff, `429`, `401`, CSRF, re-login     |
| Filters       | table-driven unit tests                | client filter ordering, `MAX` truncation         |
| Discovery     | snapshot comparison of payloads        | HA entity definitions, `unique_id` stability     |
| Coordinator   | stub facade + fake MQTT                | poll sequence, change detection, orphan cleanup  |
| Presence      | injected clock                         | `AWAY_TIMEOUT` logic without waiting             |
| End-to-end    | `go-mqtt` mock broker + mock console   | startup path, LWT, reconnect                     |

### 11.2 Obtaining fixtures

`script/capture-fixtures.sh` calls every relevant endpoint with a real API
key, **redacts** MACs, IPs, serial numbers and names through a fixed
substitution scheme, and writes the result to
`internal/unifi/integration/testdata/`. That anchors the decoders to real
responses without identifying data entering the repository. It also stores
the console's own OpenAPI document so schema drift between Network versions
is visible.

Because the schema is already pinned by the published specification (§2.5),
the committed fixtures are hand-written against that schema where no capture
from real hardware exists yet — clearly marked as such in the file header.
Replacing them with captured output is a drop-in operation.

### 11.3 Not tested

Live operation against a real console is not automatable (there is no UniFi
test container). Actuator calls (restart, power-cycle) are verified against
the mock and once manually against real hardware — documented as a checklist
in the PR.

---

## 12. Roadmap

| Phase | Content                                                                                                            | Result                            |
| ----- | ------------------------------------------------------------------------------------------------------------------ | --------------------------------- |
| **0** | **Project setup** — Makefile, linters, CI, CodeQL, Dependabot, release workflows, Docker, HA add-on packaging, skeleton | ✅ done                        |
| **1** | **Spec verification (§2.5), `internal/model`, `internal/config`, `internal/unifi` + integration client, fixtures**   | ✅ done — `unifi2mqtt --once` reports the full site inventory |
| **2** | **`internal/coordinator` + MQTT publication, bridge LWT, change detection**                                        | ✅ done — 363 topics on the broker |
| **3** | **`internal/hass` — discovery for devices, ports; orphan cleanup, birth message, localisation table**              | ✅ done — 345 entities in HA        |
| **4** | **Clients: filter engine, presence with `AWAY_TIMEOUT`, `device_tracker` discovery**                               | ✅ done — 7 of 121 clients on the reference site |
| **5** | **`internal/unifi/classic` — login/CSRF, health, SSID/signal enrichment; SSID filter becomes available**           | ✅ done — site health + full filtering |
| **6** | **Actuators: command queue, buttons/switches, write-back with follow-up poll**                                     | ✅ done — verified on real hardware |
| **7** | **`internal/web` — diagnostic SPA, Ingress panel**                                                                 | ✅ done                            |
| **8** | **Documentation pass, release 1.0.0**                                                                              | ✅ done                            |
| **9** | Optional: WebSocket event stream as a poll accelerator (§2.4)                                                      | sub-second client event latency   |

Phase 5 is the only one depending on an unofficial interface and is therefore
deliberately late.

---

## 13. Open questions

**Resolved since the first draft:**

- ~~Exact Integration API field names~~ — settled via the OpenAPI spec
  (§2.5). Every type in §4 is bound to it.
- ~~PoE power draw per port~~ — settled: absent from the Integration API,
  classic layer only (§2.2 footnote 3).
- ~~Whether the list endpoints suffice~~ — settled by running against a
  live console: `/networks` and `/devices` both omit fields their detail
  counterparts carry, so both fan out (§2.2, §8.2).
- ~~Localisation scope~~ — settled: English entity_id seeds and `unique_id`s,
  localised friendly names (§6.2), matching the sibling projects.
- ~~Minimum Network version~~ — settled at **10.5**: the specs of 10.5.67 and
  10.6.90 are identical for every schema this project reads, and 10.5.67 is
  the reference installation. The daemon reads `GET /v1/info`
  (`applicationVersion`) at startup, logs it, and warns below 10.5 instead of
  refusing to run — an older console may still work, it is simply untested.
- ~~A curl-pipe systemd installer~~ — dropped. Docker, the Home Assistant
  add-on and the plain release binary cover the installation paths; a
  bespoke installer script is not maintained here.

**Still open:**

1. **Multi-site** — the concept assumes one site per daemon instance (the
   `SITE` key). The topic layout already carries `<site>`, so extending to
   several sites is additive. Whether it is needed is decided after phase 4.
2. **Toggling a WLAN through the official API** — `PUT /wifi/broadcasts/{id}`
   exists but expects the full configuration object. Whether a safe
   read-modify-write for just `enabled` is possible without silently
   resetting settings when the schema grows is assessed in phase 6. Until
   then: classic.

---

## Sources

- [Getting Started with the Official UniFi API — Ubiquiti Help Center](https://help.ui.com/hc/en-us/articles/30076656117655-Getting-Started-with-the-Official-UniFi-API)
- [UniFi Developer Portal](https://developer.ui.com/)
- [beezly/unifi-apis — archived OpenAPI specifications per Network version](https://github.com/beezly/unifi-apis)
- [uchkunr/unifi-best-practices — cross-flavour API reference](https://github.com/uchkunr/unifi-best-practices)
- [Ubiquiti Community Wiki — classic controller API](https://ubntwiki.com/products/software/unifi-controller/api)
- [go-mtec2mqtt](https://github.com/SukramJ/go-mtec2mqtt) — structural blueprint
- [go-mqtt](https://github.com/SukramJ/go-mqtt) — MQTT client
