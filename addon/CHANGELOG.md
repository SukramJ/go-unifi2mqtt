# Version 1.2.0 (2026-09-13)

The release that comes out of a full measurement of everything this
bridge puts on an MQTT broker: every discovery payload, every topic,
every identity string, pinned byte for byte and then corrected. Four
entities that were reporting something untrue now report the truth, two
failures that only showed up after a broker restart are gone, and the
discovery payloads gained an `origin` block.

**Nothing re-registers.** No `unique_id`, no device identifier and no
entity_id seed moved in this release, so every entity keeps its
history, its area, its name and every automation pointing at it.

## Devices, ports and the WAN can now report being *not* reachable

The `reachable` connectivity sensor matched the value `ONLINE` and
nothing at all on the nine other states a UniFi device can be in
(`OFFLINE`, `UPDATING`, `ADOPTING`, `ISOLATED`, …). A Home Assistant
binary sensor ignores a payload that matches neither of its two
configured values, so the sensor turned *on* when a device first
appeared and never turned off again — while staying *available*, so
nothing indicated the reading was stale. A connectivity sensor that
reported every device permanently reachable, whatever the console said.

The same shape affected each port's link sensor (which could not report
a link the console cannot read) and the site's WAN connectivity sensor.

All three now report both states. **Existing installations will see
these entities change value on the first poll after the upgrade** —
which, for anything that was actually offline, is the first correct
reading they have given.

## The Locate switch reflects the LED

Its state topic was never written by anything, so the entity sat at
`unknown` forever: a press worked, the LED came on, and the switch did
not move. The LED state is now read back from the console and
published.

A press also refreshes promptly again, as does an SSID toggle — both
were waking a poll loop that does not publish their state, so the
entity kept its old value until the next hourly poll.

## Two bridges on one broker: `MQTT_CLIENT_ID`

Two daemons with the shipped configuration both presented the client
identifier `unifi2mqtt-unifi`, and MQTT requires a broker to disconnect
the existing session when a second client presents the same identifier.
They evicted each other in a loop, neither published reliably, and
nothing in either log said why.

`MQTT_CLIENT_ID` (add-on: `mqtt_client_id`) sets it explicitly. Leaving
it unset keeps exactly the identifier your installation presents today,
so there is nothing to do unless you run two.

## Stale-config cleanup only removes what this run published

The startup cleanup used to remove any retained discovery config that
*looked* like one of ours. Two UniFi consoles bridged to one broker
publish byte-identical config topics, unique ids, availability topics
and state topics for the whole site plane, so on such a setup "looks
like mine" meant one console's bridge deleting the other console's
entities out of Home Assistant — silently, because Home Assistant says
nothing when entities disappear with their retained configs.

The rule is now ownership by record: a config is removed only if **this
run of the daemon published that exact topic**. The cost is stated
rather than hidden — a config genuinely left behind by an *earlier*
run, for example from a device you removed while the bridge was not
running, is no longer removed either, because from the broker it is
indistinguishable from a second console's live entity. Those are listed
in the log once, as `coordinator.reconcile_unclaimed` with the full
topic, and an operator who knows there is no second console can clear
each with one empty retained publish. `HASS_CLEANUP: false` still turns
the whole sweep off.

## Discovery payloads name their origin, and the site device has one name

Every discovery config now carries an `origin` block identifying
`go-unifi2mqtt` as the integration that announced it, which Home
Assistant shows on the device page. It is additive and keys nothing.

The synthetic "UniFi Site" device was announced under two spellings —
`UniFi Site <site name>` by the health sensors and
`UniFi Site <internal reference>` by the SSID switches — which Home
Assistant resolved by whichever config happened to arrive last. Both
now use the site's display name. **On most installations this changes
nothing visible**, because the two differed only in one letter's case
and the health sensors already wrote last; on a console whose display
name differs from its internal reference, the site device is renamed to
the display name. The registry keys never used the name, so nothing
re-registers.

Client devices also stop reporting an empty manufacturer and report
none instead — which is what Home Assistant already displayed.

## Two failures that only appeared after the broker came back

Both are invisible until the thing they fix happens, and both left the
fleet in a state nothing reported:

- **A broker that comes back without its retained store** — a restart
  without persistence, or a failover to a fresh node — now gets every
  class of discovery config again. Client and site-health configs were
  announced once per daemon run and would not have come back at all
  until the daemon itself was restarted; device and SSID configs would
  have come back on the hourly cycle.
- **The bridge availability marker is no longer lost to the circuit
  breaker.** A connection drop is exactly what trips the breaker, and
  the first thing a reconnected daemon does is announce itself online —
  which the tripped breaker refused. The retained marker stayed at the
  `offline` the Last Will had just written, and **every entity sat
  unavailable** while the daemon was demonstrably connected and
  publishing. Birth and death now bypass the breaker; everything else
  still goes through it.

## Under the hood

