#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
#
# Capture UniFi Network Integration API responses as test fixtures.
#
# The decoders in internal/unifi/integration are anchored to golden JSON
# under internal/unifi/integration/testdata/. Those files ship
# hand-written against the published OpenAPI schema; this script
# replaces them with real output from your own console, which is the
# stronger anchor.
#
# Everything identifying is redacted on the way out — MAC addresses, IP
# addresses, hostnames, SSIDs, serial numbers and UUIDs are replaced
# with synthetic values from the documentation ranges (RFC 5737 /
# RFC 7042). The mapping is consistent within a run, so relationships
# survive: a device's uplinkDeviceId still points at the right device,
# and a client's IP still falls inside its network's subnet. Without
# that property the fixtures would decode fine but exercise none of the
# resolution logic.
#
# Usage:
#     UNIFI_HOST=192.168.1.1 UNIFI_API_KEY=... script/capture-fixtures.sh
#
# Optional:
#     UNIFI_PORT=443            console HTTPS port
#     UNIFI_SITE=default        site to capture
#     OUT_DIR=...               where to write (defaults to the testdata dir)
#     KEEP_RAW=1                also keep the unredacted responses in a
#                               temp dir and print its path — for local
#                               debugging only, NEVER commit those.

set -euo pipefail

HOST="${UNIFI_HOST:-}"
API_KEY="${UNIFI_API_KEY:-}"
PORT="${UNIFI_PORT:-443}"
SITE_NAME="${UNIFI_SITE:-default}"
OUT_DIR="${OUT_DIR:-internal/unifi/integration/testdata}"

die() { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }
info() { printf '\033[36m==>\033[0m %s\n' "$*"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }

[ -n "$HOST" ] || die "UNIFI_HOST is required"
[ -n "$API_KEY" ] || die "UNIFI_API_KEY is required"
command -v curl >/dev/null || die "curl not found"
command -v python3 >/dev/null || die "python3 not found (needed for redaction)"

BASE="https://${HOST}:${PORT}"
RAW_DIR="$(mktemp -d)"
trap '[ "${KEEP_RAW:-0}" = "1" ] || rm -rf "$RAW_DIR"' EXIT

# fetch <path> <output-name>
# Console certificates are self-signed on the LAN address, hence -k.
# This only ever reads; nothing here can change console state.
fetch() {
	local path="$1" name="$2" code
	code=$(curl -sS -k -o "${RAW_DIR}/${name}" -w '%{http_code}' \
		-H "X-API-KEY: ${API_KEY}" \
		-H 'Accept: application/json' \
		"${BASE}${path}") || die "request failed: ${path}"

	case "$code" in
	200) ok "${name} (${path})" ;;
	401) die "401 Unauthorized — check UNIFI_API_KEY" ;;
	404) printf '  \033[33m! skipped %s — 404 (endpoint absent on this version)\033[0m\n' "$name" >&2
	     rm -f "${RAW_DIR}/${name}"; return 1 ;;
	*)   printf '  \033[33m! skipped %s — HTTP %s\033[0m\n' "$name" "$code" >&2
	     rm -f "${RAW_DIR}/${name}"; return 1 ;;
	esac
}

# --- resolve the API prefix and the site UUID -------------------------------

PREFIX=""
for candidate in /proxy/network/integration /integration; do
	if curl -sSf -k -o /dev/null -H "X-API-KEY: ${API_KEY}" "${BASE}${candidate}/v1/info" 2>/dev/null; then
		PREFIX="$candidate"
		break
	fi
done
[ -n "$PREFIX" ] || die "no Integration API found at ${BASE} (needs UniFi Network 10.5+)"
info "API prefix: ${PREFIX}"

fetch "${PREFIX}/v1/info" info.json
fetch "${PREFIX}/v1/sites" sites.json

APP_VERSION=$(python3 -c "import json;print(json.load(open('${RAW_DIR}/info.json'))['applicationVersion'])")
info "console runs UniFi Network ${APP_VERSION}"

SITE_ID=$(python3 - "$RAW_DIR/sites.json" "$SITE_NAME" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
want = sys.argv[2]
for s in doc.get("data", []):
    if want in (s.get("internalReference"), s.get("name"), s.get("id")):
        print(s["id"]); break
else:
    known = [s.get("internalReference") for s in doc.get("data", [])]
    sys.exit(f"site {want!r} not found; this console has: {known}")
PY
) || die "$SITE_ID"
info "site ${SITE_NAME} -> ${SITE_ID}"

# --- capture ----------------------------------------------------------------

S="${PREFIX}/v1/sites/${SITE_ID}"
fetch "${S}/devices?limit=200" devices.json
fetch "${S}/clients?limit=200" clients.json
fetch "${S}/networks?limit=200" networks.json
fetch "${S}/wifi/broadcasts?limit=200" wlans.json || true

# One device detail + statistics per device kind, so the fixtures cover
# ports, radios and the uplink rather than just the overview shape.
python3 - "$RAW_DIR/devices.json" <<'PY' > "$RAW_DIR/.device-picks" || true
import json, sys
devices = json.load(open(sys.argv[1])).get("data", [])
picked = {}
for d in devices:
    feats = set(d.get("features") or [])
    kind = "ap" if "accessPoint" in feats else "switch" if "switching" in feats else "other"
    picked.setdefault(kind, d["id"])
