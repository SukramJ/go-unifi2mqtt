// SPDX-License-Identifier: MIT
//
// The whole UI: fetch a snapshot, render it, repeat. No framework and
// no build step, so the assets can be embedded straight into the binary
// and the Home Assistant add-on needs nothing extra.
//
// Every URL here is relative. The Ingress proxy serves this page under
// a generated path prefix, and an absolute /api/state would escape it.

const REFRESH_MS = 5000;

const I18N = {
  en: {
    problems: "Problems", loops: "Poll loops", health: "Site health",
    devices: "Devices", clients: "Clients", wlans: "WLANs",
    name: "Name", model: "Model", state: "State", uptime: "Uptime",
    memory: "Memory", throughput: "Throughput", uplink: "Uplink",
    type: "Type", presence: "Presence", address: "Address",
    network: "Network", signal: "Signal",
    clientsOff: "Client publication is off (CLIENTS.ENABLE).",
    home: "home", away: "away", never: "never", updated: "updated",
    offline: "no connection to the daemon",
  },
  de: {
    problems: "Probleme", loops: "Abfrageschleifen", health: "Site-Zustand",
    devices: "Geräte", clients: "Clients", wlans: "WLANs",
    name: "Name", model: "Modell", state: "Status", uptime: "Betriebszeit",
    memory: "Speicher", throughput: "Durchsatz", uplink: "Uplink",
    type: "Typ", presence: "Anwesenheit", address: "Adresse",
    network: "Netzwerk", signal: "Signal",
    clientsOff: "Client-Veröffentlichung ist aus (CLIENTS.ENABLE).",
    home: "anwesend", away: "abwesend", never: "nie", updated: "aktualisiert",
    offline: "keine Verbindung zum Daemon",
  },
};

let lang = "en";
const t = (key) => (I18N[lang] && I18N[lang][key]) || I18N.en[key] || key;

const $ = (sel) => document.querySelector(sel);

