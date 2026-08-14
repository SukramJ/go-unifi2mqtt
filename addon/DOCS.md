# go-unifi2mqtt — Add-on documentation

Bridges a local UniFi Network console to MQTT, with Home Assistant MQTT
auto-discovery. Devices, ports, radios, WLANs and — optionally —
network clients appear as entities on their own, wired into your actual
network topology.

## Prerequisites

- A UniFi OS console (UDM / UDM-Pro / UDR / UCG / UX / UniFi OS Server)
  or a software controller, reachable from your Home Assistant host.
- **UniFi Network 10.5 or newer** for the official Integration API.
  Older versions may work but are untested.
- An MQTT broker. Leaving `mqtt_server` empty makes the add-on use the
  broker the Supervisor already knows about (the MQTT integration or the
  core-mosquitto add-on).

## Getting started

1. UniFi Network → **Settings → Control Plane → Integrations** → create
   an API key. It is shown exactly once.
2. Fill in `unifi_host` and `unifi_api_key`.
3. Start the add-on. Devices appear in Home Assistant within a minute.

The API key inherits the permissions of the admin that created it. With
`controls_enable` off, a **read-only** admin is enough — and is the
recommended setup.

## What you get

| Object | Entities |
| --- | --- |
| Device | state, reachable, uptime, CPU, memory, uplink TX/RX, firmware, update available |
| Port | link, speed, PoE state, PoE watts (classic layer) |
| Radio | channel, TX retries |
| Client | presence (`device_tracker`), IP, signal (classic layer) |
| Site | WAN connectivity, WAN IP, latency, throughput, client counts (classic layer) |
| WLAN | enabled switch (classic layer, with controls on) |

Devices are linked by `via_device`, so Home Assistant draws the real
hierarchy: gateway → switch → access point → client.

## Options

### UniFi console

| Option | Default | Description |
| --- | --- | --- |
| `unifi_host` | — | IP or hostname of the console. Required. |
| `unifi_port` | `443` | `443` UniFi OS · `8443` software controller · `11443` UniFi OS Server |
| `unifi_api_key` | — | Integration API key. Required. |
| `unifi_site` | `default` | Site to bridge. |
| `unifi_verify_tls` | `false` | Verify the console's certificate. Off by default because consoles present a self-signed certificate on their LAN address. If you reach the console through a hostname with a trusted certificate, turn this on. |

### Classic API fallback

The official Integration API exposes no site health, no per-client SSID
or signal strength, no PoE wattage, no client blocking and no WLAN
toggle. Enabling this adds a cookie-session client against the classic
controller API to fill those gaps.

| Option | Default | Description |
| --- | --- | --- |
| `classic_enable` | `false` | Enable the classic client. |
| `classic_username` | — | A **local** UniFi admin (Settings → Admins). Not a Ubiquiti SSO account, and 2FA must be off — neither can be satisfied non-interactively. |
| `classic_password` | — | That account's password. |

> The classic API is undocumented and can change between controller
> releases. Everything it powers is optional: if the login fails, those
> capabilities simply stay unavailable and the add-on keeps running on
> the official API.

### MQTT

| Option | Default | Description |
| --- | --- | --- |
| `mqtt_server` | — | Broker host. **Leave empty** to use the Home Assistant MQTT service. |
| `mqtt_port` | `1883` | Broker port. |
| `mqtt_login` / `mqtt_password` | — | Broker credentials. |
| `mqtt_topic` | `unifi` | Root of the published topic tree. |
| `hass_enable` | `true` | Publish Home Assistant discovery. |
| `hass_base_topic` | `homeassistant` | Discovery prefix. Only change this if you changed it in the MQTT integration too. |

### Polling

| Option | Default | Description |
| --- | --- | --- |
| `refresh_devices` | `60` | Device list: state, firmware, update flag. |
| `refresh_clients` | `30` | Client list — sets presence latency. |
| `refresh_health` | `60` | Site health (needs `classic_enable`). |
| `refresh_static` | `3600` | Device details (ports, radios, uplinks) and the network/WLAN catalogues. These change rarely and cost one request per object, so they sit on a slow loop. |

### Clients and presence

**Off by default.** A busy network would otherwise create hundreds of
entities on first start — the reference installation has 121 clients.

A client is published only when it matches **every** non-empty filter.
Empty lists mean "no restriction on this dimension".

