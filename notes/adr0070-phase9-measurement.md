# ADR 0070 phase 9 — measurement for go-unifi2mqtt

- Status: measurement, not a decision
- Date: 2026-09-13
- Subject: [ADR 0070](https://github.com/SukramJ/openccu-loom/blob/main/docs/adr/0070-shared-ha-discovery-model-module.md)
  and its rollout table, row *"9 | `go-unifi2mqtt` (1537) | Proves hardware
  expansion and dynamic entity sets"*
  (`notes/concepts/shared-ha-discovery-model.md:871`)
- Measured against: this repository at `origin/main` (`48534d9`),
  `github.com/SukramJ/go-hamqtt` v0.32.0 (`7efac2f`),
  `github.com/SukramJ/go-mqtt` v1.3.0 (consumed before this PR) / v1.5.1
  (bumped to here)
- Precedents: go-zendure2mqtt (phase 5, PRs #39–#45), go-mtec2mqtt's
  `notes/adr0070-phase6-measurement.md` (phase 6, F1–F11),
  go-homeconnect2mqtt's `notes/adr0070-phase7-measurement.md` (phase 7, F1–F12)
  and go-daikin2mqtt's `notes/adr0070-phase8-measurement.md` (phase 8, F1–F14)

This document measures what go-unifi2mqtt actually puts on an MQTT broker
today: every discovery payload, every topic, every identity string, the
availability model, the delivery guarantee, and every place the same topic is
composed more than once. It takes no decision and it fixes nothing.

No production Go file was modified by the measurement. What this PR adds is the
*pins* — two test files plus their goldens, a `.gitattributes`, and the
`go-mqtt` bump — because the precedent from phases 5–8 is that defects are
corrected in their own step *before* the migration, so the later byte-equality
proof compares against corrected bytes.

> **Why `notes/`.** This repository's root-level documents (`CONCEPT.md`,
> `README.md`, `changelog.md`, `AI_POLICY.md`) are all operator- or
> design-facing and ship with the release. This is a working document for one
> programme, superseded when phase 9 ends. Phases 6, 7 and 8 all made the same
> call; `notes/` did not exist here and is created by this PR.

---

## 1. What is actually there

### 1.1 The repository

```sh
git ls-files '*.go' | grep -v _test.go | xargs cat | wc -l   # 9 952
git ls-files '*_test.go'                | xargs cat | wc -l   # 7 558   (at origin/main)
```

| Package | Go non-test | Go test |
| --- | ---: | ---: |
| `cmd/unifi2mqtt` | 626 | 109 |
| `internal/config` | 985 | 870 |
| `internal/coordinator` | 2 929 | 3 295 |
| **`internal/hass`** | **1 528** | **1 406** |
| `internal/model` | 585 | 318 |
| `internal/state` | 285 | 219 |
| `internal/unifi` | 866 | 801 |
| `internal/unifi/classic` | 702 | 533 |
| `internal/unifi/integration` | 985 | 1 032 |
| `internal/web` | 433 | 283 |
| `internal/version` | 28 | 26 |

### 1.2 The 1537 LOC figure

The rollout table says 1537 and attributes it to `internal/hass`. Measured,
`internal/hass` is **1 528** lines across six files — right about what it
covers and nine lines stale, the same cause as zendure's 375, mtec's 591,
homeconnect's 716 and daikin's 846.

It is also, as in every prior phase, about half the real addressable surface.
Measured by brace-matched function extents and whole files where the whole file
is topic or payload construction:

| Where | Lines | What it owns |
| --- | ---: | --- |
| `internal/hass/{discovery,control,client,health,cleanup,i18n}.go` | 1 528 | every discovery payload, all four config-topic composers, the ownership/orphan model, the localisation table |
| `internal/coordinator/command.go` | 393 | the six command-topic filters and the parser that is the second copy of the command suffix vocabulary |
| `internal/coordinator/discovery.go` | 282 | announce/clear/re-announce, the HA birth watch, the control gating |
| `internal/coordinator/publish.go` | 258 | change detection, forced republish, and every QoS/retain decision |
| `internal/coordinator/reconcile.go` | 240 | the retained-config sweep and its readiness gate |
| `internal/coordinator/topics.go` | 209 | the topic tree and the key constants that double as entity keys |
| `internal/coordinator/coordinator.go` (9 funcs) | 233 | availability, bridge info/error, the three poll loops that publish |
| `internal/coordinator/device.go` (6 funcs) | 135 | device, port, radio and WLAN state |
| `internal/coordinator/client.go` (5 funcs) | 103 | presence, client metadata, client discovery |
| `internal/coordinator/health.go` (2 funcs) | 65 | the site-health plane |
| `cmd/unifi2mqtt/main.go` MQTT wiring | ~40 | the will, the birth hook, the breaker, `hass.New` |
| **Total** | **≈ 3 486** | |

**3 486, not 1 537 — a factor of 2.3**, the largest ratio in the programme
(zendure 1.8×, mtec 1.6×, homeconnect 1.8×, daikin 1.9×). The reason is
structural: this is the only bridge whose published surface is *dynamic* in
both directions — entities appear and disappear with ports, radios, SSIDs and
roaming clients — so the announce/clear/reconcile machinery is as large as the
payload builder itself.

### 1.3 Dependency state

```
require golang.org/x/sync v0.22.0
require gopkg.in/yaml.v3 v3.0.1
require github.com/SukramJ/go-mqtt v1.3.0   -> bumped to v1.5.1 in this PR
```

No `go-hamqtt`, no `go-ha-catalog`. go-hamqtt v0.32.0 requires go-mqtt v1.5.1,
so the bump is a prerequisite and is taken here, alone, where it can be seen to
move nothing: the pins were generated before it and pass after it.

**Subscriptions the daemon makes:**

| Filter | QoS | When | Source |
| --- | ---: | --- | --- |
| `<hass_base>/status` | 1 | always, with discovery on | `internal/coordinator/discovery.go:136` |
| `homeassistant/+/+/+/config` | 1 | transient (5 s, one reconcile) | `reconcile.go` via `hass.ConfigFilter` |
| `unifi/<site>/device/+/cmd/restart` | 1 | `CONTROLS.ENABLE` | `command.go:110` |
| `unifi/<site>/device/+/cmd/locate/set` | 1 | `CONTROLS.ENABLE` | `command.go:111` |
| `unifi/<site>/device/+/port/+/cmd/power_cycle` | 1 | `CONTROLS.ENABLE` | `command.go:112` |
| `unifi/<site>/client/+/blocked/set` | 1 | `CONTROLS.ENABLE` | `command.go:113` |
| `unifi/<site>/client/+/cmd/authorize` | 1 | `CONTROLS.ENABLE` | `command.go:114` |
| `unifi/<site>/wlan/+/enabled/set` | 1 | `CONTROLS.ENABLE` | `command.go:115` |

`HASS_ENABLE` defaults to **false** (`internal/config/config.go:136` has no
default entry): discovery is opt-in, as on daikin. `CLIENTS.ENABLE` and
`CONTROLS.ENABLE` are off by default too, so the shipped default is a read-only
infrastructure bridge.

---

## 2. What it publishes

### 2.1 The shape

Retained JSON at `<HASS_BASE_TOPIC>/<platform>/<node_id>/<key>/config`, one
config per entity, built by **four** separate config-topic composers:

| Builder | File | Produces |
| --- | --- | --- |
| `Discovery.configTopic` | `internal/hass/discovery.go:500` | every device, port and radio entity, and both device controls |
| `Discovery.clientConfigTopic` | `internal/hass/client.go:184` | the tracker, the IP and signal sensors, the block switch, the authorize button |
| `Discovery.Health` (inline) | `internal/hass/health.go:99` | the seven site-health entities |
| `Discovery.WLANControl` (inline) | `internal/hass/control.go:211` | one switch per SSID |

State at `unifi/<site>/{device,client,wlan,health}/…`, commands at the same
paths with a `cmd/…` or `…/set` suffix, a per-object `…/attributes` sibling,
and the bridge plane at `unifi/bridge/{status,info,error}`.

### 2.2 The entity census

Measured by running the real coordinator (`OnConnect` → `refreshStatic` →
`refreshDevices` → `refreshDeviceStats` → `refreshClients` → `refreshHealth`)
against five scenarios with a recorder in place of the broker. Nothing between
the console and the broker is stubbed. Numbers held as Go literals in
`TestSurfaceCensus`.

| Scenario | Entities | Messages | Platform mix |
| --- | ---: | ---: | --- |
| `minimal.en` (default config + `HASS_ENABLE`) | 45 | 92 | binary_sensor 11, sensor 34 |
| `minimal.de` | 45 | 92 | identical |
| **`full.en`** (clients + all six controls + classic) | **75** | **147** | binary_sensor 12, button 6, device_tracker 3, sensor 45, switch 9 |
| `full.de` | 75 | 147 | identical |
| `nonascii.de` (the same fleet, German names) | 75 | 147 | identical |

The fleet behind it: four devices (a gateway, a 2-port PoE switch, an online
dual-radio AP, an offline AP), four clients (wireless, wired, guest, and a
MAC-less VPN client), two SSIDs, one site-health aggregate. Between them they
reach every branch of `deviceSpecs`, `portSpecs`, `radioSpecs`, `healthSpecs`,
`Client`, `ClientControls`, `DeviceControls` and `WLANControl`.

**The entity count does not move with `LANGUAGE` in any scenario** — only the
display `name`. Asserted directly, `en` against `de`, by
`TestIdentityIsLanguageIndependent`: config topic, `unique_id`,
`default_entity_id`, state topic, command topic and `device.identifiers` are
byte-identical across the two languages for all 120 en/de config pairs. This
bridge is the **first in the programme where that invariant holds with no
exceptions** (mtec, homeconnect and daikin each had at least one entity whose
entity-id seed moved with the language).

The count *does* move with hardware: 45 → 75 entities for the same fleet once
clients and controls are on, and every port, radio, SSID and roaming client
adds or removes entities at runtime. That is the "dynamic entity sets" the
rollout table nominates this bridge to prove, and it is why the
announce/clear/reconcile half of the surface is as big as the builder half.

### 2.3 The topic form — **five segments, with a node id**

```
homeassistant/sensor/unifi_00005e005301/cpu_utilization/config
homeassistant/binary_sensor/unifi_00005e005302/port_1_poe/config
homeassistant/button/unifi_00005e005302/port_1_power_cycle/config
homeassistant/device_tracker/unifi_client_00005e005311/presence/config
homeassistant/sensor/unifi_site_default/wan_latency/config
homeassistant/switch/unifi_site_default/wlan_w1111111-1111-4111-8111-111111111111/config
```

Verified against **all 315 config topics** across the five pinned scenarios by
`TestConfigTopicForm`: 315 of 315 are exactly five segments,
`<prefix>/<platform>/<node_id>/<object_id>/config`, and in **315 of 315 the
node-id segment is byte-equal to the payload's own `device.identifiers[0]`**.
`strings.Join` at `discovery.go:500`, `client.go:184`, and inline
concatenation at `health.go:99` and `control.go:211`.

**This matches homeconnect and inverts zendure, mtec and daikin.** It is the
single most consequential fact for step 6, and it is the *good* direction:
`publisher.SupersededTopics` with no `forms` argument defaults to
`[]LegacyTopicFunc{LegacyTopicWithNodeID}`, which renders
`<prefix>/<platform>/<node_id>/<object_id>/config` — exactly this shape. The
default is therefore the right *form* for this bridge, but only under a
condition that has to be met deliberately, not assumed — see [F5](#f5).

Two things do **not** line up and are the reason F5 is not a no-op:

- `unique_id != <node_id>_<object_id>` on **21 of 315** configs. The three
  shapes are pinned individually:

  | Config | `node_id` | `object_id` | `unique_id` |
  | --- | --- | --- | --- |
  | client IP sensor | `unifi_client_<key>` | `ip` | `unifi_client_<key>_client_ip` |
  | client signal sensor | `unifi_client_<key>` | `signal` | `unifi_client_<key>_client_signal` |
  | SSID switch | `unifi_site_<site>` | `wlan_<uuid>` | `unifi_wlan_<uuid>_enabled` |

  The first two come from `renderClientSensor` (`client.go:122`) using `s.key`
  for the id and a separate `suffix` argument for the topic; the third from
  `WLANControl` (`control.go:176`, `:211`) putting the switch on the *site*
  node while namespacing its id under `wlan`. None of this is a defect today —
  Home Assistant keys on `unique_id` and never reads the topic segments — but
  it means `LegacyTopicByUniqueID` would retract nothing and any step-4
  component key has to be the *object-id* segment, not a slug of the id.

- `Discovery.ConfigFilter()` is **`homeassistant/+/+/+/config`**
  (`cleanup.go:76`), a strictly five-segment filter. A device bundle at
  `homeassistant/device/<node>/config` is four segments and therefore invisible
  to this daemon's own orphan reconcile, in both directions. Pinned by
  `TestConfigFilterMatchesEveryConfigTopic`. (mtec's F11 and daikin's F5 second
  bullet, same shape, different cause.)

### 2.4 Two measured payloads, verbatim

A device sensor (`full.en`, decoded from the golden):

```json
{
  "name": "CPU utilization",
  "unique_id": "unifi_00005e005301_cpu_utilization",
  "default_entity_id": "sensor.gateway_cpu_utilization",
  "state_topic": "unifi/default/device/00005e005301/cpu_utilization",
  "unit_of_measurement": "%",
  "state_class": "measurement",
  "entity_category": "diagnostic",
  "icon": "mdi:cpu-64-bit",
  "json_attributes_topic": "unifi/default/device/00005e005301/attributes",
  "availability": [
    {"topic": "unifi/bridge/status"},
    {"topic": "unifi/default/device/00005e005301/state",
     "value_template": "{{ 'online' if value == 'ONLINE' else 'offline' }}"}
  ],
  "availability_mode": "all",
  "device": {
    "identifiers": ["unifi_00005e005301"],
    "connections": [["mac", "00:00:5e:00:53:01"]],
    "name": "Gateway",
    "manufacturer": "Ubiquiti",
    "model": "UCG-Ultra",
    "sw_version": "4.3.6"
  }
}
```

A control — the shape `discovery.ValidateBody` refuses ([F2](#f2)):

```json
{
  "name": "Restart",
  "unique_id": "unifi_00005e005301_restart",
  "default_entity_id": "button.gateway_restart",
  "state_topic": "",          <- not a valid key for platform "button"
  "optimistic": false,        <- not a valid key for platform "button"
  "command_topic": "unifi/default/device/00005e005301/cmd/restart",
  "payload_press": "PRESS",
  "entity_category": "config",
  "icon": "mdi:restart",
  "availability": [ … two sources … ],
  "availability_mode": "all",
  "device": { "identifiers": ["unifi_00005e005301"], … }
}
```

`via_device` is set wherever an uplink is known — `unifi_<uplink mac>` on
`deviceInfo` (`discovery.go:419`) and on `clientDeviceInfo` (`client.go:174`) —
so the whole gateway → switch → AP → client topology reaches Home Assistant's
device page. No other bridge in the programme publishes a device graph, and it
is the one payload feature step 4 has to carry that the prior four phases never
exercised.

### 2.5 The whole topic tree

For `full.en` — the richest installation measured — 147 retained messages on
147 distinct topics:

| Plane | Topics |
| --- | ---: |
| discovery configs (`homeassistant/…/config`) | 75 |
| object state and attributes (`unifi/default/…`) | 71 |
| bridge (`unifi/bridge/status`, `unifi/bridge/info`) | 2 (a third, `unifi/bridge/error`, only on a loop failure) |

Of those, **6 state topics are read by no published entity** and **4
advertised state topics are written by nobody** — see [F3](#f3) and
[F1](#f1). The 15 command topics of `full.en` (45 across the five scenarios) are
advertised and subscribed but never
written by this daemon, which is correct: they are inbound.

### 2.6 Retain and QoS

Read off the transport call by `TestPublishQoSAndRetain`, over all 315 recorded
publishes per run:

| Plane | QoS | Retain | Where |
| --- | ---: | --- | --- |
| discovery configs (and clears) | **1** | true | `publish.go:194` |
| bridge availability (`online`/`offline`, and the will) | **1** | true | `coordinator.go:270`, `:283`, `main.go:243` |
| everything else — device, port, radio, client, WLAN, health, `bridge/info` | **0** | true | `publish.go:122`, `:150` |
| `bridge/error` | 0 | **false** | `coordinator.go:693` (not reached in any pinned scenario) |

**This is the only bridge in the programme that does not publish everything at
one QoS.** Discovery at QoS 1 and state at QoS 0 is a deliberate split, stated
at `coordinator/discovery.go:21-24`: a lost config leaves a device silently
missing from Home Assistant, while a lost state value is republished on the
next poll.

That makes the migration trap sharper here than on the four predecessors:
`publisher.QoS`'s zero value is `QoSUnset` and resolves to **QoS 1**. A
`publisher.Config{}` left unset would silently move this bridge's 71 state
publishes per `full` cycle (305 across the five pinned scenarios) from QoS 0 to
QoS 1 — and, unlike on daikin, a reviewer
comparing against "the bridge publishes at QoS 1" would find that half right
and stop looking. Deliberate QoS 0 is the sentinel
`publisher.QoSAtMostOnce` (`0x80`) and has to be written explicitly in
`StateConfig.QoS` while `Config.QoS` and `AvailabilityConfig.QoS` stay at 1.
See [F9](#f9).

Retain is uniform: every publish the daemon makes in a normal cycle is
retained, including the discovery clears (an empty retained payload is how a
retained message is deleted). The one unretained topic is `bridge/error`, and
the comment at `coordinator.go:679` says why.

---

## 3. Identity — what the library can reproduce byte for byte

Home Assistant keys the entity registry on `(domain, platform, unique_id)` and
the device registry on `identifiers`. Neither has a migration path.

### 3.1 The strings

| String | Formula | Source |
| --- | --- | --- |
| `unique_id`, device entity | `"unifi_" + mac + "_" + key` | `discovery.go:356` |
| `unique_id`, device control | `"unifi_" + mac + "_" + key` | `control.go:225`, `:261` |
| `unique_id`, client entity | `"unifi_client_" + clientKey + "_" + s.key` | `client.go:86`, `:123` |
| `unique_id`, client control | `"unifi_client_" + clientKey + "_" + ("blocked"\|"authorize")` | `control.go:112`, `:142` |
| `unique_id`, site health | `"unifi_site_" + site + "_" + key` | `health.go:71` |
| `unique_id`, SSID switch | `"unifi_wlan_" + wlanID + "_enabled"` | `control.go:176` |
| `device.identifiers[0]` | `"unifi_" + mac` / `"unifi_client_" + clientKey` / `"unifi_site_" + site` | `discovery.go:496`, `client.go:182`, `health.go:106` |
| `device.connections[0]` | `["mac", <colon mac>]` | `discovery.go:412`, `client.go:166` |
| `device.via_device` | `"unifi_" + uplinkMAC` | `discovery.go:419`, `client.go:174` |
| `default_entity_id` | `<platform> + "." + collapseTokens(slugify(device.Name) + "_" + slugify(key))` | `discovery.go:438` |
| state topic | `unifi/<site>/device/<mac>/<key>` etc. | `topics.go:112`, `:141`, `:147`, `:164` |
| command topic | the same with a `cmd/…` or `…/set` suffix | `control.go:59`, `:69`, `:85`, `:127`, `:159`, `:200` |

Four properties worth naming, three of them *better* than the precedents:

1. **The `unifi_` namespace is a compile-time literal** (`discovery.go:55`),
   not a configurable MQTT root. Changing `MQTT_TOPIC` orphans nothing. This
   is mtec's and daikin's strength and zendure's documented defect (ADR 0070
   §2.2).
2. **Identity is MAC-derived, never name-derived.** `unique_id` and
   `device.identifiers` carry no display name, no localised string and no API
   UUID (`discovery.go:493-496` says why: UUIDs change on re-adopt). Renaming a
   device in UniFi moves nothing in either registry.
3. **`default_entity_id` is fully language-independent**, verified en against
   de on all 120 config pairs. This is the invariant mtec, homeconnect and
   daikin each violated somewhere; here it holds.
4. **The slug is where the library cannot follow.** `slugify`
   (`discovery.go:458`) transliterates `ä→a` deliberately, matching Home
   Assistant's own `slugify` and saying so at `discovery.go:446-454`; it also
   folds `-` to `_` and then `collapseTokens` (`discovery.go:481`) drops a
   token that repeats the one before it. `topic.Slug` does none of the three.
   See §3.3 and [F4](#f4).

There is **one** normaliser for entity ids (`slugify` + `collapseTokens`) and
**one** for topic segments (`sanitiseSegment`, `topics.go:175`), and they are
unrelated by design: `sanitiseSegment` preserves case, hyphens and umlauts and
only removes what MQTT forbids in a topic name. Daikin had three normalisers;
this bridge has two, with a clean split of responsibility. Nothing in this tree
slugifies a topic segment, which is why the UniFi WLAN UUID survives into the
topic with its hyphens intact.

### 3.2 Duplicate `unique_id`s — **zero**

`TestNoDuplicateEntityRegistryKeys`, across all five scenarios and all 315
configs: zero duplicates on `unique_id` alone, zero on `(platform, unique_id)`,
zero on `default_entity_id`. mtec had nine; homeconnect, daikin and unifi have
zero. The phase inherits no duplicate question, and go-hamqtt v0.32.0's
`(platform, unique_id)` rule costs this bridge nothing.

`collapseTokens` is what keeps `default_entity_id` unique *and* readable: a
switch named "Switch Garage" carrying the `state` key would otherwise seed
`switch.switch_garage_state`; it seeds `sensor.switch_garage_state` with the
stutter removed. It is also, quietly, a collision risk that did not fire: two
devices named "Garage" and "Garage Garage" would collapse to the same seed.
Zero in the measured fleet; not asserted to be impossible.

### 3.3 Slug agreement — measured over the real catalogue

`TestSlugAgreementOverTheRealCatalogue`, with go-hamqtt v0.32.0's `topic.Slug`
transcribed verbatim beside a transcription of this bridge's unexported
`slugify`.

**Entity keys: 2 of 35 diverge.** Thirty-three of the thirty-five object-id
segments this daemon publishes (`state`, `cpu_utilization`, `port_1_poe`,
`radio_5g_channel`, `wan_latency`, `presence`, …) are lower-case ASCII
snake_case, on which both functions are the identity. The two that diverge are
the SSID switches, whose object-id segment is `wlan_<uuid>` and whose UUID
carries hyphens (`w1111111-1111-4111-8111-111111111111`, per
`internal/unifi/integration/testdata/wlans.json`): `topic.Slug` preserves the
hyphen, `slugify` would fold it. Nothing slugifies that segment today, so this
is a constraint on step 4, not a divergence that exists on the wire.

**Device names: 9 of 13 probes diverge.**

| Name | `hass.slugify` | `topic.Slug` |
| --- | --- | --- |
| `Gateway` | `gateway` | `gateway` |
| `Switch Garage` | `switch_garage` | `switch_garage` |
| `Büro-Gateway` | `buro_gateway` | `buero-gateway` |
| `Switch Küche` | `switch_kuche` | `switch_kueche` |
| `Außengerät` | `aussengerat` | `aussengeraet` |
| `Groß-NAS` | `gross_nas` | `gross-nas` |
| `Märkus' Händy` | `markus_handy` | `maerkus_haendy` |
| `Süd-WLAN` | `sud_wlan` | `sued-wlan` |
| `Gäste` | `gaste` | `gaeste` |
| `EG-Wohnzimmer` | `eg_wohnzimmer` | `eg-wohnzimmer` |
| `` (empty) | `` | `x` |

Three independent divergence classes: the umlaut expansion (`ä→a` vs `ä→ae`),
the hyphen (folded vs preserved), and the empty string (`""` vs `"x"`).

The divergence reaches **`default_entity_id` only** — `unique_id` and
`device.identifiers` are MAC-derived and ASCII, and no topic segment is
slugified. That is materially cheaper than homeconnect's F10 (where the device
slug was in the node id and the device identifier) and the same shape as
daikin's F4. But Home Assistant never renames a registered entity, so swapping
the function strands the entity *ids* of every entity on a non-ASCII-named
device — and in this bridge's actual user base, "Büro", "Küche", "Gäste-WLAN"
are the normal case, not the exotic one. See [F4](#f4).

An additional wrinkle this bridge has and the others do not: `collapseTokens`
is applied *after* slugification, so `topic.Slug` alone does not reproduce
`default_entity_id` even for pure-ASCII names whose device name repeats a token
of the key — `Switch Garage` + `state` is `switch_garage_state` here and
`switch_garage_state` under `topic.Slug` only because `Slug` never joins the
two. Any `discovery.Context` has to reproduce the *composition*, not just the
normaliser.

### 3.4 What I could not determine

- **Whether Home Assistant leaves an MQTT `binary_sensor` latched or moves it
  to `unknown` when an inbound payload matches neither `payload_on` nor
  `payload_off`.** [F1](#f1)'s severity turns on it: latched means the
  `reachable` sensor reports ONLINE forever after the first ONLINE, `unknown`
  means it merely goes blank. Reading HA's `MqttBinarySensor` message handler,
  an unmatched payload is logged and the state is left untouched — but that was
  read, not run. ***Settled by:*** one `mosquitto_pub` of `OFFLINE` to a device
  state topic against a live HA 2026.9, watching the entity.
- **Whether go-hamqtt's default `Layout` renders a bundle `NodeID` of
  `unifi_<mac>` without a consumer-supplied `Context`.** `SupersededTopics`
  takes `NodeID` from `discovery.Bundle.NodeID`, so the five-segment default
  retracts this fleet correctly *iff* the bundle's node id is byte-equal to
  `device.identifiers[0]` — which is a modelling choice made at step 4, not a
  property of the default. Nothing in go-hamqtt v0.32.0's non-test code
  decides it. ***Settled by:*** the first rendered bundle at step 4, compared
  against the goldens.
- **Whether a Home Assistant device bundle can carry a `device_tracker`
  component at all**, and whether a bundle may span a device whose components
  come from two poll loops with different readiness (the client tracker and the
  client's IP sensor). No prior phase published a `device_tracker`.
  ***Settled by:*** one throwaway bundle against HA 2026.9 at step 3b.
- **Whether two UniFi consoles ever report the same site `internalReference`.**
  [F6](#f6)'s cross-console collision assumes they do; `default` is the shipped
  value on every console, which is strong evidence but not a guarantee, and
  nothing in this repository asserts it. ***Settled by:*** one `GET /v1/sites`
  against a second console.

---

## 4. The availability model

**Two levels, as a list, with `availability_mode: "all"`, on every entity.**

All 315 configs carry an `availability` **array** and `"availability_mode":
"all"`; none carries the singular `availability_topic`, `payload_available` or
`payload_not_available`. Asserted by `TestAvailabilityModelIsTwoLevel`.

| Level | Topic | Count (of 315) |
| --- | --- | ---: |
| bridge only | `unifi/bridge/status` | **128** |
| bridge + device | `unifi/bridge/status` **and the device's own state topic**, with `value_template: "{{ 'online' if value == 'ONLINE' else 'offline' }}"` | **187** |

The 128 bridge-only entities are the ones that *report* the offline condition —
`state`, `reachable`, `firmware`, `update_available` (`discovery.go:240`,
`:250`, `:280`, `:286`), the client tracker (`client.go:95-100`), the client block
switch, the guest authorize button, every site-health entity and every SSID
switch. Making them unavailable when the thing they describe is offline would
hide exactly the information the user needs; the reasoning is written out at
`discovery.go:386-396`.

The second level is **the object's own state topic with a `value_template`**,
not a dedicated availability topic. That is the distinguishing fact of this
bridge's availability plane and the one thing the library's model does not
express directly:

- go-hamqtt's zero `model.Availability` resolves to `{LevelBridge, LevelDevice}`
  with mode `all` — the same *shape*, which is why this is homeconnect's
  situation rather than mtec's and daikin's. Taking the default does not grey
  out the fleet the way it would on daikin.
- But `LevelDevice` names `Layout.Availability(DeviceSlot(dev, e))`, a
  dedicated `<root>/<uid>/availability` topic. This bridge publishes no such
  topic and has no separate reachability signal to put on one — the signal *is*
  the `state` value. Reproducing today's payloads therefore needs either a
  `Layout` whose device-availability slot returns the device state topic **and**
  a per-level `value_template`, or a new `unifi/<site>/device/<mac>/availability`
  topic (which is additive on the wire, and a payload change on 187 configs).
- The 128/187 split is per entity, not per device, so it cannot be expressed by
  one `Availability` value on the bundle. `model.BridgeOnly()` for the 128 and
  the two-level value for the 187 is the shape, applied per component.

Verified, too: every second-level topic is one the daemon actually writes
(`TestAvailabilityModelIsTwoLevel` checks it against the recorded surface), and
the bridge topic is published by `OnConnect` (`coordinator.go:270`), by
`AnnounceOffline` on a clean stop (`:283`) and as the CONNECT will
(`cmd/unifi2mqtt/main.go:243`, retained). **There is no availability defect
here.** That is the second time in the programme, after daikin — and this one
is richer than daikin's, because the device level is real rather than absent.

See [F8](#f8) for the modelling consequence.

---

## 5. The pins

Two test files, five goldens, one `.gitattributes`.

### 5.1 `internal/coordinator/surface_pin_test.go` + `testdata/surface/`

Five scenarios × one golden each, ~13 000 lines of testdata:

```
testdata/surface/{minimal,full}.{en,de}.json
testdata/surface/nonascii.de.json
```

Each golden is one document:

```go
type recordedMsg struct {
	Topic  string         `json:"topic"`
	QoS    int            `json:"qos"`
	Retain bool           `json:"retain"`
	JSON   map[string]any `json:"json,omitempty"`   // decoded discovery config / attributes
	Text   *string        `json:"text,omitempty"`   // scalar state payload
	Empty  bool           `json:"empty,omitempty"`  // a retained-config clear
}
type surfaceDoc struct {
	Scenario string        `json:"scenario"`
	Messages []recordedMsg `json:"messages"`
}
```

**Topic, payload, QoS and retain live in the same row**, so a topic move, a
payload change and a delivery-guarantee change are all one diff. The payload is
stored **decoded**, so a reviewer reads JSON rather than an escaped blob.
Comparison is on **canonical re-encoding**: both sides go back through
`encoding/json`, which sorts object keys, so formatting, key order and line
endings cannot decide a run.

The recorder is the package's existing `fakeBroker`, which implements the real
`Publish` signature. The messages come from `Coordinator.OnConnect`,
`refreshStatic`, `refreshDevices`, `refreshDeviceStats`, `refreshClients` and
`refreshHealth`, gated exactly as `Run` gates them. **Nothing is stubbed except
the console (`Source`) and the broker (`Publisher`)** — the topic layout, the
discovery payloads, the change detection and the QoS/retain decisions are all
the production code.

One flag regenerates all five:

```sh
go test ./internal/coordinator -run TestPublishedSurfaceGolden -update-surface-golden
```

and **fails**, printing the new digests, because of the next paragraph.

### 5.2 The digests, held outside the goldens

```go
var goldenDigests = map[string]string{
	"minimal.en":  "69dd1e5f…",
	…
}
```

A golden produced by the code it guards is invisible to the run that produces
it. The only thing that makes a regeneration visible is a value held outside
the file. `-update-surface-golden` deliberately never writes this map: it
prints the five new literals and fails, so a human has to paste them in, in the
same commit, whose message must name the finding that moved the bytes. The
digest is taken over the **canonical encoding of the rebuilt scenario**, not
over the file's bytes, so it is immune to checkout differences as well as to
reformatting. Verified by mutation M10 below: with a mutated builder and
regenerated goldens, `TestPublishedSurfaceGolden` passes and
`TestPublishedSurfaceDigests` fails, naming every affected scenario.

### 5.3 `internal/coordinator/surface_invariants_test.go`

Twelve tests that rebuild the surface from the builders and **never read
testdata**, so every one of them still fails immediately after a regeneration:

| Test | Pins |
| --- | --- |
| `TestConfigTopicForm` | 315 of 315 config topics are five segments; the node id is the device identifier on 315; `unique_id != <node>_<object>` on exactly 21 ([F5](#f5)) |
| `TestConfigFilterMatchesEveryConfigTopic` | the reconcile filter matches all 315, and does **not** match a device bundle |
| `TestNoDuplicateEntityRegistryKeys` | zero duplicates on `unique_id`, `(platform, unique_id)` and `default_entity_id` |
| `TestDeviceBlocksArePinned` | the eight `device.identifiers` as Go literals |
| `TestIdentityIsLanguageIndependent` | `unique_id`, `default_entity_id`, every topic and `device.identifiers` compared en against de directly |
| `TestAvailabilityModelIsTwoLevel` | the list form, mode `all`, the 128/187 split, and that every named topic is published ([F8](#f8)) |
| `TestPublishQoSAndRetain` | 315 configs at QoS 1, 5 availability at QoS 1, 305 state publishes at QoS 0, all retained ([F9](#f9)) |
| `TestAdvertisedStateTopicsArePublished` | **builder against builder**: every advertised state/attributes topic is one the publish path writes ([F1](#f1), [F10](#f10)) |
| `TestKnownUnpublishedTopicsAreStillAdvertised` | the allowlist's other half — an entry that stops being advertised must be deleted, not left to mask a future one |
| `TestCommandTopicsAreSubscribed` | **builder against builder**: all 45 advertised command topics match one of the six subscribed filters ([F10](#f10)) |
| `TestSlugAgreementOverTheRealCatalogue` | 2 of 35 entity keys, 9 of 13 device-name probes ([F4](#f4)) |
| `TestTwoDefaultInstancesCollideOnEveryString` | two instances share every config topic and every `unique_id`, and changing `MQTT_TOPIC` moves only the availability topic ([F6](#f6)) |
| `TestSurfaceCensus` | the entity, message and platform counts of §2.2, as Go literals |

`knownAdvertisedButUnpublished` is the one allowlist, four entries, each named
to [F1](#f1) so a fix deletes them and the second test fails until it does.

### 5.4 `.gitattributes`

The repository had none, ships JSON fixtures under
`internal/unifi/integration/testdata/` and now `internal/coordinator/testdata/`,
and its CI test matrix includes `windows-latest`
(`.github/workflows/ci.yml:59`). Git rewrites text files to CRLF on a Windows
checkout, which changes their bytes; homeconnect's digests broke exactly this
way. The comparison here is on canonical re-encoding and the digest is over the
rebuilt document rather than the file, so neither would actually break — but
the repository should not depend on that. Added `* text=auto eol=lf` plus
binary markers.

### 5.5 Mutation proof

Every mutation was applied to a **committed** tree, the test run observed, and
the tree restored with `git checkout --`. M6–M9 were run **after regenerating
the goldens**, which is the only honest test of a builder pin.

| # | Mutation | Caught by |
| ---: | --- | --- |
| M1 | `configTopic` swaps the platform and node-id segments | `TestConfigTopicForm` + the digests (all 5) + two pre-existing tests |
| M2 | `idPrefix` `unifi` → `ubnt` | `TestDeviceBlocksArePinned` + the digests (all 5) + four pre-existing tests |
| M3 | `slugify` transliterates `ü→ue` | golden + digest, **`nonascii.de` alone** |
| M4 | discovery publish `retain` true → false | `TestPublishQoSAndRetain` + the digests (all 5) |
| M5 | state publish `mqtt.QoS0` → `mqtt.QoS1` | `TestPublishQoSAndRetain` + the digests (all 5) |
| **M6** | `deviceSpecs`' `stateSuffix: "uptime"` → `"uptime_s"` — **the config side only, goldens regenerated** | **`TestAdvertisedStateTopicsArePublished`** |
| **M7** | `keyUptime` → `"uptime_s"` — **the publish side only, goldens regenerated** | **`TestAdvertisedStateTopicsArePublished`** |
| **M8** | `DeviceControls`' `"cmd/restart"` → `"cmd/reboot"` — **the config side only, goldens regenerated** | **`TestCommandTopicsAreSubscribed`** |
| M9 | `availabilityFor` drops the device level, goldens regenerated | `TestAvailabilityModelIsTwoLevel` + `TestAdvertisedStateTopicsArePublished` |
| M10 | `Manufacturer` `Ubiquiti` → `Ubiquity`, **goldens regenerated** | the digests alone, naming all five scenarios; the golden test passes |

M6, M7 and M8 are the point of the exercise. Each changes **one** of the two
independent vocabularies that have to agree, which is invisible to a golden
regeneration and is precisely the failure mode [F10](#f10) describes. None is
caught by the golden after regeneration; all three are caught by the
builder-against-builder tests.

---

## 6. `discovery.Validate` — what the library says about today's payloads

All 315 payloads were run through go-hamqtt v0.32.0's
`discovery.ValidateBody(platform, body)` out of tree, so the dependency is not
taken a step early.

```
payloads=315  blocking=18  advisory-only=0  warnings=0
```

**18 of 315 are refused**, and they are exactly the 18 `button` configs (six per
`full`/`nonascii` scenario × three scenarios), each for two reasons:

```
button: "state_topic" is not a valid key for platform "button"
        (Home Assistant would drop it silently)
button: "optimistic"  is not a valid key for platform "button"
```

Both come from struct shape rather than intent: `entity.StateTopic` is
`json:"state_topic"` **without** `omitempty` (`discovery.go:160`), so every
payload emits it — as `""` for a button — and `controlEntity.Optimistic` is
likewise `json:"optimistic"` without `omitempty` (`control.go:46`), so every
control emits it, including the two button kinds, where the MQTT button schema
does not declare it.

Harmless today: HA's MQTT discovery schemas are `extra=REMOVE_EXTRA`, so both
keys are dropped on arrival and the button works. **Fatal at step 6**: a
bundle that fails validation publishes nothing, and every device bundle
carrying a restart button — that is *every infrastructure device* once
`CONTROLS.ENABLE` is on, plus any guest client — would lose its entire entity
set, not just the button. See [F2](#f2). This is homeconnect's F-shape (11 of
687 for a `device_class`) with a wider blast radius, because here the offending
key is on a platform every device has.

Everything else validates clean: `device_tracker`, `switch`, `sensor` and
`binary_sensor` produce zero issues and zero warnings, `default_entity_id` is
accepted on all five platforms, and the two-level `availability` list with
`availability_mode` passes.

---

## Findings

Ranked by severity. **None was fixed in this PR.** Every one is reproducible
from the pins.

<a name="f1"></a>
### F1 — the `reachable` binary sensor can turn on but never off · **high**

`internal/hass/discovery.go:243-251`:

```go
{
	platform: PlatformBinarySensor, key: "reachable", stateSuffix: "state",
	deviceClass: "connectivity", payloadOn: "ONLINE",
	// Anything that is not exactly ONLINE counts as unreachable,
	// which is what an automation wants: ADOPTING and UPDATING
	// are not states you can route traffic through.
	payloadOff:      "",
	bridgeAvailOnly: true,
},
```

The intent is stated in the comment. The implementation does not achieve it:
`entity.PayloadOff` is `json:"payload_off,omitempty"` (`discovery.go:166`), so
an empty string is **omitted from the payload entirely**, and Home Assistant
falls back to its schema default of `"OFF"`. Measured in the golden:

```json
"payload_on": "ONLINE",        <- and no payload_off at all
"state_topic": "unifi/default/device/00005e005301/state"
```

The values that arrive on that topic are `model.DeviceState` strings
(`model.go:56-66`): `ONLINE`, `OFFLINE`, `PENDING_ADOPTION`, `UPDATING`,
`ADOPTING`, `DELETING`, `CONNECTION_INTERRUPTED`, `ISOLATED`,
`U5G_INCORRECT_TOPOLOGY`, `UNKNOWN`. **None of them is `OFF`.** So the sensor
matches `payload_on` on `ONLINE` and matches nothing on every other value.

Reading HA's MQTT binary-sensor message handler, an unmatched payload is logged
and the entity's state is left untouched — the sensor latches `on` after the
first `ONLINE` and never reports a device going away. That is the opposite of
what a connectivity sensor is for, and it is silent: the entity stays
*available* (it is `bridgeAvailOnly`), it just lies. §3.4's first unknown is
whether HA latches or blanks; either way the sensor cannot report `off`.

The same shape does **not** affect the other binary sensors: `port_link`
(`UP`/`DOWN`), `port_poe` and `update_available` (`ON`/`OFF`) all set both
payloads explicitly, and `wan_connectivity` sets `payload_on: "ok"` with no
`payload_off`, so it has the identical defect on a smaller surface
(`health.go:18-23`).

Fix: publish `payload_off: "OFF"` and map every non-`ONLINE` state to `OFF` on
the wire, or give the entity a `value_template` the way the availability
sources already use. Both change a retained payload on every device, so this
belongs in the defect step with a release note.

<a name="f2"></a>
### F2 — every button payload is refused by `discovery.Validate` · **high (gates step 6)**

§6. 18 of 315, all `button`, for `state_topic` and `optimistic` — two keys the
MQTT button schema does not declare, emitted because neither struct field has
`omitempty` (`discovery.go:160`, `control.go:46`).

Today Home Assistant drops both silently and the button works. At step 6 a
blocking validation result means the bundle is not published at all, so the
device loses **every** entity, not the button. With `CONTROLS.ENABLE` on that
is every infrastructure device plus every guest client.

Fix: `omitempty` on both fields, or a button-specific payload struct. Either
way it changes the retained bytes of 18 configs and therefore belongs in the
defect step, before the byte-equality proof.

<a name="f3"></a>
### F3 — six published state topics that no entity reads, including PoE wattage · **medium**

Measured per scenario, as topics the daemon publishes that no config names:

| Topic | Published by | Read by |
| --- | --- | --- |
| `unifi/<site>/device/<mac>/port/<n>/poe/power_w` | `device.go:159` | **nobody** |
| `unifi/<site>/health/lan/state` | `coordinator/health.go:72` | **nobody** |
| `unifi/<site>/health/wlan/state` | `coordinator/health.go:73` | **nobody** |
| `unifi/<site>/health/vpn/state` | `coordinator/health.go:74` | **nobody** |
| `unifi/<site>/wlan/<id>/name` | `device.go:178` | **nobody** |
| `unifi/<site>/wlan/<id>/enabled` | `device.go:175` | only the SSID switch, i.e. nobody unless `CONTROLS.WLAN_ENABLE` |

`unifi/bridge/info` is a seventh, and is diagnostic by design.

The PoE wattage one is the interesting one. `keyPortPoEPower = "poe/power_w"`
(`topics.go:67`) is published whenever the classic layer reports a port drawing
power — one of the classic layer's advertised reasons to exist
(`CLAUDE.md`, "PoE wattage") — and `portSpecs` (`discovery.go:291`) announces
`link`, `speed` and `poe` and **no power sensor**. So the value reaches the
broker and never reaches Home Assistant. Likewise `lan`, `wlan` and `vpn`
subsystem health: published, in `healthAttrs`, and with no entity, while `wan`
gets one.

None of this is broken — an MQTT consumer other than Home Assistant sees them —
but each is either a missing entity or a topic that should not be written.
Retiring a topic an installed base may already consume is a decision about the
operator contract rather than a defect fix; mtec and homeconnect both declined
to make it in the migration, and the same call applies. Adding the four missing
entities is additive and belongs after step 6, not in it.

<a name="f4"></a>
### F4 — the entity-id slug is not reproducible by `topic.Slug` · **high (gates the phase)**

§3.3. Two of 35 entity keys diverge (the SSID UUID's hyphens) and nine of
thirteen device-name probes do, on three independent classes: `ä→a` vs `ä→ae`,
the hyphen, and the empty string.

The divergence reaches `default_entity_id` only — `unique_id` and
`device.identifiers` are MAC-derived, and no topic segment is slugified — so a
swap strands entity *ids*, not registry keys. It is still a one-time
re-registration for every German device and client name, which in this bridge's
user base is most of them, and Home Assistant never renames a registered
entity.

There is a second half the other phases did not have: `collapseTokens`
(`discovery.go:481`) runs *after* slugification and drops a token that repeats
the one before it, so `default_entity_id` is a function of the **composition**
`collapseTokens(slugify(name) + "_" + slugify(key))`, not of a normaliser
applied to one string. A `discovery.Context` that supplies only an `ObjectID`
slug function does not reproduce it.

This is not a defect — `ä→a` is what Home Assistant's own `slugify` does, and
`discovery.go:446-454` says so — but it is the constraint the phase has to be
designed around. **The decision belongs before step 3, not inside it**; phase 6
hit its equivalent at step 3 and paid for it.

<a name="f5"></a>
### F5 — the five-segment default is the right *form* only if the bundle's node id is chosen to match · **high (gates step 6)**

§2.3, measured over all 315 config topics. Unlike zendure, mtec and daikin,
this bridge is already on `publisher.SupersededTopics`' default form, so the
usual trap — a default that retracts nothing — does not apply.

What does apply is that `LegacyTopicWithNodeID` keys on
`discovery.Bundle.NodeID` and the component key, and both have to be chosen to
be byte-equal to what this bridge published. Measured:

- the node id must be `device.identifiers[0]` — `unifi_<mac>`,
  `unifi_client_<key>` or `unifi_site_<site>` — which holds on 315 of 315
  today, so the bundle must be keyed on the identifier and not on a slug of the
  device name;
- the component key must be the **object-id segment**, which on 21 of 315
  configs is *not* derivable from the `unique_id` (§2.3's table). A step-4
  modelling that derives the component key from the `unique_id` suffix would
  retract nothing for those 21;
- `LegacyTopicByUniqueID` would retract nothing at all here and must **not** be
  stated — and `Config.LegacyEntityTopics` **replaces** the default rather than
  extending it, so stating it would silently stop retracting the form this
  fleet is actually on.

The consequence of getting it wrong is silent and total: the bundle is
published while the per-entity configs are still retained, Home Assistant
refuses it with one `WARNING [mqtt.entity] Received a conflicting MQTT
discovery message`, and the result is no entities and no error on the wire.

Second consequence, same fact: `Discovery.ConfigFilter()` is
`homeassistant/+/+/+/config` and `IsOwnConfig` requires a top-level
`unique_id` starting `unifi_` **and** a matching entry in the payload's
`availability` array (`cleanup.go:93-110`). A four-segment device bundle has
neither the topic shape nor an `availability` array at the top level, so a
bundle published by this daemon is invisible to its own orphan cleanup in both
directions — and the reconcile is on by default (`DefaultHASSCleanup = true`).

<a name="f6"></a>
### F6 — two default instances kick each other off the broker; two non-default ones fight over the site device · **high**

`internal/config/config.go:262-264`:

```go
// ClientID is the MQTT client identifier, derived from the topic root
// so two instances bridging different sites do not collide.
func (c *Config) ClientID() string { return ClientIDPrefix + c.MQTTTopic }
```

The comment is wrong about *sites*: the id is derived from `MQTT_TOPIC`, which
defaults to `unifi` and has nothing to do with the site. Two daemons with the
shipped config therefore both present `unifi2mqtt-unifi`, and MQTT requires the
broker to disconnect the existing session when a second client presents the
same identifier (3.1.1 §3.1.3.2 / 5.0 §3.1.4). They kick each other in a loop,
each one's `Lifecycle` reconnecting into the other's session; neither publishes
reliably, both LWTs fire repeatedly, and nothing in either log says why.

This is *better* than daikin's F1 — there is a config key out of it, and
changing `MQTT_TOPIC` is the documented way to run two bridges — but the escape
does not separate discovery:

- `unique_id`, `device.identifiers` and the config topic contain **no**
  instance-scoped string. `TestTwoDefaultInstancesCollideOnEveryString`
  measures it: changing `MQTT_TOPIC` moves the availability topic and **only**
  the availability topic; all 75 config topics and all 75 `unique_id`s are
  unchanged.
- Two instances bridging **the same console** therefore write the same 75
  retained configs with different `availability[0].topic` values, flip-flopping
  on every forced republish. `IsOwnConfig` correctly declines the other's
  payload (the availability topic is the second ownership signal,
  `cleanup.go:101`), so neither *sweeps* the other — they merely overwrite.
- Two instances bridging **different consoles** are clean on devices and
  clients (MACs differ) but collide on the **site device**:
  `siteDeviceID(d.site)` uses `Site.Internal`, which is `default` on every
  console out of the box (`internal/unifi/integration/testdata/sites.json`).
  So `unifi_site_default`, its seven health entities and every SSID switch are
  shared between two unrelated consoles, with the SSID switches keyed on UUIDs
  that *are* distinct — meaning one site device accumulates both consoles'
  SSIDs and one console's health values overwrite the other's. §3.4's fourth
  unknown is whether `internalReference` is really always `default`.

**At step 6 the stakes change.** A device bundle is *one* retained topic per
device carrying that device's entire component set, so two instances with any
divergence — a different `LANGUAGE`, a different control set, one with the
classic layer and one without — replace each other's whole entity set on every
publish rather than overwriting entity by entity. mtec's reviewer proved a
staggered two-instance upgrade deletes the sibling's entire fleet. For the site
device that is not hypothetical: it is what two consoles on one broker would do
to each other on every poll.

Fix candidates, none decided here: a `MQTT_CLIENT_ID` key defaulting to today's
formula; putting the site *UUID* rather than `internalReference` in
`siteDeviceID` (which re-keys every existing site entity); an `INSTANCE_ID`
empty by default (additive, and empty is today's behaviour).

<a name="f7"></a>
### F7 — no `origin` block · **low**

`grep -rn '"origin"' --include='*.go' internal` → no hits. Shared with all six
bridges (ADR 0070 §2.2, *"Not one of the six sets the `origin` block"*).
go-hamqtt makes it mandatory (`origin.name` missing is a blocking validation
issue), so the migration supplies it for free at step 4 — and it is a payload
addition on all 315 configs, which means it must not land in the same step as
anything else.

<a name="f8"></a>
### F8 — the library's `LevelDevice` names a topic this bridge does not publish · **medium**

§4. The availability plane is correct and complete as it stands: two levels,
list form, mode `all`, a 128/187 split between bridge-only and bridge+device,
every named topic actually written.

The *shape* matches go-hamqtt's zero `model.Availability`
(`{LevelBridge, LevelDevice}`, mode `all`), which is why this is homeconnect's
situation rather than daikin's — the default does not grey out the fleet. What
does not match is the device level's **topic and template**: the library's
`LevelDevice` names `<root>/<uid>/availability`, this bridge names the object's
own `state` topic and maps it with
`{{ 'online' if value == 'ONLINE' else 'offline' }}`.

Three things follow:

- a `Layout` whose device-availability slot returns the state topic, plus a
  per-level `value_template`, is needed to stay byte-equal;
- the 128/187 split is per entity, so one `Availability` on the bundle cannot
  express it — `model.BridgeOnly()` has to be applied per component;
- publishing a real `…/availability` topic instead would be an *improvement*
  (the daemon knows the state already), and is exactly the kind of thing that
  must not happen in the same step as the migration.

<a name="f9"></a>
### F9 — QoS 0 becomes QoS 1 on migration unless spelled out, and this bridge's split hides it · **medium**

§2.6. Discovery and availability are already QoS 1; the 71 state publishes per
`full` cycle — 305 across the five pinned scenarios — are QoS 0. `publisher.QoS`'s zero value is `QoSUnset` and resolves to
**QoS 1**, so a `publisher.Config{}` left unset moves the state plane and
leaves the two planes that were already QoS 1 untouched.

That is the trap: on the four predecessor bridges the whole surface was QoS 0,
so an unset config was a visible, uniform change. Here half the surface is
*supposed* to be QoS 1, and a reviewer checking "the bridge publishes discovery
at QoS 1, good" finds that half correct and stops.

`publisher.QoSAtMostOnce` (`0x80`) must be written explicitly in
`StateConfig.QoS` while `Config.QoS`, `AvailabilityConfig.QoS` and
`CommandConfig.QoS` stay at 1. `resolveQoS` panics at construction on an
unrecognised value, so a mistake is loud — but omission is silent, which is the
case here. Pinned by `TestPublishQoSAndRetain`, which reads the value off the
transport call rather than off a constant.

<a name="f10"></a>
### F10 — the topic suffix vocabulary exists twice, and nothing compared the two · **medium**

This repository already does the thing mtec and homeconnect had to be taught:
`internal/hass` receives the topic layout through the `Topics` interface
(`discovery.go:26-48`) rather than rebuilding it, and `CLAUDE.md` states the
rule in bold. The **path prefix** is genuinely composed once.

The **suffixes are not.** They exist as two independent vocabularies:

| Family | `internal/coordinator` | `internal/hass` |
| --- | --- | --- |
| device values | `keyState`, `keyUptime`, `keyCPUUtilization`, … (`topics.go:54-62`) | `stateSuffix: "state"`, `"uptime"`, `"cpu_utilization"`, … string literals (`discovery.go:236-288`) |
| ports | `keyPortState/Speed/PoE` + `topicBuilder.port` (`topics.go:64-67`, `:124`) | `"port/" + idx + "/state"` etc. (`discovery.go:298-317`) |
| radios | `keyRadioChannel/TxRetries` + `topicBuilder.radio` (`topics.go:69-70`, `:133`) | `"radio/" + band + "/channel"` etc. (`discovery.go:333-339`) |
| health | `keyWANState`, `keyWANLatency`, … (`coordinator/health.go:25-34`) | `"wan/state"`, `"wan/latency_ms"`, … (`hass/health.go:17-52`) |
| clients | `keyState`, `keyClientIP`, `keyClientSignal`, `keyAttributes` (`topics.go:75-77`) | `"state"`, `"ip"`, `"signal"`, `"attributes"` (`hass/client.go:51-72`, `:96`, `:129`) |
| WLAN | `keyWLANEnabled`, `keyWLANName` | `"enabled"`, `"enabled/set"` (`control.go:192-200`) |
| commands | `cmdRestart`, `cmdLocateSet`, `cmdPowerCycle`, `cmdBlockedSet`, `cmdAuthorize`, `cmdWLANEnabled` (`command.go:42-47`) | `"cmd/restart"`, `"cmd/locate/set"`, `"port/"+idx+"/cmd/power_cycle"`, `"blocked/set"`, `"cmd/authorize"`, `"enabled/set"` (`control.go:59-200`) |

Counting composition sites rather than families: **16 places in this tree
compose an MQTT topic string**, of which **nine are inside `internal/hass`** —
four config-topic composers (§2.1), the config *filter* (`cleanup.go:76`, a
fifth independent copy of the same shape), and four suffix-vocabulary tables.
On the coordinator side, `topicBuilder.devicePrefix` (`topics.go:117`)
re-composes `root/site/device/<mac>/` rather than delegating to
`topicBuilder.device`, and `subscribeCommands` (`command.go:107-116`) and
`parseCommand` (`command.go:157`) each re-compose the `root/site/` prefix
a third and fourth time.

The phase plan expected two everywhere. mtec measured two and found five;
homeconnect six; daikin nine. **Sixteen is the largest count in the
programme** — though the comparison is not like for like, because unlike
daikin's nine *state-topic* builders these are mostly one composer per family
plus a second copy of the suffix names.

They agree today — measured across all five scenarios, every advertised topic
against every published one, and every advertised command topic against the six
subscribed filters. Nothing enforced it. Now pinned builder-against-builder by
`TestAdvertisedStateTopicsArePublished` and `TestCommandTopicsAreSubscribed`,
proven by mutations M6, M7 and M8 — each of which moves one side alone,
survives a golden regeneration, and is caught.

The one place they already disagree is [F11](#f11).

<a name="f11"></a>
### F11 — the Locate switch's state topic is written by nobody · **medium**

`internal/hass/control.go:66-73` announces the switch with

```go
d.topics.DeviceTopic(dev.MAC, "locate"),        // state topic
d.topics.DeviceTopic(dev.MAC, "cmd/locate/set") // command topic
```

`grep -rn '"locate"' --include='*.go' internal | grep -v hass | grep -v _test`
returns nothing: `unifi/<site>/device/<mac>/locate` is advertised on every
device and written on none. The switch therefore sits at `unknown` forever, and
a press is executed (the command path works, `command.go:178`) but the entity
never reflects it — with "Never publish optimistic state" (`CLAUDE.md`) meaning
the only thing that could move it is a poll that does not read the locate flag
back.

ADR 0070 §2.2 already recorded this alongside the `default_entity_id` domain
mismatch — *"unifi's Locate control declares `DefaultEntityID: "button." + seed`
but publishes under `switch`, and its declared state topic is never
published"*. The first half was fixed on `origin/main`
(`control.go:267-271`, with the reasoning written out). **The second half is
still open**, now measured: four devices × one dead topic in the `full`
scenarios.

It is the one live instance of [F10](#f10)'s drift, and it is in
`knownAdvertisedButUnpublished` with this finding named, so a fix has to delete
the entry and `TestKnownUnpublishedTopicsAreStillAdvertised` fails until it
does.

Fix: either publish the locate flag (the classic API reports it) or drop the
`state_topic` and make the switch `optimistic: true` — the second contradicts
this repository's own rule and the first is the right one. Either moves a
retained payload on every device, so it belongs in the defect step.

<a name="f12"></a>
### F12 — `collapseTokens` can collide two device names into one entity-id seed · **low**

`entityIDSeed` (`discovery.go:438`) joins the slugified device name to the
slugified key and then drops a token that repeats its predecessor
(`discovery.go:481`). Two devices named `Garage` and `Garage Garage` produce
the same seed for every key; so do `AP Küche` and `AP Kuche`, because
`slugify` folds the umlaut.

Zero collisions in the measured fleet, and a collision costs only the *second*
entity's preferred id — Home Assistant appends a discriminator rather than
merging, because the `unique_id`s differ. Recorded because
`TestNoDuplicateEntityRegistryKeys` asserts zero today and the assertion is
about the fleet, not about the function.

<a name="f13"></a>
### F13 — `ClientID`'s doc comment describes a behaviour it does not have · **low**

`config.go:262`: *"derived from the topic root so two instances bridging
different sites do not collide"*. It is derived from `MQTT_TOPIC`; the site
does not enter it. Two instances bridging two sites of the *same* console with
the shipped config collide completely — see [F6](#f6). One line of doc, but it
is the line an operator would read before deciding not to set anything.

---

## Step 2 outcome — what was fixed, what was left, and what step 4 must not reopen

Added by the steps-1-and-2 PR. The findings above are the measurement
and are left as they were written; this section records what happened to
each, including the two places the measurement was wrong.

### Corrections to the measurement

- **F2 is a defect in this repository, not in go-hamqtt.** The finding
  attributes the two refused keys to "struct shape", and a reader can
  take that as naming a *shared* struct. There is no shared struct:
  go-unifi2mqtt does not depend on go-hamqtt at all, and `entity` and
  `controlEntity` are this repository's own types in `internal/hass`.
  Fixed here. No library version was bumped for it.
- **F1 has a third instance the measurement did not name.** It lists the
  device `reachable` sensor and `wan_connectivity`. `port_link` has it
  too: `payload_on: "UP"` and `payload_off: "DOWN"` against a
  `model.PortState` that is `UP`, `DOWN` **or** `UNKNOWN`. Found by the
  regression test, which is why that test asserts over each sensor's
  real state vocabulary rather than checking that the two keys are
  present.
- **F10's count is 22, not 16.** Recounted below.

### Disposition

| Finding | Disposition |
| --- | --- |
| **F1** high | **Fixed.** `value_template` mapping the whole state vocabulary onto `payload_on`/`payload_off`, on `reachable`, `wan_connectivity` and `port_link`. |
| **F2** high | **Fixed.** `omitempty` on `state_topic`, `Optimistic` becomes a `*bool` nothing sets, and the one explicit `StateTopic: ""` is gone. |
| **F3** medium | **Left**, as go-mtec2mqtt and go-homeconnect2mqtt left theirs. Retiring a topic an installed base may already consume is an operator-contract decision, and adding the four missing entities is additive work that belongs after step 6. See below for why PoE wattage did not change that. |
| **F4** high | **Decided: keep this bridge's own normalisers.** Recorded below so step 4 does not reopen it. |
| **F5** high | **Recorded** as `hass.LegacyConfigTopicForm`, with a two-candidate test. The four config-topic composers are now one. |
| **F6** high | **Half fixed:** `MQTT_CLIENT_ID`, defaulting to today's derived id. The cross-console `unifi_site_default` collision is **left** — re-keying it re-registers every existing site entity, which is a step-3 decision. |
| **F7** low | **Left.** A payload addition on every config; step 7, and not in the same step as anything else. |
| **F8** medium | **Left**, and narrowed — see below. |
| **F9** medium | **Already pinned** by `TestPublishQoSAndRetain`, which reads the value off the transport call. Nothing to fix; confirmed below. |
| **F10** medium | **Recounted and partly converged.** Four config-topic composers became one; the suffix vocabularies stay two, pinned builder-against-builder. |
| **F11** medium | **Fixed.** The locate LED is read back from `/stat/device` and published. Which exposed a second instance of the same drift in the nudge routing. |
| **F12** low | **Left.** A collision costs only the second entity's preferred id; `unique_id`s differ, so Home Assistant appends a discriminator rather than merging. |
| **F13** low | **Fixed** with F6; it is the same defect in prose. |

### F4 — decided: keep `slugify` and `collapseTokens`

**This bridge keeps its own normalisers.** Step 4 must reproduce them,
not replace them.

Three bridges have faced this question and all three kept theirs:
go-mtec2mqtt (8 of 100 keys), go-homeconnect2mqtt (5 of 7 device
probes) and go-daikin2mqtt (three separate normalisers). Here 2 of 35
entity keys diverge and 9 of 13 device-name probes, on three
independent classes: `ä→a` vs `ä→ae`, the hyphen, and the empty string.

The reasoning, in the order it matters:

1. **Home Assistant never renames a registered entity.** A `unique_id`
   it already knows keeps the `entity_id` it was first given, so
   swapping the function does not migrate anything — it strands the old
   ids and creates a second set for anything registered afterwards.
2. **`ä→a` is what Home Assistant's own `slugify` does**, and
   `discovery.go:446-454` says so. This is not a defect to be corrected
   into agreement with the library; it is agreement with the platform.
3. **The user base is German.** "Büro", "Küche", "Gäste" are the normal
   case here, not the exotic one, so the divergence is not a corner.
4. The blast radius is smaller than homeconnect's — `unique_id` and
   `device.identifiers` are MAC-derived and no topic segment is
   slugified, so only `default_entity_id` moves — and that is an
   argument for the change being *cheap*, not for it being *free*. It
   still re-registers every entity on a non-ASCII-named device.

**And the seed is a composition, not a function.** `default_entity_id`
is `collapseTokens(slugify(device.Name) + "_" + slugify(key))`
(`discovery.go:438`, `:481`), with `collapseTokens` dropping a token
that repeats the one before it. No single slug function reproduces
that, for ASCII names either: a `discovery.Context` that supplies only
an object-id slug function does not produce this bridge's seeds. **Step
4 has to reproduce the composition.**

### F9 — confirmed, and nothing to fix

The QoS split is deliberate and already pinned by
`TestPublishQoSAndRetain`, which reads `qos` off the recorded transport
call rather than off a constant — the property that let go-mtec2mqtt's
pin survive its entire publish plane moving. Measured on this branch:

| Plane | QoS | Count across the five scenarios |
| --- | ---: | ---: |
| discovery configs | 1 | 315 |
| bridge availability | 1 | 5 |
| everything else | 0 | 317 |

(317, not 305: the twelve new locate topics of F11.) Everything is
retained.

What step 5 must write: `publisher.QoSAtMostOnce` (`0x80`) explicitly in
`StateConfig.QoS`, with `Config.QoS`, `AvailabilityConfig.QoS` and
`CommandConfig.QoS` left at 1. `publisher.QoS`'s zero value is
`QoSUnset` and resolves to **1**, so an unset config moves the state
plane alone — and a reviewer checking "discovery is at QoS 1, good"
finds that half correct and stops.

### F10 — recounted: 22 composition sites, not 16

Counted as places that compose an MQTT topic or filter string from
parts, at `origin/main`:

| Where | Sites |
| --- | ---: |
| `coordinator/topics.go` — `bridge`, `device`, `devicePrefix`, `port`, `radio`, `wlan`, `health`, `client` | 8 |
| `coordinator/command.go` — the six subscribe filters' shared prefix, `parseCommand`'s prefix strip, `parseDeviceCommand`'s port-suffix reassembly | 3 |
| `hass` config-topic composers — `discovery.configTopic`, `client.clientConfigTopic`, the inline one in `health.go`, the inline one in `control.go` | 4 |
| `hass/cleanup.go` — `ConfigFilter`, a fifth independent copy of the config-topic shape | 1 |
| `hass` suffix vocabularies — `deviceSpecs`, `portSpecs`, `radioSpecs`, `healthSpecs`, `client.go`'s, `control.go`'s command suffixes | 6 |
| **Total** | **22** |

The phase plan expected two everywhere. go-mtec2mqtt measured two and
found five, go-homeconnect2mqtt six, go-daikin2mqtt nine, and this
document sixteen. **Every bridge has undercounted, including this one.**

Four became one: the config-topic composers. That is byte-neutral and
proven so, which is exactly the evidence that they had agreed. **18
remain.** They are not converged here, because unlike go-daikin2mqtt's
twelve state-topic builders these are mostly one composer per family
plus a second copy of the suffix *names*, and the suffix names are
pinned builder-against-builder by `TestAdvertisedStateTopicsArePublished`
and `TestCommandTopicsAreSubscribed`. Converging them is a step-4
question, not a defect.

The measurement said F11 was the one live instance of the drift. It was
the one live instance **in the published topics**. There was a second,
in the *routing*: `scheduleRefresh` sent locate and WLAN commands to
`nudgeDevices`, and neither control's state is published by that loop —
both come from the static loop. The nudge woke a loop that republished
an unchanged snapshot, so the entity stayed on its old value until the
next hourly poll, which looks exactly like a working nudge. Now pinned
per command kind by
`TestEachCommandNudgesTheLoopThatPublishesItsState`.

### F3 — why PoE wattage is still a stray topic

The measurement offers the alternative reading: PoE wattage is better
understood as a missing entity than as a stray topic, and adding the
entity is a change with a real user benefit.

It is, and it is still not this step's change. Adding a
`port_<n>_power` sensor adds a config topic and an entity to every
PoE-delivering port of every switch — additive on the wire, and exactly
the kind of thing that must not share a step with a migration whose
whole claim is byte-equality. It also needs a decision this step cannot
make: whether a port that stops delivering power should have its sensor
go to 0 or stop being published, which is the same question
`publishPort` already answers one way for wattage and another for the
PoE flag. **Step 7**, with the other three of F3's missing entities
(`lan`, `wlan`, `vpn` subsystem health).

The six unread topics stay published. Retiring one an installed base may
already consume is an operator-contract decision; three prior phases
declined to make it inside the migration and so does this one.

### F8 — narrower than the measurement implies

Measured against go-hamqtt after this document was written: the per-entity
128/187 split **is** expressible in the shared model.
`model.Availability` sits on `model.Description` and is resolved per
entity by `Context.Availability`, so the split is `model.BridgeOnly()`
beside the zero value, applied per component. `value_template` on an
availability entry is supported.

The only gap is spelling. `StdContext` emits a dedicated availability
topic carrying `true`/`false` for `LevelSelf`; this bridge wants the
object's own state topic with a template over the bare value. The route
is overriding `Context.Availability`, an interface method that exists
for this. **Nothing needs adding to the model**, so F8's step-3
decision is narrower than §4 suggests: it is a `Context` override, not
a choice between a `Layout` hack and changing 187 payloads.

### Bytes moved

Every figure below comes from running the **builders** at `origin/main`
and at the branch tip and diffing their output key by key — not from
diffing the regenerated fixtures, which are written by the code they
guard. The harness is committed as
`internal/coordinator/surface_dump_test.go` so every later step can
repeat it.

| Scenario | Topics added | Keys changed | Findings |
| --- | ---: | ---: | --- |
| `minimal.en` | 0 | 18 | F1 |
| `minimal.de` | 0 | 18 | F1 |
| `full.en` | 4 | 42 | F1, F2, F11 |
| `full.de` | 4 | 42 | F1, F2, F11 |
| `nonascii.de` | 4 | 42 | F1, F2, F11 |

**12 topics added, 162 keys changed, 0 topics removed.** The complete
list of what moves:

- `payload_on`, `payload_off` and `value_template` on 33 binary-sensor
  configs — F1;
- `state_topic` removed from 18 button configs, `optimistic` removed
  from 45 control configs — F2;
- 12 new `unifi/<site>/device/<mac>/locate` state topics — F11.

**No `unique_id` moves. No `device.identifiers` moves. No
`default_entity_id` moves. No config topic is added, removed or
renamed. No `qos` or `retain` flag moves. No topic is removed.** Nothing
re-registers in either Home Assistant registry.

All five digests were updated **by hand**, in the commits that moved the
bytes, each naming its finding: F2 moved `full.*` and `nonascii.de`, F11
moved the same three, F1 moved all five. `-update-surface-golden` still
refuses to write them — it prints and fails, three times here, as
designed. Census literals: 147 → 151 messages on the three `full`
scenarios; the QoS 0 state count 305 → 317.

### The release note step 7 owes

F1's finding says the fix "belongs in the defect step with a release
note". The fix is here; the note is not, because `changelog.md` and
`addon/CHANGELOG.md` are release-scoped in this repository — every
section is a shipped version, and `make release` reads the version from
`internal/version/version.go`. Opening an `Unreleased` heading would
put a section in those files that no tag matches. Step 7 bumps the
version and writes the sections; this is the text it owes, so it is not
lost between the two:

- **Devices can now report being unreachable.** The `reachable`
  connectivity sensor matched only the value `ONLINE` and nothing at
  all on the nine other states a device can be in, so it turned on when
  a device first appeared and never turned off again — while staying
  available, so nothing indicated the reading was stale. The same
  applied to each port's link sensor (which could not report `UNKNOWN`)
  and to the site's WAN connectivity sensor. All three now report both
  states. **Existing installations see these entities change value on
  the first poll after the upgrade**, which for anything that was
  actually offline is the first correct reading it has given.
- **The Locate switch reflects the LED.** Its state topic was never
  written, so the entity sat at `unknown`: a press worked and the
  switch never moved. The LED state is now read back from the console.
  A press also refreshes promptly again — as does an SSID toggle, which
  had the same problem for the same reason.
- **`MQTT_CLIENT_ID` (add-on: `mqtt_client_id`).** Two daemons on one
  broker presented the same identifier and evicted each other in a
  loop. Unset keeps exactly the identifier your installation presents
  today, so there is nothing to do unless you run two.

---

## Sequencing — the rest of phase 9

Ordered so each step de-risks the next, following the shape phases 5–8
converged on.

| Step | Work | Why here |
| ---: | --- | --- |
| **0** | **This PR.** Bump `go-mqtt` v1.3.0 → v1.5.1; add the five-scenario surface pin, the digests, the twelve builder invariants and `.gitattributes`; measure and record F1–F13. | Nothing can be proved byte-equal against a surface that was never captured. |
| 1 | Housekeeping the bump enables, payloads untouched — the goldens must not move. | Small, mechanical, and a free check that the pins do not fire on a non-payload change. |
| 2 | **Fix the defects, one commit per finding, goldens regenerated with the diff reviewed.** F2 (`omitempty` on `state_topic` and `optimistic`) and F11 (publish the locate flag) first — both are small and both gate step 6. Then F1 (`payload_off`, plus `wan_connectivity`), F13's comment, F6's `MQTT_CLIENT_ID`. F3 and F12 are contract decisions, not defect fixes. | The byte-equality proof in step 5 must compare against *corrected* bytes. F2 first because a blocking validation result costs a device its whole entity set at step 6; F1 first-equal because it is the only finding that makes a live installation report something untrue. |
| 3 | **Decide the three questions that are not implementation.** (a) F4: a consumer `discovery.Context` reproducing `collapseTokens(slugify(name) + "_" + slugify(key))`, or accept entity-id re-registration for non-ASCII names. (b) F8: a `Layout` whose device-availability slot is the state topic with a `value_template`, or publish a real `…/availability` topic and change 187 payloads. (c) F6: whether the site device is re-keyed onto the site UUID, and whether an `INSTANCE_ID` lands before the bundle. | All three are irreversible for an installed base. Phase 6 hit (a) at step 3 and paid for it. |
| 3b | **Settle §3.4's unknowns against a live Home Assistant.** One `mosquitto_pub` of a non-`ONLINE` state to settle F1's latch-or-blank; one throwaway bundle carrying a `device_tracker` component, HA 2026.9, watch the log. | Half a day, and the `device_tracker` question gates step 6 for the one bridge whose presence plane is the point of the phase. |
| 4 | **Model the fleet as `model.Entity` and render bundles, publishing nothing.** The device graph as `via_device`/parent identity; ports, radios and SSIDs as the dynamic component sets the rollout table nominates; the per-entity 128/187 availability split as per-component `Availability`. Compare the rendered per-component output **against the golden files, not against the builder it replaces**. Neither pin regenerated. | This is where a `Layout`, `Context`, node-id or availability mismatch surfaces, at zero risk — and it is the part ADR 0070 nominates unifi to prove. |
| 5 | Adopt the library on the **state and command** planes, discovery still per-entity from the old path. Spell `publisher.QoSAtMostOnce` on the state plane and leave discovery and availability at QoS 1 (F9). | The state plane has no registry keys to orphan; it is the cheap half, and it is where F9's split trap lives. |
| 6 | **Switch discovery to the device bundle.** `PublishBundle` + `SupersededTopics(prefix, bundle)` with the **default** five-segment form (F5) and a bundle `NodeID` byte-equal to `device.identifiers[0]`; teach `IsOwnConfig` and `ConfigFilter` to recognise a four-segment bundle in both directions. Verify against a live HA that no `Received a conflicting MQTT discovery message` warning appears. | The one step no unit test can prove. Everything above exists to make it a small diff. |
| 7 | Apply the step-3 decisions, add the `origin` block (F7), the four missing entities of F3 if that is the call, `changelog.md` + `addon/CHANGELOG.md`, version bump across the spots `CLAUDE.md` names. | Operator-visible last. |

### What I would not do

- **Do not swap `slugify` for `topic.Slug` as a convenience.** Two entity keys
  change and every German device name does; the win is cosmetic and the cost is
  every entity id on the device.
- **Do not state `LegacyEntityTopics` at step 6 without re-reading F5.** This
  is the one bridge in the programme whose fleet the *default* already
  retracts, and the field replaces rather than extends — stating
  `LegacyTopicByUniqueID` here would turn a working retraction into none.
- **Do not accept the availability default without reading F8.** The shape
  matches and the topic does not, which is the harder failure to see: the
  payload looks right and the entity is grey.
- **Do not fix F1, F2 and F11 in the migration step.** All three move retained
  bytes on an installed base. A migration step whose golden diff is empty is
  provable; one whose diff is a hundred rows is not.
- **Do not remove F3's unread topics as tidying.** Retiring a topic an
  installed base may already consume is an operator-contract decision; three
  prior phases declined to make it inside the migration.
- **Do not shrink the pins.** The 45 sensors of a minimal read-only install
  that nobody looks at are exactly where an identity regression hides.
- **Do not regenerate a golden to make a step pass.** Every regeneration in
  steps 2 and 7 must come with a reviewed diff and a hand-updated digest naming
  the finding; steps 1, 4, 5 and 6 must regenerate nothing.

---

## Appendix — how the measurement was taken

Everything in this document was produced by the two pin test files it
describes, against this repository's own working tree, with two exceptions,
both in `surface_invariants_test.go` and both unreachable from any production
path:

- `librarySlug` — a verbatim transcription of go-hamqtt v0.32.0's `topic.Slug`
  (`topic/topic.go`), kept there so §3.3 can be counted without taking the
  dependency one step early. It deletes itself at step 4.
- `hassSlugProbe` — a transcription of `internal/hass.slugify`, which is
  unexported, kept beside `librarySlug` so the two are read together.

§6's validation was run out of tree, from a throwaway module depending on
go-hamqtt v0.32.0, over the golden files rather than over live builder output,
so nothing in this repository's `go.mod` moved for it.

The fixtures are synthesised rather than captured, because
`internal/unifi/integration/testdata/` holds *API responses*, not domain
objects, and the coordinator consumes the latter. Their values follow the
shipped fixtures: the site is `{ID: "site-uuid", Internal: "default"}`, the
SSID ids are UUID-shaped with hyphens as `wlans.json` has them, and the MACs
are in the `00:00:5e:00:53:xx` documentation range.

Commands that reproduce the numbers:

```sh
# the whole pinned surface, and every count in §2.2
go test ./internal/coordinator -run TestSurfaceCensus -v

# the topic form (§2.3), the identity plane (§3.1-3.2), the availability
# model (§4) and the delivery guarantee (§2.6)
go test ./internal/coordinator -run 'TestConfigTopicForm|TestConfigFilter|TestNoDuplicate|TestDeviceBlocks|TestIdentityIs|TestAvailabilityModel|TestPublishQoS' -v

# the slug (§3.3), with every divergent probe logged
go test ./internal/coordinator -run TestSlugAgreementOverTheRealCatalogue -v

# the two vocabularies against each other (F10, F11)
go test ./internal/coordinator -run 'TestAdvertised|TestKnownUnpublished|TestCommandTopicsAreSubscribed' -v

# regenerate the goldens; this FAILS and prints the new digests to paste in
go test ./internal/coordinator -run TestPublishedSurfaceGolden -update-surface-golden
```

The LOC figures use `git ls-files` and brace-matched function extents, not
`awk`-to-next-`func` spans.