function fmtDuration(seconds) {
  if (seconds === null || seconds === undefined || seconds < 0) return "—";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${Math.floor(seconds)}s`;
}

function fmtRate(bps) {
  if (!bps) return "—";
  const units = ["bit/s", "kbit/s", "Mbit/s", "Gbit/s"];
  let v = bps, i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

function stateClass(s) {
  if (s === "ONLINE") return "state-online";
  if (s === "OFFLINE") return "state-offline";
  return "state-other";
}

// Text nodes only — never innerHTML with values from the console. A
// device named `<img onerror=…>` is a perfectly legal UniFi name.
function cell(text, className) {
  const td = document.createElement("td");
  td.textContent = text ?? "—";
  if (className) td.className = className;
  return td;
}

function chip(key, value, cls) {
  const el = document.createElement("span");
  el.className = "chip" + (cls ? " " + cls : "");
  const k = document.createElement("span");
  k.className = "k";
  k.textContent = key;
  const v = document.createElement("span");
  v.textContent = value;
  el.append(k, v);
  return el;
}

function applyStaticLabels() {
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
}

function renderBadges(d) {
  const mqtt = $("#mqtt-badge");
  mqtt.textContent = d.bridge.mqtt_connected ? "MQTT ✓" : "MQTT ✗";
  mqtt.className = "badge" + (d.bridge.mqtt_connected ? "" : " bad");

  $("#site-badge").textContent = d.site.name
    ? `${d.site.name} · UniFi ${d.site.application_version}`
    : "";
  $("#version-badge").textContent = `v${d.bridge.version} · ${fmtDuration(d.bridge.uptime_s)}`;
}

function renderAlerts(d) {
  const section = $("#alerts");
  const list = $("#alert-list");
  list.replaceChildren();

  const problems = [];
  if (!d.bridge.mqtt_connected) problems.push("MQTT: disconnected");
  for (const e of d.errors || []) problems.push(`${e.loop}: ${e.error}`);
  for (const [cap, ok] of Object.entries(d.bridge.capabilities || {})) {
    if (!ok) problems.push(`capability unavailable: ${cap}`);
  }

  for (const p of problems) {
    const li = document.createElement("li");
    li.textContent = p;
    list.append(li);
  }
  section.hidden = problems.length === 0;
}

function renderLoops(d) {
  const box = $("#loops");
  box.replaceChildren();
  for (const l of (d.loops || []).sort((a, b) => a.name.localeCompare(b.name))) {
    // A loop that has not completed in a while is the clearest sign
    // something is wrong, so it gets its own colour before it errors.
    const cls = l.failed ? "bad" : l.age_s > 300 || l.age_s < 0 ? "stale" : "";
    box.append(chip(l.name, l.age_s < 0 ? t("never") : `${fmtDuration(l.age_s)} ago`, cls));
  }
}

function renderHealth(d) {
  const section = $("#health-section");
  if (!d.health) { section.hidden = true; return; }
  section.hidden = false;

  const h = d.health;
  const cells = [
    ["WAN", h.wan], ["LAN", h.lan], ["WLAN", h.wlan], ["VPN", h.vpn],
    ["WAN IP", h.wan_ip || "—"],
    ["Latency", h.latency_ms === null ? "—" : `${h.latency_ms} ms`],
    ["↓", fmtRate(h.rx_bps)], ["↑", fmtRate(h.tx_bps)],
    [t("clients"), `${h.clients_user + h.clients_guest + h.clients_iot}`],
  ];

  const box = $("#health");
  box.replaceChildren();
  for (const [k, v] of cells) {
    const cellEl = document.createElement("div");
    cellEl.className = "cell";
    const ks = document.createElement("span");
    ks.className = "k";
    ks.textContent = k;
    const vs = document.createElement("span");
    vs.className = "v";
    vs.textContent = v;
    cellEl.append(ks, vs);
    box.append(cellEl);
  }
}

function renderDevices(d) {
  const tbody = $("#devices tbody");
  tbody.replaceChildren();
  $("#device-count").textContent = `(${d.devices.length})`;

  for (const dev of d.devices) {
    const tr = document.createElement("tr");
    const name = cell(dev.name);
    if (dev.update_available) {
      const up = document.createElement("span");
      up.className = "dim";
      up.textContent = " ⬆";
      up.title = "update available";
      name.append(up);
    }
    tr.append(
      name,
      cell(dev.model, "dim"),
      cell(dev.state, stateClass(dev.state)),
      cell(dev.has_stats ? fmtDuration(dev.uptime_s) : "—"),
      cell(dev.has_stats ? `${dev.cpu_pct.toFixed(1)} %` : "—"),
      cell(dev.has_stats ? `${dev.memory_pct.toFixed(1)} %` : "—"),
      cell(dev.has_stats ? `↓${fmtRate(dev.rx_bps)} ↑${fmtRate(dev.tx_bps)}` : "—", "dim"),
      cell(dev.uplink_mac || "—", "dim"),
    );
    tbody.append(tr);
  }
}

function renderClients(d) {
  const tbody = $("#clients tbody");
  tbody.replaceChildren();
  $("#client-count").textContent = `(${d.clients.length})`;
  $("#clients-empty").hidden = d.clients.length > 0;

  for (const c of d.clients) {
    const tr = document.createElement("tr");
    tr.append(
      cell(c.name),
      cell(c.type, "dim"),
      cell(c.home ? t("home") : `${t("away")} (${fmtDuration(c.away_for_s)})`,
        c.home ? "state-online" : "dim"),
      cell(c.ip || c.mac || "—", "dim"),
      cell(c.network ? `${c.network}${c.vlan ? ` · VLAN ${c.vlan}` : ""}` : "—", "dim"),
      cell(c.signal_dbm ? `${c.signal_dbm} dBm` : c.ssid || "—", "dim"),
    );
    tbody.append(tr);
  }
}

function renderWLANs(d) {
  const section = $("#wlan-section");
  if (!d.wlans || d.wlans.length === 0) { section.hidden = true; return; }
  section.hidden = false;

  const box = $("#wlans");
  box.replaceChildren();
  for (const w of d.wlans) {
    box.append(chip(w.name, w.enabled ? "on" : "off", w.enabled ? "" : "stale"));
  }
}

function render(d) {
  if (d.language && d.language !== lang && I18N[d.language]) {
    lang = d.language;
    document.documentElement.lang = lang;
    applyStaticLabels();
  }
  renderBadges(d);
  renderAlerts(d);
  renderLoops(d);
  renderHealth(d);
  renderDevices(d);
  renderClients(d);
  renderWLANs(d);
  $("#updated").textContent = `${t("updated")}: ${new Date().toLocaleTimeString()}`;
}

async function tick() {
  try {
    const resp = await fetch("api/state", { cache: "no-store" });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    render(await resp.json());
  } catch (err) {
    // The daemon being gone is exactly what someone opens this page to
    // find out, so say it plainly instead of leaving stale numbers up.
    const mqtt = $("#mqtt-badge");
    mqtt.textContent = t("offline");
    mqtt.className = "badge bad";
    $("#updated").textContent = String(err);
  }
}

applyStaticLabels();
tick();
setInterval(tick, REFRESH_MS);
