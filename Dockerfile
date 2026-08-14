# SPDX-License-Identifier: MIT
# Multi-stage build for go-unifi2mqtt.
#
# CGO is disabled so the resulting binary is statically linked against
# Go's net package — that means the runtime image can be distroless,
# carrying no shell, no package manager and no userland to attack.
#
# Unlike go-mtec2mqtt there is no operator-editable catalog file: the
# UniFi entity model is derived from the controller's API responses at
# runtime, so the image only carries the binary plus the annotated
# config-template.yaml for reference. The optional web UI's static
# assets are go:embed-ed into the binary.

# ---------- Stage 1: build ----------
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache go.mod / go.sum separately so unrelated source edits don't
# bust the dependency-download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags="-s -w \
        -X github.com/SukramJ/go-unifi2mqtt/internal/version.Version=${VERSION} \
        -X github.com/SukramJ/go-unifi2mqtt/internal/version.Commit=${COMMIT} \
        -X github.com/SukramJ/go-unifi2mqtt/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/unifi2mqtt ./cmd/unifi2mqtt

# ---------- Stage 2: runtime ----------
# The distroless static image ships /etc/ssl/certs/ca-certificates.crt,
# which the UniFi HTTPS client needs whenever the operator points it at
# a console with a publicly trusted certificate. Self-signed consoles —
# the common case on a LAN IP — are handled by the daemon's explicit
# UNIFI_VERIFY_TLS / UNIFI_CA_FILE options instead.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/unifi2mqtt /app/
COPY --from=builder /src/config-template.yaml /app/

# /config is the canonical mount point for the operator's config.yaml.
# XDG_CONFIG_HOME steers config.Locate at the mount so a `docker run -v
# ./my-config:/config:ro` Just Works.
VOLUME ["/config"]
ENV XDG_CONFIG_HOME=/config

# Optional web UI. Off by default; when enabled set WEB_BIND to
# 0.0.0.0:8080 (the 127.0.0.1 default is unreachable from outside the
# container) and publish the port with `docker run -p 8080:8080`.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/unifi2mqtt"]