- `object_id` is no longer published. Home Assistant's MQTT discovery
  schemas drop keys a platform does not declare, and `object_id` is
  accepted by 0 of the 32 platforms on 2026.9; `default_entity_id`,
  which every payload already carried alongside it, is the seed that is
  actually read. The device Locate switch's seed also named the wrong
  domain (`button.…` on a `switch` entity) and is corrected — on a new
  installation it now gets the entity id it asks for.
- Eighteen button payloads carried two keys the button schema does not
  declare (`state_topic`, `optimistic`). Home Assistant dropped them on
  arrival and the buttons worked, so nothing visible changes; they are
  gone because a payload should say what it means.
- The state, command and birth/availability planes now publish through
  the shared `go-hamqtt` library, with the delivery guarantee stated
  explicitly rather than inherited: discovery and availability at
  QoS 1, state at QoS 0, exactly as before. `go-hamqtt` moves to 0.34.0
  and `go-mqtt` is at 1.5.1.
- Discovery stays **one retained config per entity**. Home Assistant
  also accepts a single "device bundle" config per device; that form
  was measured, built, proved byte-equal and then deliberately not
  published, because two UniFi consoles that cannot be told apart would
  replace each other's whole entity set instead of overwriting it entity
  by entity. The full reasoning is in the repository, in
  `notes/adr0070-phase9-measurement.md`.

# Version 1.1.0 (2026-08-16)

A dependency release: the MQTT client library go-mqtt moves from 1.2.0
to 1.3.0, an audit release that fixed 42 findings from a full-codebase
adversarial review. No bridge code changed — everything below arrives
through the library.

## Reconnects are damped when the broker flaps

A connection that dies within ten seconds of coming up now counts as
flapping and reconnects with an exponentially growing delay instead of
redialling immediately — the pattern two instances fighting over one
ClientID (or a draining broker) produce, which previously turned into
a full-speed reconnect storm. A connection that was stable and then
drops still reconnects instantly, and an intentional shutdown no
longer risks a spurious reconnect racing the broker's own socket
close.

## The circuit breaker only trips on real broker trouble

Client-side validation errors (an invalid topic, a QoS the broker does
not allow) no longer count toward opening the circuit, so one bad
topic cannot block every healthy publish for the recovery window. The
breaker also discards outcomes of publishes that started before a
state change, and `unifi2mqtt.mqtt_breaker_state` transitions are now
delivered in order — the logged state always reflects broker health.

## Hardened wire handling

The client validates inbound frames strictly per MQTT 5.0 (malformed
broker traffic tears the connection down cleanly instead of being
carried along), QoS 2 exchanges recover when broker and client
disagree about an identifier, and payload buffers handed to Publish
may now be reused immediately — the library copies what it must keep
for a retransmit.

# Version 1.0.1 (2026-08-14)

A fix release for two things a German Home Assistant install surfaced,
plus the cleanup that makes stale entities disappear on their own.

## Entity ids stay English

Discovery seeded the entity_id with `object_id` alone. Home Assistant
Core deprecated that key in 2025.10 and removed it in 2026.4, so on a
current release nothing seeded the id and HA derived it from the
display name — which is translated. A `de` install ended up with
`sensor.cpu_auslastung`.

Both `object_id` and `default_entity_id` are published now, because
neither works everywhere on its own: the first is gone in current
releases, the second is not honoured reliably in older ones. The seed
is also readable rather than a MAC —
`sensor.unifi_sw_har_cpu_utilization`, not
`sensor.unifi_f492bf8394ba_cpu_utilization` — and slugs the way Home
Assistant itself does, so an umlaut loses its diaeresis (`Süd` → `sud`)
instead of expanding to `sued` and disagreeing with the id HA would
derive for the same name.

`unique_id` is unchanged, so the update orphans nothing. It also means
entities that already exist keep the ids they were given: Home
Assistant keys them by `unique_id`, so no update can rename them.
Delete the affected entities under Settings → Devices & Services →
MQTT and restart to have them recreated with English ids.

## Orphaned entities are removed

Until now the daemon could only clear entities it had seen appear and
disappear within one run. A discovery config left behind by an earlier
version, by a device removed while the daemon was stopped, or by a
filter that no longer matches, stayed retained on the broker — so Home
Assistant recreated the entity on every start and it sat there
unavailable forever, with nothing to say where it came from.

On start the daemon now reads the retained configs under the discovery
prefix and clears the ones it owns but no longer publishes. Ownership
requires two independent signals to agree: the `unique_id` has to be in
this project's namespace **and** the payload has to name this bridge's
availability topic. A second instance bridging another console to the
same broker under its own `MQTT_TOPIC` is therefore correctly seen as
somebody else's, and another integration's entities are never touched.

The sweep waits for each source to report before it will remove
anything of that kind. An empty announced set means "not polled yet"
until the device, client, WLAN or health poll says otherwise —
publishing nothing because a poll failed must never read as "these
entities are gone". A source that is switched off is the exception, and
deliberately so: turning `CLIENTS.ENABLE` off is a decision that those
entities should disappear.

