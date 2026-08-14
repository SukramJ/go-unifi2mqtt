# go-unifi2mqtt — Home Assistant Add-on

This directory packages the [go-unifi2mqtt](https://github.com/SukramJ/go-unifi2mqtt)
daemon as a Home Assistant add-on.

## Installation

1. In Home Assistant go to **Settings → Add-ons → Add-on Store**.
2. Open the ⋮ menu → **Repositories** and add
   `https://github.com/SukramJ/go-unifi2mqtt`.
3. Install **go-unifi2mqtt** from the newly listed repository.
4. Fill in `unifi_host` and `unifi_api_key` (see [DOCS.md](DOCS.md)), then start
   the add-on.

The add-on registers an Ingress panel, so the diagnostic web UI is reachable
from the HA sidebar without exposing a port.

## Two images, two purposes

| Image                                          | Built by                          | Purpose                                                             |
| ---------------------------------------------- | --------------------------------- | ------------------------------------------------------------------- |
| `ghcr.io/sukramj/go-unifi2mqtt-addon-{arch}`   | `.github/workflows/addon-image.yml` | **This add-on.** HA base image + bashio + `script/run.sh`.          |
| `ghcr.io/sukramj/go-unifi2mqtt`                | `.github/workflows/docker-build-push.yml` | Plain distroless daemon image for `docker run` / compose. |

`addon/config.yaml` references the first one via its `image:` key, so the
Supervisor **pulls** a pre-built image instead of compiling Go on your
Home Assistant host. The distroless image is *not* a valid add-on image: it has
no `run.sh` to translate add-on options into `UNIFI_*` environment variables.

## Building locally (fallback)

Only needed when you want the Supervisor to build from source — remove the
`image:` key from `addon/config.yaml` first. Note that `addon/Dockerfile`
expects the **repository root** as build context (the Go sources live there,
not in `addon/`), which the stock local-add-on flow does not provide. A manual
build works:

```sh
docker build \
  --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:latest \
  -f addon/Dockerfile -t go-unifi2mqtt-addon .
```

## Files

| File           | Purpose                                                             |
| -------------- | ------------------------------------------------------------------- |
| `config.yaml`  | Add-on manifest: options, schema, Ingress, version.                 |
| `build.yaml`   | Per-arch base images for the local-build fallback.                  |
| `Dockerfile`   | Local-build fallback (repo root as context).                        |
| `DOCS.md`      | The add-on's Documentation tab — every option explained.            |
| `CHANGELOG.md` | The add-on's Changelog tab. Kept identical to the root `changelog.md`. |
