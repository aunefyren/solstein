# Solstein

[![CI](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/go.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![CodeQL](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/codeql-analysis.yml?branch=main&style=for-the-badge&label=CodeQL)](https://github.com/aunefyren/solstein/actions/workflows/codeql-analysis.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/aunefyren/solstein?style=for-the-badge)](https://go.dev/dl/)

A self-hosted podcast RSS proxy that sits between podcast hosts and Audiobookshelf. Optional modules add regional exits over embedded WireGuard and strip dynamically inserted ads by comparing downloads from two regions.

The name comes from the Viking sunstone (Iceland spar), which shows everything twice through double refraction.

> **Status:** early development. Subscribing to and serving rewritten feeds works; serving episode audio, background polling and the VPN and ad-removal modules are not implemented yet. See `docs/design.md`.

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

## Subscribing to a feed

Put Solstein in front of the feed URL you would normally give your podcast app:

```
https://solstein.example.com/api/rss/<auth_token>/https://feeds.acast.com/public/shows/example
```

`auth_token` is in `config.json`, generated on first run. The first request fetches the source feed and subscribes to it; from then on Solstein serves its own copy. `https:/host/...` (as some reverse proxies rewrite it) and a bare `host/...` also work.

Alternatively, add feeds through the API and use the signed `feed_url` it returns, which keeps the token out of your podcast app:

```
curl -H "Authorization: Bearer <auth_token>" -H "Content-Type: application/json" \
     -d '{"source_url": "https://feeds.acast.com/public/shows/example"}' \
     https://solstein.example.com/api/v1/feeds
```

| Method and path | Purpose |
|---|---|
| `GET /api/v1/feeds` | List feeds |
| `POST /api/v1/feeds` | Subscribe: `{"source_url", "exit", "delivery_mode", "poll_interval_minutes"}`; `201` when new, `200` when already subscribed |
| `GET /api/v1/feeds/{id}` | One feed |
| `PATCH /api/v1/feeds/{id}` | Change `exit`, `delivery_mode` or `poll_interval_minutes`; an empty value clears the override |
| `DELETE /api/v1/feeds/{id}` | Unsubscribe and delete everything stored for the feed |

The API takes the token as `Authorization: Bearer <token>` or `?token=<token>`.

### Audiobookshelf on the same Docker network

Audiobookshelf refuses to fetch from private addresses by default, so a Solstein reached as `http://solstein:8080` is blocked until you allow it on the Audiobookshelf container:

```yaml
    environment:
      SSRF_REQUEST_FILTER_WHITELIST: solstein
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
| `disable_auth` | `-disableauth` | `SOLSTEIN_DISABLE_AUTH` | `false` | Turn off the subscribe token and URL signatures. Only for a private network. |
| `auth_token` | `-authtoken` | `SOLSTEIN_AUTH_TOKEN` | generated | Subscribe and API token. At least 16 characters, no `/ ? # % &` or spaces. |
| `url_signing_key` | — | — | generated | Signs the feed and episode URLs Solstein writes out. Changing it breaks every subscribed URL. |
| `allowed_client_networks` | `-allowedclientnetworks` | `SOLSTEIN_ALLOWED_CLIENT_NETWORKS` | `[]` (any) | IPs/CIDRs allowed to use Solstein at all, e.g. `172.16.0.0/12` for a Docker network. Comma-separated for flag and env. |
| `trusted_proxies` | `-trustedproxies` | `SOLSTEIN_TRUSTED_PROXIES` | `[]` (none) | Reverse proxies whose `X-Forwarded-For`/`-Proto`/`-Host` are believed. |
| `allowed_source_hosts` | `-allowedsourcehosts` | `SOLSTEIN_ALLOWED_SOURCE_HOSTS` | `[]` (any) | Hosts feeds may be subscribed from; subdomains included, e.g. `acast.com`. |
| `delivery_mode` | `-deliverymode` | `SOLSTEIN_DELIVERY_MODE` | `cache` | Default for feeds: `cache` (download and serve from disk), `stream` (pass through live) or `original` (only proxy the feed). |
| `poll_interval_minutes` | `-pollinterval` | `SOLSTEIN_POLL_INTERVAL` | `15` | Minutes between feed polls. |
| `cache_retention_days` | `-cacheretention` | `SOLSTEIN_CACHE_RETENTION` | `14` | Days cached episodes are kept. |
| — | `-configdir` | `SOLSTEIN_CONFIG_DIR` | `config` (`/config` in Docker) | Directory for `config.json`, the database, logs and cache. |
| — | `-version` | — | — | Print the version and exit. |

### User and group (Docker)

The container starts as root only to fix ownership of `/config`, then drops to `PUID`:`PGID` (default `1000`:`1000`) before starting Solstein. Set them to the user and group that should own the mounted volume on the host (`id -u` and `id -g`). If you run the container as a non-root user instead (`user: "1000:1000"` in compose), `PUID`/`PGID` are ignored and the volume must already be writable by that user.

`GET /api/health` returns `{"status":"ok","version":"…"}` for health checks.

## Development

See `docs/development.md`.

## Licence

GPL-3.0. See `LICENSE`.
