# Solstein

[![CI](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/go.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![CodeQL](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/codeql-analysis.yml?branch=main&style=for-the-badge&label=CodeQL)](https://github.com/aunefyren/solstein/actions/workflows/codeql-analysis.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/aunefyren/solstein?style=for-the-badge)](https://go.dev/dl/)

A self-hosted podcast RSS proxy that sits between podcast hosts and Audiobookshelf. Optional modules add regional exits over embedded WireGuard and strip dynamically inserted ads by comparing downloads from two regions.

The name comes from the Viking sunstone (Iceland spar), which shows everything twice through double refraction.

> **Status:** early development. Only the service skeleton exists; feed proxying and the modules are not implemented yet. See `docs/design.md`.

## Running

With Docker:

```yaml
services:
  solstein:
    image: aunefyren/solstein:latest
    environment:
      SOLSTEIN_EXTERNAL_URL: http://solstein:8080
      SOLSTEIN_TIMEZONE: Europe/Oslo
      PUID: 1000
      PGID: 1000
    volumes:
      - ./solstein:/config
    ports:
      - 8080:8080
    restart: unless-stopped
```

No `NET_ADMIN`, `/dev/net/tun` or `network_mode` is needed; tunnels run in-process.

From source (Go 1.26):

```
go run .
```

## Configuration

On first run Solstein creates `config.json` in its config directory (`/config` in Docker); that file is the configuration. Every setting can also be changed with a flag or an environment variable. These are applied at start-up and saved back to `config.json`, so they stay in effect after the flag or variable is removed. If both are given, the flag wins.

| config.json | Flag | Environment variable | Default | Description |
|---|---|---|---|---|
| `port` | `-port` | `SOLSTEIN_PORT` | `8080` | Port Solstein listens on. |
| `external_url` | `-externalurl` | `SOLSTEIN_EXTERNAL_URL` | — | URL Audiobookshelf uses to reach Solstein. |
| `log_level` | `-loglevel` | `SOLSTEIN_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn` or `error`. |
| `timezone` | `-timezone` | `SOLSTEIN_TIMEZONE` | system (`TZ`) | IANA time zone such as `Europe/Oslo`, used for log timestamps and schedules. An unknown name stops start-up. |
| `allow_private_destinations` | `-allowprivatedestinations` | `SOLSTEIN_ALLOW_PRIVATE_DESTINATIONS` | `false` | Allow fetching from private and loopback addresses, e.g. a feed hosted on your LAN. Off by default so Solstein can't be used to reach your internal network. |
| — | `-configdir` | `SOLSTEIN_CONFIG_DIR` | `config` (`/config` in Docker) | Directory for `config.json`, the database, logs and cache. |
| — | `-version` | — | — | Print the version and exit. |

### User and group (Docker)

The container starts as root only to fix ownership of `/config`, then drops to `PUID`:`PGID` (default `1000`:`1000`) before starting Solstein. Set them to the user and group that should own the mounted volume on the host (`id -u` and `id -g`). If you run the container as a non-root user instead (`user: "1000:1000"` in compose), `PUID`/`PGID` are ignored and the volume must already be writable by that user.

`GET /api/health` returns `{"status":"ok","version":"…"}` for health checks.

## Development

See `docs/development.md`.

## Licence

GPL-3.0. See `LICENSE`.