for kind, did in picked.items():
    print(f"{kind} {did}")
PY

STATS_SRC=""
while read -r kind did; do
	[ -n "${did:-}" ] || continue
	fetch "${S}/devices/${did}" "device_${kind}.json" || continue
	[ -n "$STATS_SRC" ] || STATS_SRC="$did"
done < "$RAW_DIR/.device-picks"
rm -f "$RAW_DIR/.device-picks"

if [ -n "$STATS_SRC" ]; then
	fetch "${S}/devices/${STATS_SRC}/statistics/latest" device_stats.json || true
fi

# The console's own OpenAPI document, so schema drift between Network
# versions is visible without hunting for an archived copy. Not a
# fixture — written next to them for reference and gitignored.
fetch "${PREFIX}/openapi/document.json" "openapi-${APP_VERSION}.json" || true

# --- redact -----------------------------------------------------------------

info "redacting"
mkdir -p "$OUT_DIR"

python3 - "$RAW_DIR" "$OUT_DIR" <<'PY'
"""Replace identifying values with synthetic ones, consistently.

Consistency is the whole point: uplinkDeviceId must keep pointing at the
device it named, and a client's IP must stay inside its network's
subnet, or the fixtures stop exercising the resolution logic they exist
to test.
"""
import ipaddress, json, os, re, sys

raw_dir, out_dir = sys.argv[1], sys.argv[2]

MAC_RE = re.compile(r"^([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$")
UUID_RE = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

# Documentation ranges: RFC 7042 for MACs, RFC 5737 for IPv4.
macs, uuids, names, ssids = {}, {}, {}, {}
# Subnets are remapped as whole blocks so host addresses inside them
# keep their relative position — that is what preserves containment.
subnets = {}

def mac(v):
    if v not in macs:
        macs[v] = "00:00:5E:00:53:%02X" % (len(macs) + 1)
    return macs[v]

def uuid(v):
    if v not in uuids:
        n = len(uuids) + 1
        h = "%08x" % n
        uuids[v] = f"{h}-{n:04d}-4000-8000-{n:012d}"
    return uuids[v]

def name(v, table, prefix):
    if not v:
        return v
    if v not in table:
        table[v] = f"{prefix}-{len(table) + 1}"
    return table[v]

DOC_NETS = ["192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"]

def ip(v):
    try:
        addr = ipaddress.ip_address(v)
    except ValueError:
        return v
    if addr.version != 4:
        return "2001:db8::1"
    # Map each /24 the console uses onto a documentation /24, keeping
    # the host octet so a client stays inside its own network.
    net = str(ipaddress.ip_network(f"{v}/24", strict=False))
    if net not in subnets:
        subnets[net] = DOC_NETS[len(subnets) % len(DOC_NETS)]
    base = ipaddress.ip_network(subnets[net])
    return str(base.network_address + int(addr) % 256)

IP_KEYS = {"ipAddress", "hostIpAddress", "wanIp", "gateway"}
NAME_KEYS = {"name", "hostname", "displayName", "note"}
DROP_KEYS = {"serialNumber", "serial", "configurationId", "x_authkey", "authkey"}

def walk(node, key=None):
    if isinstance(node, dict):
        return {k: walk(v, k) for k, v in node.items()}
    if isinstance(node, list):
        return [walk(v, key) for v in node]
    if isinstance(node, str):
        if key in DROP_KEYS:
            return "redacted"
        if MAC_RE.match(node):
            return mac(node)
        if UUID_RE.match(node):
            return uuid(node)
        if key in IP_KEYS:
            return ip(node)
        if key == "ssid" or (key == "name" and "ssid" in (key or "")):
            return name(node, ssids, "SSID")
        if key in NAME_KEYS:
            return name(node, names, "Device")
        # CIDR strings, e.g. additionalHostIpSubnets entries.
        if "/" in node:
            try:
                n = ipaddress.ip_network(node, strict=False)
                return f"{ip(str(n.network_address))}/{n.prefixlen}"
            except ValueError:
                pass
    return node

for fn in sorted(os.listdir(raw_dir)):
    if not fn.endswith(".json"):
        continue
    src = os.path.join(raw_dir, fn)
    try:
        doc = json.load(open(src))
    except json.JSONDecodeError:
        print(f"  ! {fn} is not JSON, skipped")
        continue
    # The OpenAPI document carries no site data; copy it verbatim.
    out = doc if fn.startswith("openapi-") else walk(doc)
    with open(os.path.join(out_dir, fn), "w") as f:
        json.dump(out, f, indent=2, sort_keys=False)
        f.write("\n")
    print(f"  {fn}")

print(f"\n  redacted {len(macs)} MACs, {len(uuids)} UUIDs, "
      f"{len(names)} names, {len(subnets)} subnets")
PY

info "written to ${OUT_DIR}"
[ "${KEEP_RAW:-0}" = "1" ] && info "unredacted responses kept in ${RAW_DIR} — do NOT commit these"

cat <<'EOF'

Next steps:
  1. Review the diff — confirm nothing identifying survived.
  2. Update testdata/README.md to say the fixtures are now captured
     rather than hand-written, and note the console version.
  3. go test ./internal/unifi/... and fix whatever the real shapes broke.
EOF
