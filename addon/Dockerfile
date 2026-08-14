# SPDX-License-Identifier: MIT
# Home Assistant add-on Dockerfile (local-build FALLBACK path).
#
# PREFERRED PATH: do not build this file at all. Instead let the Supervisor
# pull the pre-built multi-arch image referenced by `image:` in
# addon/config.yaml (ghcr.io/sukramj/go-unifi2mqtt-addon-{arch}). Those images
# are produced by .github/workflows/addon-image.yml. See addon/README.md for
# the full rationale.
#
# FALLBACK PATH (this file): a self-contained multi-stage build that compiles
# the Go binary itself and lands it on a Home Assistant base image so bashio is
# available to script/run.sh.
#
# IMPORTANT — BUILD CONTEXT ASSUMPTION:
#   The Go sources live at the REPOSITORY ROOT, not inside addon/. The builder
#   stage below therefore assumes the Docker build context is the repository
#   root (so `COPY . .` brings in go.mod, cmd/ and internal/). Home Assistant's
#   local add-on builder normally uses the add-on directory (addon/) as the
#   build context, which would NOT contain the Go sources — so this Dockerfile
#   cannot be built by the stock local-add-on flow. It IS buildable manually
#   with the repo root as context, e.g.:
#
#     docker build \
#       --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:latest \
#       -f addon/Dockerfile -t go-unifi2mqtt-addon .
#
#   For Home Assistant installs, use the PREFERRED GHCR image path instead.

# BUILD_FROM (the per-arch HA base image) is consumed by the runtime stage's
# `FROM ${BUILD_FROM}`. An ARG used in a FROM must be declared *before the first
# FROM* to be a global build arg; declaring it only next to the runtime FROM
# leaves it blank. The Supervisor (and addon-image.yml) supply it per arch.
ARG BUILD_FROM

# ---------- Stage 1: build the Go binary ----------
# Pinned to match the project's Go toolchain (see go.mod / root Dockerfile).
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

# Repo-root build context: brings in cmd/ and internal/.
COPY . .

ARG VERSION=addon
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Static, CGO-free binary so it runs unmodified on the HA base image.
ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags="-s -w \
        -X github.com/SukramJ/go-unifi2mqtt/internal/version.Version=${VERSION} \
        -X github.com/SukramJ/go-unifi2mqtt/internal/version.Commit=${COMMIT} \
        -X github.com/SukramJ/go-unifi2mqtt/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/unifi2mqtt ./cmd/unifi2mqtt

# ---------- Stage 2: Home Assistant add-on runtime ----------
# BUILD_FROM is supplied by the Supervisor from addon/build.yaml.
ARG BUILD_FROM
FROM ${BUILD_FROM}

COPY --from=builder /out/unifi2mqtt /usr/bin/unifi2mqtt

# The daemon needs no writable working directory — every setting arrives
# through UNIFI_* env from run.sh.
WORKDIR /app

# Add-on entrypoint: reads /data/options.json and exec's the daemon.
COPY script/run.sh /run.sh
RUN chmod +x /run.sh

# Diagnostic web UI served through Ingress.
EXPOSE 8080

# Add-on image labels (BUILD_ARCH / BUILD_VERSION are injected by the
# Supervisor build; defaults keep manual builds from failing).
ARG BUILD_ARCH=amd64
ARG BUILD_VERSION=1.0.0
LABEL \
  io.hass.version="${BUILD_VERSION}" \
  io.hass.type="addon" \
  io.hass.arch="${BUILD_ARCH}"

CMD ["/run.sh"]
