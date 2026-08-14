# go-unifi2mqtt — Add-on documentation

Bridges a local UniFi Network console to MQTT, with Home Assistant MQTT
auto-discovery.

## Prerequisites

- A UniFi OS console (UDM / UDM-Pro / UDR / UCG / UX / UniFi OS Server) or a
  software controller, reachable from your Home Assistant host.
- **UniFi Network 10.5 or newer** for the official Integration API. Older
  versions can only be used with the classic API fallback (see below).
- An MQTT broker. Leaving `mqtt_server` empty makes the add-on use the broker
  the Supervisor already knows about (the MQTT integration / core-mosquitto).

## Creating the API key

1. Open UniFi Network → **Settings → Control Plane → Integrations**.
2. Create an API key and copy it — it is shown exactly once.
3. Paste it into the add-on's `unifi_api_key` option.

The key inherits the permissions of the admin that created it. If you leave
`controls_enable` off, a **read-only** admin is sufficient and is the
recommended setup.

## Options

### UniFi console

| Option             | Default   | Description                                                                                     |
| ------------------ | --------- | ----------------------------------------------------------------------------------------------- |
| `unifi_host`       | —         | IP or hostname of the console, e.g. `192.168.1.1`. Required.                                    |
| `unifi_port`       | `443`     | HTTPS port. Use `443` for UniFi OS, `8443` for a standalone software controller, `11443` for UniFi OS Server. |
| `unifi_api_key`    | —         | The Integration API key. Required.                                                              |
| `unifi_site`       | `default` | Site to bridge. `default` is the primary site on most installs.                                 |
| `unifi_verify_tls` | `false`   | Verify the console's TLS certificate. Off by default because consoles ship a self-signed certificate on their LAN address. |

### Classic API fallback

The official Integration API exposes no site health, no client blocking and no
WLAN on/off. Enabling the fallback adds a cookie-session client against the
classic controller API to fill those gaps.

| Option              | Default | Description                                                                        |
| ------------------- | ------- | ---------------------------------------------------------------------------------- |
| `classic_enable`    | `false` | Enable the classic API client.                                                     |
| `classic_username`  | —       | **Local** UniFi admin account (not a Ubiquiti SSO login). 2FA must be off.          |
| `classic_password`  | —       | Password for that account.                                                         |

> The classic API is undocumented and can change between controller releases.
> Everything it powers is optional — with `classic_enable: false` the add-on
> runs entirely on the officially supported surface.

### MQTT

| Option          | Default | Description                                                                   |
| --------------- | ------- | ------------------------------------------------------------------------------ |
| `mqtt_server`   | —       | Broker host. **Leave empty** to use the Home Assistant MQTT service.           |
| `mqtt_port`     | `1883`  | Broker port.                                                                   |
| `mqtt_login`    | —       | Broker username.                                                               |
| `mqtt_password` | —       | Broker password.                                                               |
| `mqtt_topic`    | `unifi` | Root of the published topic tree, e.g. `unifi/default/device/<mac>/state`.     |

### Polling

| Option             | Default | Description                                                            |
| ------------------ | ------- | ----------------------------------------------------------------------- |
| `refresh_devices`  | `60`    | Seconds between device polls (state, uptime, CPU, memory, ports).      |
| `refresh_clients`  | `30`    | Seconds between client polls — drives presence detection latency.      |
| `refresh_health`   | `60`    | Seconds between site-health polls (needs `classic_enable`).            |

### Clients / presence

Off by default: a busy network would otherwise create hundreds of Home
Assistant entities on first start. A client is published only when it matches
**every** non-empty filter list — empty lists mean "no restriction on this
dimension".

| Option                  | Default      | Description                                                                |
| ----------------------- | ------------ | --------------------------------------------------------------------------- |
| `clients_enable`        | `false`      | Publish clients at all.                                                    |
| `clients_types`         | `[WIRELESS]` | Connection types to include: `WIRED`, `WIRELESS`, `VPN`, `TELEPORT`.       |
| `clients_networks`      | `[]`         | Network / VLAN names, e.g. `["LAN", "IoT"]`.                               |
| `clients_vlans`         | `[]`         | VLAN IDs, e.g. `[1, 20]`.                                                  |
| `clients_ssids`         | `[]`         | SSIDs, e.g. `["Home", "IoT"]`.                                             |
| `clients_include_macs`  | `[]`         | When non-empty, **only** these MACs are published — every other filter is bypassed. |
| `clients_exclude_macs`  | `[]`         | MACs never published, applied last.                                        |
| `clients_max`           | `100`        | Hard cap on published clients. Excess clients are skipped with a warning.  |

### Controls

| Option            | Default | Description                                                                                |
| ----------------- | ------- | -------------------------------------------------------------------------------------------- |
| `controls_enable` | `false` | Expose write-back entities: restart device, power-cycle PoE port, locate LED, block client, toggle WLAN. |

Leaving this off makes the add-on strictly read-only, which pairs with a
read-only UniFi admin for the API key.

### Miscellaneous

| Option       | Default | Description                                            |
| ------------ | ------- | -------------------------------------------------------- |
| `web_enable` | `true`  | Serve the diagnostic web UI through the Ingress panel. |
| `language`   | `en`    | Display language for HA friendly names (`en`, `de`).   |
| `debug`      | `false` | Verbose logging.                                       |

## Troubleshooting

**`401 Unauthorized` on every request** — the API key is wrong or was created
on a different console. Regenerate it under Settings → Control Plane →
Integrations.

**`404` on `/proxy/network/integration/v1/...`** — the console runs a UniFi
Network version older than 10.5. Upgrade, or run with `classic_enable: true`
only.

**TLS errors** — leave `unifi_verify_tls` off for a console reached by IP; it
presents a self-signed certificate.

**No entities in Home Assistant** — check that `hass_enable` is on, the MQTT
integration is set up, and the add-on log shows a successful broker connect.
