# Solstein

[![CI](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/go.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/aunefyren/28cb38a6289c7b2b21694175a243e7eb/raw/solstein-coverage.json&style=for-the-badge)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![CodeQL](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/codeql-analysis.yml?branch=main&style=for-the-badge&label=CodeQL)](https://github.com/aunefyren/solstein/actions/workflows/codeql-analysis.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/aunefyren/solstein?style=for-the-badge)](https://go.dev/dl/)

A self-hosted podcast RSS proxy that sits between podcast hosts and Audiobookshelf. Optional modules add regional exits over embedded WireGuard and strip dynamically inserted ads by comparing downloads from two regions.

The name comes from the Viking sunstone (Iceland spar), which shows everything twice through double refraction.

> **Status:** early development. The core proxy works: subscribing, rewritten feeds, background polling, caching new episodes, serving audio (from the cache with range support, or streamed from the source) and cache clean-up. VPN exits work with any WireGuard VPN through `.conf` files, and with Proton VPN from its server list; ad removal is not implemented yet. See `docs/design.md`.

## Running

With Docker:

```yaml
services:
  solstein:
    image: ghcr.io/aunefyren/solstein:latest
    environment:
      SOLSTEIN_EXTERNAL_URL: http://solstein:8080
      SOLSTEIN_TIMEZONE: Europe/Oslo
      PUID: 1000
      PGID: 1000
    volumes:
      - ./solstein:/app/config
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

## VPN exits

A feed can be fetched through a VPN exit instead of your own connection, for example to get another country's version of a feed. Tunnels run inside Solstein: no `NET_ADMIN`, no `/dev/net/tun`, no gluetun or `network_mode`. Any WireGuard VPN works: export its `.conf` files and point Solstein at them.

In `config.json` (paths are relative to the config directory, `/app/config` in Docker):

```json
"vpn": {
  "providers": {
    "myvpn": {
      "type": "wireguard",
      "config_dir": "wireguard/myvpn",
      "servers": {
        "se-sto-001": { "country": "SE", "city": "Stockholm" },
        "de-fra-002": { "country": "DE", "city": "Frankfurt" }
      }
    },
    "vps": { "type": "wireguard", "config_file": "wireguard/vps.conf", "country": "DE" }
  },
  "exits": {
    "sweden": { "provider": "myvpn", "locations": ["SE"], "strict": true },
    "europe": { "provider": "myvpn", "locations": ["SE", "area:northern-europe", "continent:europe"], "exclude": ["NO"] }
  }
}
```

- **Providers** are pools of servers. `config_file`, `config_files` or `config_dir` (every `*.conf` in it); `servers` gives each file's location by name (file name without `.conf`). `max_tunnels` caps simultaneous tunnels.
- **Proton VPN** needs only WireGuard private keys; the servers come from Proton's published list (via [gluetun-servers](https://github.com/qdm12/gluetun-servers)), built in and refreshed daily:

  ```json
  "proton": {
    "type": "protonvpn",
    "private_keys": ["env:PROTON_KEY_1", "env:PROTON_KEY_2"],
    "filter": { "countries": ["SE", "DE", "NL"] }
  }
  ```

  Generate the keys in Proton's dashboard (Downloads → WireGuard configuration; only the `PrivateKey` line is needed, and one key works on every server). Each open tunnel uses its own key; `max_tunnels` defaults to the number of keys. `tier: "free"` limits the pool to free servers. `filter` narrows it by `countries`, `cities` or `servers` (names like `SE#12` or hostnames); Secure Core and Tor servers are left out unless `secure_core` / `tor` is `include` or `only`.
- **Exits** pick a server from one provider. `locations` is an ordered preference list: a country (`SE`), city (`SE/Stockholm`), server (`server:se-sto-001`), UN M49 area (`area:northern-europe`) or continent (`continent:europe`, plus `north-america` and `south-america`). With `strict`, only the first location is used; otherwise the list is worked down. `exclude` lists countries never to use. `selection` is `sticky` (default), `random` or `least-failed`.
- Use an exit per feed with `"exit": "sweden"` when adding it through the API, or `PATCH` an existing feed. `direct` is always available.
- Tunnels open when first needed and close after 5 minutes unused. A server that stops handshaking is left out for a while (1 minute, growing to 30) and the next one is used.
- DNS goes through the tunnel, using the `DNS` line of the `.conf`.
- Keys can stay out of `config.json`: `private_keys` accepts `env:NAME` (e.g. from Docker's `env_file`) and `file:PATH` (e.g. a Docker secret) as well as the key itself.
- A mistake in one provider or exit only disables that one; Solstein logs why at start-up.

## Configuration

On first run Solstein creates `config.json` in its config directory (`/app/config` in Docker); that file is the configuration. Every setting can also be changed with a flag or an environment variable. These are applied at start-up and saved back to `config.json`, so they stay in effect after the flag or variable is removed. If both are given, the flag wins.

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
| `cache_retention_days` | `-cacheretention` | `SOLSTEIN_CACHE_RETENTION` | `14` | Days cached episodes are kept on disk. Expired episodes stay in the feed and are fetched from the source again if played. |
| — | `-configdir` | `SOLSTEIN_CONFIG_DIR` | `config` (`/app/config` in Docker) | Directory for `config.json`, the database, logs and cache. |
| — | `-version` | — | — | Print the version and exit. |

### User and group (Docker)

The container starts as root only to fix ownership of `/app/config`, then drops to `PUID`:`PGID` (default `1000`:`1000`) before starting Solstein. Set them to the user and group that should own the mounted volume on the host (`id -u` and `id -g`). If you run the container as a non-root user instead (`user: "1000:1000"` in compose), `PUID`/`PGID` are ignored and the volume must already be writable by that user.

`GET /api/health` returns `{"status":"ok","version":"…"}` for health checks.

## Development

See `docs/development.md`.

## Licence

GPL-3.0. See `LICENSE`.
