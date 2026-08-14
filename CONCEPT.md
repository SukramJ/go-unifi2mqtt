# Concept: go-unifi2mqtt

Implementation concept for bridging a local UniFi Network installation to
MQTT. This document is the project's design reference — code follows it, not
the other way around.

**Last updated:** 2026-08-14 · **Status:** phases 0–3 done, phase 4 next

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

The tree is rooted at `MQTT_TOPIC` (default `unifi`) and keyed by site and
MAC address. `<site>` is the site identifier (`default`), `<mac>` the
normalised MAC without separators.

### 5.1 Bridge level

| Topic                 | Retain | Content                                                   |
| --------------------- | ------ | --------------------------------------------------------- |
| `unifi/bridge/status` | ✅     | `online` \| `offline` — LWT + birth                       |
| `unifi/bridge/info`   | ✅     | JSON: daemon version, controller version, detected flavour |
| `unifi/bridge/error`  | ❌     | JSON: last non-fatal error (diagnostics)                  |

The LWT deliberately lives **under our own root**, not under
`homeassistant/` — the discovery prefix belongs to Home Assistant, and a
bridge availability topic has no business there. (This intentionally differs
from `go-mtec2mqtt`, where it sits elsewhere for historical reasons.)

### 5.2 Site health

| Topic                                | Retain | Content                      |
| ------------------------------------ | ------ | ---------------------------- |
| `unifi/<site>/health/wan/state`      | ✅     | `ok` \| `warning` \| `error` |
| `unifi/<site>/health/wan/ip`         | ✅     | `203.0.113.7`                |
| `unifi/<site>/health/wan/latency_ms` | ✅     | `12`                         |
| `unifi/<site>/health/wan/rx_bps`     | ✅     | `18400000`                   |
| `unifi/<site>/health/wan/tx_bps`     | ✅     | `2100000`                    |
| `unifi/<site>/health/wlan/state`     | ✅     | `ok` \| `warning` \| `error` |
| `unifi/<site>/health/lan/state`      | ✅     | ditto                        |
| `unifi/<site>/health/vpn/state`      | ✅     | ditto                        |
| `unifi/<site>/health/clients/total`  | ✅     | `47`                         |
| `unifi/<site>/health/clients/guest`  | ✅     | `3`                          |
| `unifi/<site>/health/attributes`     | ✅     | JSON aggregate for HA        |

### 5.3 Devices

| Topic                                                     | Direction | Retain | Content                              |
| --------------------------------------------------------- | --------- | ------ | ------------------------------------- |
| `unifi/<site>/device/<mac>/state`                         | out       | ✅     | `ONLINE` \| `OFFLINE` \| …            |
| `unifi/<site>/device/<mac>/uptime`                        | out       | ✅     | seconds                               |
| `unifi/<site>/device/<mac>/cpu_utilization`               | out       | ✅     | `12.5`                                |
| `unifi/<site>/device/<mac>/memory_utilization`            | out       | ✅     | `48.0`                                |
| `unifi/<site>/device/<mac>/uplink_tx_bps`                 | out       | ✅     | `1048576`                             |
| `unifi/<site>/device/<mac>/uplink_rx_bps`                 | out       | ✅     | `2097152`                             |
| `unifi/<site>/device/<mac>/firmware`                      | out       | ✅     | `7.0.25`                              |
| `unifi/<site>/device/<mac>/update_available`              | out       | ✅     | `ON` \| `OFF`                         |
| `unifi/<site>/device/<mac>/attributes`                    | out       | ✅     | JSON: model, IP, type, uplink, adoption time |
| `unifi/<site>/device/<mac>/port/<idx>/state`              | out       | ✅     | `UP` \| `DOWN`                        |
| `unifi/<site>/device/<mac>/port/<idx>/speed`              | out       | ✅     | Mbit/s                                |
| `unifi/<site>/device/<mac>/port/<idx>/poe`                | out       | ✅     | `ON` \| `OFF`                         |
| `unifi/<site>/device/<mac>/port/<idx>/poe/power_w`        | out       | ✅     | `7.4` (classic layer only)            |
| `unifi/<site>/device/<mac>/radio/<band>/channel`          | out       | ✅     | `36`                                  |
| **`unifi/<site>/device/<mac>/cmd/restart`**               | **in**    | —      | any payload → restart                 |
| **`unifi/<site>/device/<mac>/cmd/locate/set`**            | **in**    | —      | `ON` \| `OFF`                         |
| **`unifi/<site>/device/<mac>/port/<idx>/cmd/power_cycle`**| **in**    | —      | any payload                           |