`HASS_CLEANUP: false` (add-on: **Remove orphaned entities**) turns it
off.

## Add-on options are labelled

The add-on configuration page rendered every option as its raw
identifier — `controls_port_power_cycle` with no hint of what it does —
because `config.yaml` carries no labels and no translation files
existed. All 40 options now have a name and a description in English
and German.

# Version 1.0.0 (2026-08-14)

First release. A pure-Go daemon bridging a local UniFi Network
installation to MQTT, with Home Assistant auto-discovery.

Verified end to end against UniFi Network 10.5.67 on a 12-device site
with 121 clients.

## What it does

- **Devices** — state, reachability, uptime, CPU, memory, uplink rates,
  firmware and update-available, per-port link/speed/PoE, per-radio
  channel and TX retries.
- **Clients** — presence as `device_tracker`, IP, network and VLAN.
  Filtered by connection type, network, VLAN, SSID and MAC lists.
- **Site health** — WAN status, WAN IP, latency, throughput and client
  counts.
- **Controls** — restart a device, power-cycle a PoE port, authorize a
  guest, toggle the locate LED, block a client, switch an SSID.
- **Home Assistant discovery** — entities appear on their own, wired
  into the real topology: `via_device` links client → AP → switch →
  gateway.
- **Diagnostic web UI** — read-only status page, doubling as the
  add-on's Ingress panel.

Ships as a static binary, a distroless Docker image and a Home Assistant
add-on.

## Design decisions worth knowing

- **The official API comes first.** Everything runs on the documented
  Network Integration API (`X-API-KEY`, Network 10.5+). The undocumented
  classic API is opt-in and fills only the gaps it must: site health,
  per-client SSID and signal, PoE wattage, client blocking, WLAN
  toggles. If it fails, those capabilities switch off and the daemon
  keeps running — the difference between "site health is missing" and
  "the bridge is down".
- **Safe by default.** Clients, controls and the web UI are all off
  until you turn them on. A read-only UniFi admin is enough for the API
  key unless you enable controls.
- **The MAC is the identity.** Topics and Home Assistant `unique_id`s
  are keyed on the normalised MAC, never the API's UUIDs — those change
  on re-adopt or a controller restore and would orphan every entity
  along with its history.
- **Language changes nothing but labels.** `LANGUAGE: de` renames what
  you see; `entity_id`, `unique_id` and every topic stay English, so
  switching never re-creates an entity or loses history. Both
  `object_id` and `default_entity_id` are published, because Home
  Assistant removed the first in 2026.4 and does not honour the second
  reliably before that — with only one, some release derives the
  entity_id from the translated name.
- **Entity ids read like the network.** The seed is
  `<device>_<key>`, so an automation references
  `sensor.unifi_sw_har_cpu_utilization` rather than a MAC address.
- **Presence has a grace period.** A client stays `home` until absent
  for `AWAY_TIMEOUT` (default 300 s). Wireless clients vanish for a
  cycle while roaming between access points; flipping immediately makes
  every presence automation flap.
- **No optimistic state.** After a command the affected object is
  re-polled and the published state comes from the console, so a failed
  command snaps the entity back instead of lying.
- **Retained commands are ignored.** A stale `mosquitto_pub -r` would
  otherwise power-cycle a port every time the daemon starts.
- **Only changed values are published**, with a per-topic forced
  republish (`FORCE_REPUBLISH`, default 600 s). Measured on the
  reference site: 24 messages across three poll cycles instead of 1089.

## Requirements

- UniFi OS console or software controller, **Network 10.5+**.
- An MQTT broker.
- For the optional classic layer: a **local** UniFi admin account —
  not a Ubiquiti SSO login, and 2FA must be off.

## Notes for operators

- `unifi2mqtt --once` prints the full site inventory including each
  client's VLAN, without touching the broker. Use it to check filters
  before enabling client publication.
- `VERIFY_TLS` defaults to off because consoles serve a self-signed
  certificate on their LAN address. If you reach yours through a
  hostname with a trusted certificate, turn it on; `CA_FILE` is the
  other way to keep verification.
- Enabling controls means anyone who can publish to your broker can
  restart your network hardware. Use a broker ACL.
- Clients with no IP — about 14% on the reference site, mostly wired
  devices the console has not seen an address for — cannot match a VLAN
  filter and are skipped when one is set.

## Documentation

- [`README.md`](README.md) — quickstart, topic layout, security notes.
- [`config-template.yaml`](config-template.yaml) — every setting,
  documented inline.
- [`addon/DOCS.md`](addon/DOCS.md) — the Home Assistant add-on. Its
  configuration page is fully labelled in English and German.
- [`CONCEPT.md`](CONCEPT.md) — the design rationale, including why the
  API surface is split the way it is.