| Option | Default | Description |
| --- | --- | --- |
| `clients_enable` | `false` | Publish clients at all. |
| `clients_types` | `[WIRELESS]` | `WIRED`, `WIRELESS`, `VPN`, `TELEPORT`. |
| `clients_networks` | `[]` | Network names, e.g. `["LAN", "IoT"]`. |
| `clients_vlans` | `[]` | VLAN IDs, e.g. `[1, 20]`. |
| `clients_ssids` | `[]` | SSIDs. **Needs `classic_enable`** — the Integration API does not report a client's SSID. |
| `clients_include_macs` | `[]` | When set, **only** these MACs. This is an either/or: it bypasses every other filter, it does not add to them. |
| `clients_exclude_macs` | `[]` | Never published. Beats everything, including the allowlist. |
| `clients_exclude_guests` | `true` | Skip guest-network clients. |
| `clients_max` | `100` | Hard cap. Reaching it logs a warning naming how many matched. |
| `clients_away_timeout` | `300` | Seconds a client may be absent before presence flips to `not_home`. Below `2 × refresh_clients` this flaps: wireless clients drop out for a cycle while roaming between access points. |
| `clients_signal_sensor` | `false` | Signal-strength sensor per wireless client. Needs `classic_enable`. |

**Example** — only the wireless clients on the IoT VLAN, never the
printer:

```yaml
clients_enable: true
clients_types: ["WIRELESS"]
clients_vlans: [20]
clients_exclude_macs: ["aa:bb:cc:11:22:33"]
```

Note that clients without an IP cannot be matched against a VLAN filter
and are therefore skipped — on the reference installation that is about
14% of them, mostly wired devices the console has not seen an address
for.

### Controls

`controls_enable` is the master switch; each control then decides
individually what Home Assistant may do.

| Option | Default | Needs classic | Entity |
| --- | --- | --- | --- |
| `controls_enable` | `false` | — | master switch |
| `controls_device_restart` | `true` | no | button per device |
| `controls_port_power_cycle` | `true` | no | button per PoE port |
| `controls_guest_authorize` | `true` | no | button per guest |
| `controls_device_locate` | `false` | **yes** | switch per device |
| `controls_client_block` | `false` | **yes** | switch per client |
| `controls_wlan_enable` | `false` | **yes** | switch per SSID |

Turning a classic-only control on without `classic_enable` is a startup
error rather than a silently dead entity.

> **Anyone who can publish to your broker can restart your network
> hardware.** Restrict write access with a broker ACL before enabling
> controls.

### Miscellaneous

| Option | Default | Description |
| --- | --- | --- |
| `web_enable` | `true` | Serve the diagnostic UI as an Ingress panel. |
| `language` | `en` | `en` or `de`. Affects display names only — entity ids never change, so switching language never re-creates entities or loses history. The configuration page you are reading options on is translated too. |
| `debug` | `false` | Verbose logging. |

## The diagnostic panel

With `web_enable` on, the add-on adds a sidebar panel showing the broker
link, every poll loop with its age, which classic capabilities are live,
site health, all devices with live statistics and PoE draw, and the
published clients. It is read-only: it answers "is the bridge doing what
it should?", nothing more.

## Troubleshooting

**`401` on every request** — the API key is wrong or belongs to a
different console. Regenerate it under Settings → Control Plane →
Integrations.

**`404` on `/proxy/network/integration/v1/…`** — the console runs a
Network version older than 10.5.

**Classic login fails with "Invalid username or password"** — UniFi OS
answers a rejected login with HTTP 403. Check that the account is a
**local** admin rather than a Ubiquiti SSO login, and that 2FA is off.
The add-on keeps running without it; only the classic-only features stay
unavailable.

**TLS errors** — leave `unifi_verify_tls` off for a console reached by
IP; it presents a self-signed certificate.

**No entities appear** — check that `hass_enable` is on, the MQTT
integration is configured, and the log shows a successful broker
connect. After a Home Assistant restart the add-on re-announces
everything automatically.

**Entity ids came out in German** — entities created before version
1.0.1 could pick up their id from the translated name. Home Assistant
keys entities by `unique_id`, so an update cannot rename them: delete
the affected entities (or the whole device) under Settings → Devices &
Services → MQTT, then restart the add-on. They come back with English
ids and translated display names.

**Site health is missing** — it needs `classic_enable`. The Integration
API's WAN endpoint returns only an id and a name.
