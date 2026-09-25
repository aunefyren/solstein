# Solstein

[![CI](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/go.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/aunefyren/28cb38a6289c7b2b21694175a243e7eb/raw/solstein-coverage.json&style=for-the-badge)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![CodeQL](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/codeql-analysis.yml?branch=main&style=for-the-badge&label=CodeQL)](https://github.com/aunefyren/solstein/actions/workflows/codeql-analysis.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/aunefyren/solstein?style=for-the-badge)](https://go.dev/dl/)

A self-hosted podcast RSS proxy that sits between podcast hosts and Audiobookshelf. Optional modules add regional exits over embedded WireGuard and strip dynamically inserted ads by comparing downloads from two regions.

The name comes from the Viking sunstone (Iceland spar), which shows everything twice through double refraction.

> **Status:** early development. The core proxy works: subscribing, rewritten feeds, background polling, caching new episodes, serving audio (from the cache with range support, or streamed from the source) and cache clean-up. VPN exits work with any WireGuard VPN through `.conf` files, and with Proton VPN from its server list. Ad removal (region diff) works on Acast shows, checked end to end with Audiobookshelf. How it all works: [`docs/`](docs/README.md); what is planned: [`docs/wip.md`](docs/wip.md).

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
| `POST /api/v1/feeds` | Subscribe: `{"source_url", "exit", "delivery_mode", "poll_interval_minutes", "region_diff", "region_diff_exits", "region_diff_on_failure", "region_diff_trim_break_markers"}`; `201` when new, `200` when already subscribed |
| `GET /api/v1/feeds/{id}` | One feed |
| `PATCH /api/v1/feeds/{id}` | Change any of the settings above; an empty value clears the override |
| `DELETE /api/v1/feeds/{id}` | Unsubscribe and delete everything stored for the feed |
| `POST /api/v1/feeds/{id}/retry` | Try the feed's failed episodes again in the background (withheld, or published with ads), e.g. after a fix; `{"queued": n}` |
| `POST /api/v1/feeds/{id}/prepare` | Download (or clean) the feed's episodes that have no file yet — backlog, or expired from the cache — in the background, before anyone plays them; `{"newest": n}` limits it to the newest `n`. `{"queued": n}` |

The API takes the token as `Authorization: Bearer <token>` or `?token=<token>`.

The full HTTP API — every route, parameter, response and status code, including the feed and episode URLs clients use — is described in OpenAPI 3.1 format in [`docs/openapi.yaml`](docs/openapi.yaml). Open it in any OpenAPI viewer (Swagger Editor, Redocly, or your editor's OpenAPI preview), or generate a client from it.

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

**Keeping your own address out of it.** Set `default_exit` to a VPN exit (for example one in your own country) so every feed goes through it unless it names another, and `disable_direct: true` so nothing ever goes out on your own connection: the server-list refresh takes the default exit too, and a feed or setting naming `direct` is refused. Solstein never falls back to `direct` when the VPN is down; requests fail instead, and a `default_exit` that doesn't exist or failed to load stops start-up. Only the WireGuard connections to the VPN servers themselves leave from your address, as they must.

## Removing ads (region diff)

Hosts such as Acast insert ads per listener region. Region diff downloads each episode through two exits in different ad markets at the same moment, keeps the audio both share and drops the rest: the show stays byte for byte as published, without re-encoding. It needs two exits, typically one in your own country and one abroad (see **VPN exits**), and works on MP3 episodes.

```json
"region_diff": {
  "enabled": true,
  "exits": ["norway", "sweden"],
  "fallback_exits": ["germany"],
  "on_failure": "publish",
  "backlog": 0,
  "trim_break_markers": false,
  "keep_failed_downloads": false
}
```

- `exits`: the pair, your home region first (its download is the one kept, with its tags). `direct` works as the home side, but shows the host your own address; with `disable_direct` use a VPN exit in your own country.
- `fallback_exits`: tried in turn when the pair's downloads are identical (no dynamic ads, or the same campaign in both markets). If every one agrees, the episode is kept as it is.
- `enabled`: region diff for every feed that doesn't set its own `region_diff`. With it `false`, feeds can still switch it on one by one.
- `on_failure`: `publish` serves an episode that can't be cleaned (not MP3, implausible result) with its ads; `hide` keeps it out of the feed, and tries it again after 1 hour, after 6 hours, then daily for about a week (Solstein says so at start-up). After that it stays out until `POST /api/v1/feeds/{id}/retry` or a change of settings.
- `backlog`: how many of a new feed's newest existing episodes are cleaned straight away. The rest are cleaned the first time they are played: the client waits a few seconds (about 5 for a 40-minute episode). If it takes over 20 seconds, it gets `503` and `Retry-After`, and the next attempt gets the clean file. To clean a feed's older episodes ahead instead, use `POST /api/v1/feeds/{id}/prepare`.
- `trim_break_markers`: also remove the short chime or sting a host splices in around ad breaks, where it can be cut without a glitch (it has to repeat identically, be under 5 seconds and sit at a break). Off by default, since a show's own sting at breaks would go too.
- `min_shared_seconds` (default `2`) and `max_removed_share` (default `0.3`) tune the diff and its sanity check.
- An episode that can't be cleaned on request (ABS gets `503`) is tried again on each request, and after three failed attempts `on_failure` applies. A download that looks cut off is fetched again before the diff.
- `keep_failed_downloads`: keep both downloads of an episode whose diff failed, with a note, in `regiondiff-failures/` in the config directory for 14 days, to see what the host sent. Off by default; episodes are large.
- The two exits must come out in different countries: two in the same one get the same ads. Solstein checks this for VPN exits (not for `direct`, whose country it can't know): at start-up it stays off if both can only be in one and the same country, and before each download it skips an exit that has fallen back to the home exit's country.
- Changing a setting that affects the result (exits, diff settings, `trim_break_markers`, switching region diff on or off, a feed's exit or delivery mode) clears the cached episodes made with the old settings; they are prepared again the next time they are played.
- New episodes appear in the feed once cleaned, whatever the feed's `delivery_mode`, and the feed carries the cleaned file's size and duration.
- Per feed (API): `region_diff` (`on`, `off`, or empty for the global setting), `region_diff_exits` (its own pair), `region_diff_on_failure` and `region_diff_trim_break_markers`. The feed's `region_diff_in_use` shows the result.
- If the exits don't exist, region diff stays off and Solstein logs why at start-up.

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
| `default_exit` | `-defaultexit` | `SOLSTEIN_DEFAULT_EXIT` | `""` (direct) | Exit for feeds that don't name one, e.g. a VPN exit. Must exist, or Solstein doesn't start. |
| `disable_direct` | `-disabledirect` | `SOLSTEIN_DISABLE_DIRECT` | `false` | Never use this host's own connection; needs `default_exit`. |
| `delivery_mode` | `-deliverymode` | `SOLSTEIN_DELIVERY_MODE` | `cache` | Default for feeds: `cache` (download and serve from disk), `stream` (pass through live) or `original` (only proxy the feed). |
| `poll_interval_minutes` | `-pollinterval` | `SOLSTEIN_POLL_INTERVAL` | `15` | Minutes between feed polls. |
| `cache_retention_days` | `-cacheretention` | `SOLSTEIN_CACHE_RETENTION` | `14` | Days cached episodes are kept on disk. Expired episodes stay in the feed and are fetched from the source again if played. |
| `region_diff` | — | — | off | Ad removal; see **Removing ads**. Set in `config.json` only. |
| — | `-configdir` | `SOLSTEIN_CONFIG_DIR` | `config` (`/app/config` in Docker) | Directory for `config.json`, the database, logs and cache. |
| — | `-version` | — | — | Print the version and exit. |

### User and group (Docker)

The container starts as root only to fix ownership of `/app/config`, then drops to `PUID`:`PGID` (default `1000`:`1000`) before starting Solstein. Set them to the user and group that should own the mounted volume on the host (`id -u` and `id -g`). If you run the container as a non-root user instead (`user: "1000:1000"` in compose), `PUID`/`PGID` are ignored and the volume must already be writable by that user.

`GET /api/health` returns `{"status":"ok","version":"…"}` for health checks.

## Development

See `docs/development.md`, and [`docs/README.md`](docs/README.md) for how Solstein works.

## Licence

GPL-3.0. See `LICENSE`.
