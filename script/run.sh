#!/usr/bin/with-contenv bashio
# SPDX-License-Identifier: MIT
# Home Assistant add-on entrypoint for go-unifi2mqtt.
#
# Reads the user's add-on options (/data/options.json) via bashio, maps them
# onto the daemon's UNIFI_* environment variables, wires up Ingress, and
# finally exec's the binary so it becomes PID 1 and receives signals directly.
# The daemon ships no config file in the add-on image — every setting is
# supplied through UNIFI_* env here (main.go falls back to env-only when no
# config.yaml is found).
set -e

bashio::log.info "Starting go-unifi2mqtt add-on..."

# --- UniFi console (Network Integration API) ---
export UNIFI_HOST="$(bashio::config 'unifi_host')"
export UNIFI_PORT="$(bashio::config 'unifi_port')"
export UNIFI_API_KEY="$(bashio::config 'unifi_api_key')"
export UNIFI_SITE="$(bashio::config 'unifi_site')"
export UNIFI_VERIFY_TLS="$(bashio::config 'unifi_verify_tls')"

# --- Classic API fallback (optional; needed for site health, client
#     blocking and WLAN toggles the official API does not expose) ---
export UNIFI_CLASSIC_ENABLE="$(bashio::config 'classic_enable')"
if bashio::config.has_value 'classic_username'; then
  export UNIFI_CLASSIC_USERNAME="$(bashio::config 'classic_username')"
  export UNIFI_CLASSIC_PASSWORD="$(bashio::config 'classic_password')"
fi

# --- MQTT ---
# Zero-config: when mqtt_server is left empty, borrow the broker the
# Supervisor already knows about (the HA MQTT integration / core-mosquitto
# add-on) via the mqtt service. An explicit mqtt_server always wins; if
# nothing is set and no service is offered, fall back to core-mosquitto:1883.
if bashio::config.has_value 'mqtt_server'; then
  export UNIFI_MQTT_SERVER="$(bashio::config 'mqtt_server')"
  export UNIFI_MQTT_PORT="$(bashio::config 'mqtt_port')"
  export UNIFI_MQTT_LOGIN="$(bashio::config 'mqtt_login')"
  export UNIFI_MQTT_PASSWORD="$(bashio::config 'mqtt_password')"
elif bashio::services.available 'mqtt'; then
  bashio::log.info "mqtt_server empty; using the Home Assistant MQTT service."
  export UNIFI_MQTT_SERVER="$(bashio::services 'mqtt' 'host')"
  export UNIFI_MQTT_PORT="$(bashio::services 'mqtt' 'port')"
  export UNIFI_MQTT_LOGIN="$(bashio::services 'mqtt' 'username')"
  export UNIFI_MQTT_PASSWORD="$(bashio::services 'mqtt' 'password')"
else
  bashio::log.warning "mqtt_server empty and no MQTT service offered; falling back to core-mosquitto:1883."
  export UNIFI_MQTT_SERVER="core-mosquitto"
  export UNIFI_MQTT_PORT="1883"
fi
export UNIFI_MQTT_TOPIC="$(bashio::config 'mqtt_topic')"

# --- Home Assistant discovery ---
export UNIFI_HASS_ENABLE="$(bashio::config 'hass_enable')"
export UNIFI_HASS_BASE_TOPIC="$(bashio::config 'hass_base_topic')"

# --- Polling cadences (seconds) ---
export UNIFI_REFRESH_DEVICES="$(bashio::config 'refresh_devices')"
export UNIFI_REFRESH_CLIENTS="$(bashio::config 'refresh_clients')"
export UNIFI_REFRESH_HEALTH="$(bashio::config 'refresh_health')"
export UNIFI_REFRESH_STATIC="$(bashio::config 'refresh_static')"

# --- Client publication + filtering ---
# Off by default in the add-on schema: a busy network can otherwise create
# hundreds of Home Assistant entities on first start. The include/exclude
# lists below narrow it down to the clients an automation actually cares
# about. Empty lists mean "no restriction on this dimension".
export UNIFI_CLIENTS_ENABLE="$(bashio::config 'clients_enable')"
export UNIFI_CLIENTS_TYPES="$(bashio::config 'clients_types | join(",")')"
export UNIFI_CLIENTS_NETWORKS="$(bashio::config 'clients_networks | join(",")')"
export UNIFI_CLIENTS_VLANS="$(bashio::config 'clients_vlans | join(",")')"
export UNIFI_CLIENTS_SSIDS="$(bashio::config 'clients_ssids | join(",")')"
export UNIFI_CLIENTS_INCLUDE_MACS="$(bashio::config 'clients_include_macs | join(",")')"
export UNIFI_CLIENTS_EXCLUDE_MACS="$(bashio::config 'clients_exclude_macs | join(",")')"
export UNIFI_CLIENTS_EXCLUDE_GUESTS="$(bashio::config 'clients_exclude_guests')"
export UNIFI_CLIENTS_MAX="$(bashio::config 'clients_max')"
export UNIFI_CLIENTS_AWAY_TIMEOUT="$(bashio::config 'clients_away_timeout')"
export UNIFI_CLIENTS_SIGNAL_SENSOR="$(bashio::config 'clients_signal_sensor')"

# --- Controls (write-back from Home Assistant) ---
# The master switch gates all of them; each one below then decides
# individually. The last three need the classic layer, and the daemon
# refuses to start if they are on without it.
export UNIFI_CONTROLS_ENABLE="$(bashio::config 'controls_enable')"
export UNIFI_CONTROLS_DEVICE_RESTART="$(bashio::config 'controls_device_restart')"
export UNIFI_CONTROLS_PORT_POWER_CYCLE="$(bashio::config 'controls_port_power_cycle')"
export UNIFI_CONTROLS_GUEST_AUTHORIZE="$(bashio::config 'controls_guest_authorize')"
export UNIFI_CONTROLS_DEVICE_LOCATE="$(bashio::config 'controls_device_locate')"
export UNIFI_CONTROLS_CLIENT_BLOCK="$(bashio::config 'controls_client_block')"
export UNIFI_CONTROLS_WLAN_ENABLE="$(bashio::config 'controls_wlan_enable')"

# --- Misc ---
export UNIFI_LANGUAGE="$(bashio::config 'language')"
export UNIFI_DEBUG="$(bashio::config 'debug')"

# --- Diagnostic web UI / Ingress ---
# Bind to all interfaces on 8080 so the Supervisor's Ingress proxy can reach
# the UI (the daemon's 127.0.0.1 default is unreachable from the proxy). The
# SPA uses relative URLs, so it works behind the ingress path prefix.
export UNIFI_WEB_ENABLE="$(bashio::config 'web_enable')"
export UNIFI_WEB_BIND="0.0.0.0:8080"

bashio::log.info "Configuration prepared; UniFi: ${UNIFI_HOST}:${UNIFI_PORT}, MQTT: ${UNIFI_MQTT_SERVER}:${UNIFI_MQTT_PORT}"
bashio::log.info "Web UI bound to ${UNIFI_WEB_BIND} (served via Ingress)."

# Hand off to the daemon (becomes PID 1).
exec /usr/bin/unifi2mqtt