### 5.4 Clients

| Topic                                         | Direction | Retain | Content                                                    |
| --------------------------------------------- | --------- | ------ | ----------------------------------------------------------- |
| `unifi/<site>/client/<mac>/state`             | out       | ✅     | `home` \| `not_home`                                        |
| `unifi/<site>/client/<mac>/ip`                | out       | ✅     | `192.168.1.42`                                              |
| `unifi/<site>/client/<mac>/signal`            | out       | ✅     | dBm (classic layer only)                                    |
| `unifi/<site>/client/<mac>/attributes`        | out       | ✅     | JSON: type, SSID, VLAN, network, uplink device, connected since |
| `unifi/<site>/client/<mac>/blocked`           | out       | ✅     | `ON` \| `OFF`                                               |
| **`unifi/<site>/client/<mac>/blocked/set`**   | **in**    | —      | `ON` \| `OFF`                                               |
| **`unifi/<site>/client/<mac>/cmd/authorize`** | **in**    | —      | JSON `{"minutes": 60}` or empty for the default             |

### 5.5 WLANs

| Topic                                    | Direction | Retain | Content       |
| ---------------------------------------- | --------- | ------ | ------------- |
| `unifi/<site>/wlan/<id>/enabled`         | out       | ✅     | `ON` \| `OFF` |
| `unifi/<site>/wlan/<id>/name`            | out       | ✅     | the SSID      |
| **`unifi/<site>/wlan/<id>/enabled/set`** | **in**    | —      | `ON` \| `OFF` |

### 5.6 Conventions

- **Scalar topics instead of JSON blobs** for anything that becomes an HA
  entity — one `state_topic` per sensor, no `value_template` maze.
- **`attributes` topics** additionally carry a JSON object that HA binds as
  `json_attributes_topic`. That is where things go which are useful to see
  but pointless as their own entity.
- **Every state topic is `retain: true`**, no command topic is. A daemon
  restart therefore does not leave HA sitting on `unavailable`.
- **Commands are handled with a retain check.** On (re)subscribe the broker
  re-delivers the last retained command; the handler drops messages with the
  retain flag set, otherwise a stale `mosquitto_pub -r` would trigger a real
  port power-cycle on every daemon start. (`go-mqtt` exposes the flag as
  `Message.Retain`.)
- **QoS 0 for state, QoS 1 for commands and discovery.** State is republished
  cyclically anyway; a lost command, by contrast, is a visible failure.

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

| Artefact             | Source                                            | Localised? |
| -------------------- | ------------------------------------------------- | ---------- |
| `unique_id`          | `unifi_<mac>_<stable_english_key>`                | **never**  |
| `entity_id` seed     | slugified **English** name                        | **never**  |
| `name` (friendly)    | the display name for `LANGUAGE` (`en` / `de`)     | **yes**    |
| MQTT topic segments  | stable English keys                               | **never**  |

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

- Every entity gets `availability_topic: unifi/bridge/status`. If the daemon
  dies, entities go `unavailable` in HA instead of freezing.
- **Two-stage availability:** device entities additionally use their own
  `state` topic as an availability source, so a switch that went offline does
  not sit there showing stale CPU figures.
- **Orphaned discovery configs** are reconciled at startup and after every
  structural-change poll: the coordinator remembers which `config` topics it
  published and deletes (empty retained message) those whose object
  disappeared. A removed device would otherwise leave a dead entity in HA
  forever.
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

Publishing happens **only on value change**, plus a forced full republish
every `FORCE_REPUBLISH` seconds (default 600). Reason: a broker with many
subscribers and 200 clients × 5 topics every 30 s is pointless load when
nothing changed. The periodic full pass catches subscribers without retained
support drifting permanently stale.

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
| **4** | Clients: filter engine, presence with `AWAY_TIMEOUT`, `device_tracker` discovery                                   | presence detection                |
| **5** | `internal/unifi/classic` — login/CSRF, health, SSID/signal enrichment; SSID filter becomes available               | site health + full filtering      |
| **6** | Actuators: command queue, buttons/switches, write-back with follow-up poll                                         | control from HA                   |
| **7** | `internal/web` — diagnostic SPA, Ingress panel                                                                     | web UI                            |
| **8** | Documentation pass, release 1.0.0                                                                                  | Docker + add-on + binary released |
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
