# Solstein design (working draft)

Planning-phase document. Update as decisions land; keep unresolved items under **Open questions**. Items marked **(proposed)** are the current recommendation, not yet confirmed; **(decided)** items are agreed.

**Status (2026-09-24):** the core proxy is built (all seven steps under **Core build order**). The exits and region-diff modules are still being designed.

## Overview

Solstein is a podcast RSS proxy between podcast hosts (Acast first) and podcast clients. Audiobookshelf (ABS) is the primary client and the one we test against, but nothing may depend on ABS-specific behaviour: any client that can subscribe to an RSS URL should work. The proxy is the core. Features beyond it are modules that can be switched off independently:

| Module | Purpose | Depends on | Off when |
|---|---|---|---|
| Exits (VPN) | Fetch through other regions via embedded WireGuard — any WireGuard VPN, with Proton as a convenience | — | No VPN provider configured |
| Region diff | Strip dynamically inserted ads by comparing downloads from two regions | Exits (at least two distinct exits) | Disabled in config, or fewer than two usable exits |

Design rules:
- The core builds, runs and is useful with every module off.
- The core defines the interfaces (see **Extension points**); modules implement them. The core never imports a module package.
- A disabled module costs nothing at runtime: no tunnels opened, no server list fetched, no double downloads.
- Modules can be enabled globally and overridden per feed.
- Solstein should ideally run on a private network only, but must be safe when exposed to the internet.

## Core: RSS proxy

### Subscribing to a feed

Two ways to add a feed, both ending in the same feed record:

1. **Prefix URL (decided):** put Solstein in front of an existing feed URL and subscribe to that in the client:
   `https://solstein.example.com/api/rss/{token}/https://feeds.acast.com/public/shows/abc`
   The first request adds the feed to Solstein's list; from then on Solstein polls it on its own schedule and the client is served from Solstein's copy. The prefix URL stays the feed's address in the client.
2. **Explicit (decided):** add the feed through the feed API and get back a Solstein feed URL to paste into the client. Lets a feed be set up (and its back catalogue processed) before any client sees it.

Parsing the embedded source URL:
- Catch-all route; rebuild the source URL from the **raw** path plus the raw query string, so the source's own query parameters survive and nothing is decoded twice.
- Accept `https:/host/...` as well as `https://host/...` (reverse proxies such as nginx collapse `//` by default), and assume `https://` when no scheme is given, so `/api/rss/{token}/feeds.acast.com/...` also works.
- Normalise the source URL (scheme and host lower-cased, default port dropped) so the same feed added twice maps to the same feed record.

### Feed records

- Each feed gets a stable feed ID, independent of the source URL. **All database IDs are UUIDs** (decided), assigned on create.
- Feeds are data, not configuration: they live in the database, not `config.json` (decided).
- Per-feed settings: exit, delivery mode, processor (e.g. region diff) and its policy, poll interval. All default to the global settings.

### Polling

- Solstein polls known feeds on a schedule (global interval, **default 15 minutes** (decided), per-feed override), independent of client requests. Client requests are served from the latest successful poll.
- New episodes found during a poll enter the episode pipeline (see **Episode pipeline**).
- If a poll fails, keep serving the last good copy and log a warning. Clients count failed fetches (ABS disables auto-download after 24).

### Feed rewriting

The rewritten feed keeps every element and namespace of the source (`itunes:`, `podcast:`, `acast:` …) and changes only what it must:
- Enclosure `url` → a Solstein episode URL with stable IDs (`/api/episodes/{feedID}/{episodeID}.mp3`), not the source URL embedded. Every other attribute in the item holding the same audio URL (`<media:content url>`, `<podcast:source uri>`) is rewritten too, so no route to the original audio remains. Unchanged in `original` delivery mode.
- Items with no `<enclosure>` but an audio `<media:content>` use that as their audio, as ABS does.
- Enclosure `length` and `itunes:duration` → the real values when Solstein holds the file (cached or processed).
- `itunes:new-feed-url` and `atom:link rel="self"` → Solstein's feed URL. Otherwise a client may follow them straight back to the source.
- Episode `guid` values are **never** changed, so a client whose feed URL is switched over to Solstein can recognise episodes it already has.
- Episodes not yet ready (see **Publish policy**) are left out, but the feed always keeps at least the already-published items: an empty channel is treated as a failed fetch by ABS.
- `pubDate` of a non-backlog episode → the time Solstein first served it (`released_at`), when that is later than the source's date, so clients that only take episodes dated after their last check don't skip it (see **Client compatibility**). Backlog episodes keep their dates.

### Delivery modes

How the core delivers episode audio when no processor handles the episode. Global setting with per-feed override:

| Mode | Behaviour | Pros | Cons |
|---|---|---|---|
| `cache` | Download at poll time through the feed's exit; serve from disk with `http.ServeContent` | Exact `Content-Length`/duration in the feed, full range support, one consistent file however often the client re-requests | Disk usage; download happens even if no client ever plays it |
| `stream` | Fetch from the source through the feed's exit at request time and stream the bytes through, forwarding `Range` | No disk; still routes through the chosen exit | `length` in the feed is the source's claim; with DAI, each (range) request may be stitched differently, so seeking or resuming could corrupt playback in clients that use `Range` (ABS doesn't) — must be verified; source slowness hits the client, and ABS gives up after 30 s |
| `original` | Leave the enclosure URL pointing at the source; only the feed is proxied | Zero cost | Audio bypasses Solstein and its exits; the client's own IP and region pick the ads |

- Default: `cache` (decided). Cached files are deleted after a retention period, **default 14 days** (decided), configurable.
- **Back catalogue (decided):** episodes that already exist when a feed is added are not downloaded in advance. They are fetched on demand when a client requests one: streamed through to the client and written to the cache at the same time. Only episodes that appear after the feed was added are downloaded at poll time. (ABS wouldn't auto-download old episodes anyway; it only takes ones newer than when the podcast was added.)
- A processor always produces a file, so an episode handled by a processor is always served from the cache, whatever the delivery mode. The delivery mode applies to episodes that no processor handles, and to the fallback when processing fails and the policy is to publish anyway.

### Episode pipeline

Each new episode moves through these states, persisted so a restart resumes rather than starting over:

```
discovered → acquiring → processing → ready
                  ↘            ↘
                   failed (retry with back-off, then fallback per policy)
```

- **Acquiring:** with `cache` mode and no processor, one download through the feed's exit. With a processor, the processor does its own acquiring (region diff downloads through two exits).
- **Processing:** skipped when no processor applies.
- **Ready:** the episode is published in the feed and served.
- Work runs in a bounded worker pool, never on the request path.

### Publish policy

Only matters when a processor (e.g. region diff) is enabled for the feed; with no processor, a `stream` or `original` episode is ready as soon as it is discovered, and a `cache` episode once it is downloaded. Whatever delays publishing — processing or just a cache download — episodes of a feed are always published in `pubDate` order, since downloads in a worker pool can finish out of order. Per feed:
- **Hide until processed (proposed default):** the episode isn't in the feed until processing succeeds. The client picks it up on its next refresh. Episodes are published **in order**: a pending episode also holds back every newer episode of the feed, because ABS only picks up episodes newer than the newest one it has (see **Client compatibility**).
- **Publish immediately:** the episode appears right away via the delivery mode (ads included), and is swapped for the processed file when ready. Clients that have already downloaded it keep the unprocessed copy — which for ABS is always, so this option is for other clients only.
- **On failure:** after the retries are exhausted, either publish unprocessed via the delivery mode, or keep hiding it and flag it.

Blocking a client request until processing finishes is ruled out: it contradicts poll-time processing and risks client timeouts.

### Storage (decided)

- SQLite through GORM, using the CGO-free `modernc.org/sqlite` driver, in `solstein.db` in the config directory.
- UUID primary keys on every table, assigned automatically on create.
- Feed records, episode states and cache metadata live here; audio files live in `cache/` in the config directory.

### Feed API (decided)

A small JSON API behind the subscribe token for the explicit subscribe flow: list, add and remove feeds, and change per-feed settings. No web UI in the first version. Built as `/api/v1/feeds` (see README); token as `Authorization: Bearer` or `?token=`.

### Subscribing, as built

- A new subscription fetches the source at once (20 s limit, under ABS's 30 s) and is stored only if it parses as RSS, together with its document and episodes in one transaction, so a mistyped URL leaves nothing behind.
- Every episode present at subscription is stored as **backlog**: published at once, fetched on demand.
- The last good source document is stored per feed (`feed_documents` table) and the served feed is rendered from it on each request, so episode state changes show up immediately.
- Polls are conditional (`If-None-Match` / `If-Modified-Since`); a failed poll keeps the last good document and records the error on the feed.
- Checked against real feeds (NPR, 355 items; Acast, 27 items): only the enclosure URLs, `atom:link rel="self"` and `itunes:new-feed-url` differ from the source; everything else is byte-identical.

### Polling and the pipeline, as built

- The poller checks every minute which feeds are due (per-feed interval, else `poll_interval_minutes`) and refreshes them one at a time, so requests aren't burst at hosts. New episodes wake the pipeline at once.
- Two download workers. Each claims the oldest waiting episode in one database transaction, so no episode is downloaded twice and older episodes go first (matching the publish-in-order rule).
- Downloads go through the feed's exit into `cache/{feedID}/{episodeID}.{ext}` via a `.part` file renamed into place when complete, so a partial file is never served. A download is abandoned after 2 minutes without data, or after 1 hour in total.
- A response that isn't audio (HTML, XML, JSON, text) is refused, so a host's error page served with status 200 is never cached as an episode. A short or empty body is retried.
- Retries after 1 min, 5 min, 15 min, 1 h and 3 h. Permanent failures (4xx other than 408/429, blocked destination, not audio) and the sixth failure mark the episode **failed**: it is then published and streamed from the source, so it can't hold the feed back.
- On start-up, episodes left mid-download are reset and leftover `.part` files removed.
- Checked end to end against Acast's CDN (`sphinx.acast.com`): a new episode was found by the poller, 20.5 MB downloaded in about a second as a valid MP3, and the served feed listed it with its real byte length.

### Serving episodes, as built

`GET`/`HEAD /api/episodes/{feedID}/{episodeID}.{ext}?sig=…`, signature checked over that exact path:
- **Cached:** served from disk with `http.ServeContent` (Range, If-Range, conditional requests, HEAD). A cached file that has gone missing is forgotten and the episode falls back to the source.
- **`original` mode:** `302` to the source.
- **Otherwise streamed** from the source through the feed's exit. `Range` is forwarded; only `Content-Type`, `Content-Length`, `Content-Range`, `Accept-Ranges` and `Last-Modified` are passed back (no cookies or tracking headers). A source error or non-audio response becomes `502` before anything is sent.
- **Tee into the cache:** in cache mode, a full (non-Range) `GET` of an episode the pipeline won't download — backlog, given up on, or ready but uncached — is written to the cache while it streams. The source request then runs on its own context, so if the listener disconnects the download still completes and the next play comes from disk. At most one tee per episode at a time; a second listener meanwhile gets a plain stream. A failed episode that caches this way becomes ready.
- Every download writes to its own uniquely named `.part` file, so the pipeline and a tee can never write into the same file.
- Checked against Acast's CDN: a backlog episode (21.5 MB) streamed and cached in 0.9 s, the second play came from the cache, a Range request returned `206`.

### Housekeeping, as built

- Hourly sweep (and once at start-up):
  - **Retention:** cache copies older than `cache_retention_days` (by `cached_at`) are deleted and the episode's cache fields cleared. The episode stays published; a later play streams it and, in cache mode, caches it again. A file that can't be deleted (e.g. being served, on Windows) is left for the next sweep rather than forgotten.
  - **Strays:** files in the cache no episode refers to — a deleted feed's audio, abandoned `.part` files — are removed, then empty feed directories. Files younger than 70 minutes (the longest a download can run, plus a margin) are left alone, so a download that has finished but not yet been recorded is never removed.
- Deleting a feed through the API removes its cache directory at once.
- **Never an empty feed:** if the publish rules would hide every episode, the oldest one is published anyway and streamed, because ABS treats a feed without items as a failed check (and turns auto-download off after 24).
- The last good source document is kept when a poll fails (step 4).

### Core build order

Each step is testable on its own; usable with ABS after step 6.

1. ✅ Storage: SQLite, feed and episode models.
2. ✅ Outbound: the exit interface and the built-in `direct` exit, with the private-address block, timeouts and a fixed User-Agent.
3. ✅ Feed parsing and rewriting, preserving every element, tested against realistic feed samples.
4. ✅ Subscribing and access: prefix URL route, feed API, token, signed URLs, client network check.
5. ✅ Polling and the episode pipeline: scheduler, download worker pool, publishing in date order.
6. ✅ Serving episodes: from the cache with range support, plus `stream` and `original`.
7. ✅ Housekeeping: cache retention, keeping the last good feed when the source fails.

## Extension points

How modules slot into the core without the core knowing about them (proposed; Go names are illustrative):

- **Exit provider (decided, built in `outbound`).** The core asks `outbound.Manager` for an HTTP client by exit name. The core itself provides `direct`; the exits module registers additional exits (`sweden`, `nordic`, …) through a `Provider`. All outbound traffic — feed polls, cache downloads, streams, processor downloads — goes through this, so outbound safeguards live in one place.

  A module supplies a **dialer**, not a ready-made HTTP client: if modules built their own clients, the core couldn't enforce the private-address block, timeouts, User-Agent or proxy handling on them. The core builds every client on top of the module's dialer.
  ```go
  type Dialer interface {
      LookupIP(ctx context.Context, host string) ([]netip.Addr, error)            // through the tunnel's DNS
      Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error)
  }
  type Provider interface {
      Exits() []string
      Dialer(exit string) (Dialer, error) // wraps ErrExitUnavailable when the tunnel is down
  }
  ```
  The dialer is looked up per connection, so a module can switch servers or restore a tunnel without the core rebuilding clients.
- **Episode processor.** The core hands a processor a job and gets a file back. The processor can download the source through any exit via the job, so region diff needs nothing else from the core.
  ```go
  type Processor interface {
      Name() string
      Process(ctx context.Context, job Job) (Result, error)
  }
  // Job carries the feed, the episode, the source URL, a working directory
  // and a Fetch(ctx, exit) helper that downloads the source through an exit.
  // Result is the output file plus its length, duration and whether it
  // differs from the source.
  ```
- **Registration.** `main.go` builds the enabled modules from config and passes them to the core. v1 allows at most one processor per feed; the interface leaves room for chaining later.
- **Dependencies.** A module that can't run (region diff with fewer than two usable exits) logs a warning and stays off; it doesn't stop start-up (proposed).

## Access and network security

Solstein is ideally reachable only by the client (e.g. ABS and Solstein on the same Docker network, `http://solstein:8080/...`), but must be safe when exposed publicly. Note that ABS blocks private addresses by default; a private-network setup needs `SSRF_REQUEST_FILTER_WHITELIST=solstein` on the ABS side (see **Client compatibility**). Without protection, the prefix URL turns it into an open proxy — through the VPN exits, on the operator's bandwidth — and a way to make it fetch internal addresses (server-side request forgery).

### Tokens in the URL

Clients generally can't be configured to send custom headers or cookies on feed and episode requests (confirmed for ABS), and enclosure downloads carry no credentials at all. So credentials go in the URL, as private podcast feeds (Patreon, Supercast) do. This works with any client.

- **Subscribe token** (decided, on by default): a secret generated on first run and stored in `config.json` (`auth_token` / `SOLSTEIN_AUTH_TOKEN`). Required on `/api/rss/{token}/...` and the feed-management API. Without it, Solstein fetches nothing on anyone's behalf.
- **Signed feed and episode URLs** (decided): every URL Solstein writes out (feed self-links, enclosures) carries an HMAC signature over its path, made with a server-side signing key (also generated and stored). A leaked episode or feed URL opens only that one feed, never Solstein as a whole. Rotating the signing key invalidates all of them, so clients would need re-subscribing.
- Caveat: a client subscribed via the prefix URL stores the subscribe token in its feed URL. Acceptable for a trusted client like ABS; the explicit subscribe flow avoids it by handing out a signed feed URL instead. Redirecting the prefix URL to the signed feed URL does not help with ABS, which follows redirects but stores the original URL.
- Tokens and signatures are redacted in logs (`***`).
- `auth_enabled` defaults to `true`; turning it off is an explicit choice for private-network-only setups, and logs a warning at start-up.

### Network settings (decided)

Inbound:
- `allowed_client_networks` — CIDR list of client addresses allowed to use Solstein at all (e.g. `172.16.0.0/12` for a Docker network, `192.168.1.0/24` for a LAN). Empty allows any address. Checked in addition to tokens, not instead of them. `/api/health` is exempt so container health checks keep working.
- `trusted_proxies` — CIDR list of reverse proxies whose `X-Forwarded-For` is believed, so the client-network check sees the real client address behind a proxy. Empty (the current default) trusts none.
- `external_url` (exists) — the base for every URL Solstein writes out.

Outbound (enforced in the exit layer, so it covers every fetch):
- Only `http` and `https`.
- `allow_private_destinations` (built; `-allowprivatedestinations` / `SOLSTEIN_ALLOW_PRIVATE_DESTINATIONS`) — `false` by default: refuse loopback, private, link-local and similar ranges. Checked on the IP each connection actually dials, not the hostname, so DNS tricks can't get round it.
- `allowed_source_hosts` — optional allowlist of source hosts (e.g. Acast domains). Empty allows any host. Applies to the **feed URLs** that can be subscribed to, not to every fetch: enclosures and redirects legitimately point at other hosts (CDNs, ad servers).

## Module: Exits (VPN)

**Any WireGuard VPN is supported.** Plain WireGuard is the foundation; provider integrations (Proton first) are conveniences on top that save the user from managing configs by hand. Everything downstream of a provider — exits, location matching, server selection, health checks — works the same whatever the provider type.

Configuration lives in `config.json` only (decided): providers and exits are nested and list-shaped, which doesn't fit flags or env vars.

### Secrets

Private keys (and preshared keys) can be written into `config.json` directly, or as a **reference** that Solstein resolves at start-up (decided):

```jsonc
"private_key": "env:PROTON_PRIVATE_KEY"          // read from an environment variable
"private_key": "file:/run/secrets/proton_key"    // read from a file, e.g. a Docker secret
"private_key": "yJ0p…="                          // literal value
```

- The reference is what `config.json` stores and what is written back; the resolved key is never persisted, logged or returned by any API. This keeps `config.json` the source of truth while letting the secret live elsewhere.
- A reference that can't be resolved (unset variable, missing file) makes the provider unavailable with a clear error; it does not stop start-up.
- The same mechanism works for any secret field added later.
- VPN secrets do **not** go through the flag/env settings table: that table writes values back to `config.json`, which would put them there in plain text.

Considered and not chosen for now: **encrypting secrets inside `config.json` with a master key from an environment variable.** It protects the file on its own — backups, a copied or shared config — but on a typical host the master key sits in the compose file next to the data volume, so anyone with host access has both, and it is then no stronger than putting the key itself in the environment. It also adds a failure mode (a lost master key makes the config unusable) and complicates write-back. References give the same practical benefit with less machinery. Can be revisited if sharing or backing up `config.json` becomes a real need.

Secrets Solstein generates itself (the subscribe token, the URL signing key) stay in `config.json`, which is `0600`.

### Tunnels

- Embedded userspace WireGuard: `golang.zx2c4.com/wireguard` with `tun/netstack`. No gluetun, no `NET_ADMIN`, no `network_mode` coupling.
- Each tunnel gets its own `http.Transport` built on the netstack `DialContext`; tunnels run concurrently.
- **DNS goes through the tunnel**, using the config's `DNS` server. Otherwise lookups leak to the host's resolver, and a CDN that picks its edge by resolver location could serve the wrong region.
- Tunnels open on demand and close after an idle period (proposed), so an exit that is configured but unused costs nothing. `max_tunnels` per provider caps concurrent tunnels, to respect plan limits on simultaneous connections.
- `direct` is always available as an exit, whether or not this module is on. It is provided by the core.

### Providers: what Solstein may use

A provider is a pool of WireGuard servers plus the credentials to reach them. Two kinds:

**`wireguard` (generic, the base).** Servers defined by standard wg-quick `.conf` files, as virtually every WireGuard VPN (and a self-hosted VPS) can export. One provider can hold one file, a list of files, or a directory of them — many providers offer a bulk download with one `.conf` per server.
- Supported from the `.conf`: `[Interface]` `PrivateKey`, `Address` (IPv4/IPv6, several allowed), `DNS`, `MTU`; `[Peer]` `PublicKey`, `PresharedKey`, `Endpoint`, `AllowedIPs`, `PersistentKeepalive`. Shell hooks (`PreUp`, `PostUp`, …) and `Table` are ignored with a warning — there is no host network to change.
- A `.conf` says nothing about location, so each server's `country` (and optionally `city`) is declared in `config.json`. When none is declared, Solstein tries to infer it from common file-name patterns (e.g. `se-sto-wg-001`, `NO-FREE#12`) and logs what it inferred; **a declaration always wins over inference**. A server with neither can only be used by referring to it by name.

**Server-list providers (conveniences).** Credentials are given once and the servers come from a published list, so the user doesn't manage per-server files.
- **`protonvpn` (first):** one WireGuard private key (from a config generated in the Proton dashboard) works across Proton servers; the interface address defaults to `10.2.0.2/32` and DNS to `10.2.0.1`. Per-server endpoint IP and public key come from the server list.
  - Simultaneous connections per plan (Proton's own pages): **Free 1, Plus 10**.
  - Every Proton WireGuard config carries the same interface address (`10.2.0.2`). On routers this blocks a second tunnel, and guides work around it by editing the address. Solstein isn't affected: each netstack tunnel is its own isolated network stack, so identical addresses don't collide.
  - Unconfirmed: whether Proton accepts the **same key** on two servers at once, and whether that counts as one connection or two. Proton's documentation doesn't say; community reports of several simultaneous Proton tunnels mostly use a separate config (and key) per tunnel. So a provider accepts a **list** of keys, `max_tunnels` defaults to the number of keys, and each concurrent tunnel uses its own key unless the user raises the limit. To be tested once the exits module exists.
- **Server list source:** gluetun's server data, now in its own repository `github.com/qdm12/gluetun-servers` (MIT, updated monthly), one file per provider (`pkg/servers/protonvpn.json`). It is a Go module that embeds the files, so Solstein ships with a built-in snapshot, refreshes from the repository periodically, and keeps the last good copy in the config directory. Keep attribution.
- What the Proton data offers (August 2026 snapshot): 1,048 WireGuard servers in 148 countries, each with country (full English name, not ISO code), city, server name (`NO#46`), hostname, IP and public key, plus `free` (50 servers in 10 countries), `secure_core` (double hop via CH/IS/SE entry servers), `tor`, `stream` and `port_forward` flags. No region field.
- Further providers in gluetun-servers that support WireGuard (e.g. Mullvad, AirVPN, IVPN) can be added as new types behind the same interface; each has its own key and address rules.

Narrowing a provider — from "everything" to "one server":

```jsonc
"vpn": {
  "providers": {
    "proton": {
      "type": "protonvpn",
      "private_keys": ["env:PROTON_KEY_1", "env:PROTON_KEY_2"],
      "tier": "plus",                 // "free" limits the pool to free servers
      "max_tunnels": 2,               // defaults to the number of keys
      "filter": {                     // optional; empty = everything the tier allows
        "countries": ["SE", "DE", "NL"],
        "cities": [],
        "servers": [],                // server names or hostnames, e.g. ["SE#12"]
        "secure_core": "exclude",     // exclude | include | only
        "tor": "exclude"
      }
    },
    "mullvad": {
      "type": "wireguard",
      "config_dir": "/app/config/wireguard/mullvad",
      "servers": {                    // location per .conf file (name without extension)
        "se-sto-wg-001": { "country": "SE", "city": "Stockholm" },
        "de-fra-wg-002": { "country": "DE", "city": "Frankfurt" }
      }
    },
    "vps": {
      "type": "wireguard",
      "config_file": "/app/config/wireguard/vps.conf",
      "country": "DE"
    }
  }
}
```

### Exits: how Solstein uses providers

An exit is a named route that feeds and region diff refer to. It picks servers from one provider by location:

```jsonc
"exits": {
  "sweden":  { "provider": "proton", "locations": ["SE"], "strict": true },
  "nordic":  { "provider": "proton", "locations": ["SE", "DK", "area:northern-europe"], "exclude": ["NO"] },
  "germany": { "provider": "mullvad", "locations": ["DE", "AT", "CH"], "selection": "sticky" },
  "europe":  { "provider": "proton", "locations": ["continent:europe"], "exclude": ["NO"] },
  "vps":     { "provider": "vps" }
}
```

- **`locations`** is an ordered preference list. Entries: country (`SE`), city (`SE/Stockholm`), server (`server:SE#12`), area (`area:northern-europe`, UN M49 sub-regions) or continent (`continent:europe`). Solstein uses the first entry with a healthy server. Omitted means anywhere the provider allows. Country codes are ISO 3166-1 alpha-2; Solstein maps them to the server list's country names and to areas with built-in tables.
- **Strict vs loose.** `strict: true` uses only the first entry: if nothing there is healthy, the exit is unavailable and the feed's failure policy applies — right when the location is the point, as for region diff. Loose (default) works down the list and degrades gracefully.
- **`exclude`** always wins (typically the home country, whose ads you already get via `direct`).
- **`selection`** within the matching pool: `sticky` (proposed default: keep one server until it fails, optionally rotating on a schedule — keeps the ad market stable and avoids reconnecting), `random`, or `least-failed`.
- **Health:** a tunnel is healthy while its WireGuard handshake is recent and a test request succeeds. A failing server is benched for a while and the next candidate tried.

Where exits are used:
- **Plain proxy:** every feed has an `exit` (default `direct`) for its polls and downloads. That gets another region's ads, or reaches geo-blocked feeds, without the diff module.
- **Region diff:** a pair of exits, e.g. `["direct", "sweden"]`, as global default with per-feed override. From Norway, `direct` plus one VPN exit is a valid pair that needs only a single tunnel. Start-up warns if the two sides can resolve to the same country (e.g. `direct` plus a loose exit that can fall back to `NO`), since that diff would find nothing.

### Exits build order (proposed)

Each step testable on its own; the core already routes every request through `outbound.Manager`, and feeds already have an `exit` setting, so exits become usable as soon as step 4 lands.

1. **Config and secrets:** the `vpn.providers` / `exits` blocks in `config.json`, validation, and `env:` / `file:` secret references.
2. **Generic WireGuard:** parse wg-quick `.conf` files; one netstack tunnel per server implementing `outbound.Dialer` (DNS through the tunnel); open on demand, close when idle, `max_tunnels`.
3. **Exit resolution:** location matching (country, city, server, area, continent) with built-in ISO 3166 and UN M49 tables; strict/loose, `exclude`, `selection`; health and benching of failing servers.
4. **Wiring:** providers registered with `outbound.Manager`; exits selectable per feed and through the feed API; a module that can't run logs a warning and stays off.
5. **Proton provider:** gluetun-servers data (embedded snapshot, periodic refresh, last good copy in the config directory), `tier` and `filter`.
6. **Live checks with a real key:** whether one Proton key holds two tunnels at once, and what country an exit IP geolocates to.

### Exits decisions to confirm

- **Tunnel lifecycle:** open on first use, close after 5 minutes idle (proposed).
- **Server selection default:** `sticky` (proposed).
- **Health checking:** a recent WireGuard handshake plus failures seen on real requests, with no extra test requests to a third-party site (proposed; avoids an external dependency and extra traffic).
- **Unusable module:** log a warning and stay off rather than refuse to start (proposed).
- **Server list source and refresh:** the gluetun-servers repository's `pkg/servers/protonvpn.json` on its default branch, fetched daily, falling back to the embedded snapshot (proposed).
- **Deferred to after v1:** file-name location inference for `.conf` files, the optional geolocation check, providers beyond Proton.

For the live checks the maintainer supplies Proton WireGuard key(s), passed as `env:` references so they never enter the repository.

## Module: Region diff

- An episode processor (see **Extension points**). Downloads each episode through two exits (configurable default pair, per-feed override). With exits off, only `direct` exists, so this module cannot run.
- Compare the files to find differing segments.
- First investigate whether Acast stitches MP3 frames without re-encoding. If so, compare at frame level (fast, exact). Fall back to audio-level alignment only if necessary.
- If the two downloads are identical (no ads, or the same global ad in both): _brief was truncated here — to be completed._

## Client compatibility: Audiobookshelf

Verified against the ABS source at v2.36.1 (commit `d22c468`, 2026-09-23). File references are to that tree. Other clients may behave differently; nothing below may become a hard dependency.

| Question | Finding | Where |
|---|---|---|
| Custom headers or cookies? | **No.** Feed fetches and episode downloads send fixed headers only (`Accept`, `Accept-Encoding`, an `audiobookshelf (+https://audiobookshelf.org…)` User-Agent). Nothing per-feed is configurable. URL tokens are the only practical auth. | `server/utils/podcastUtils.js` `getPodcastFeed`; `server/utils/ffmpegHelpers.js` `downloadPodcastEpisode` |
| Episode matching when the feed URL changes? | The feed URL is editable per podcast in the UI. An episode counts as already present if its **GUID** or its **exact enclosure URL** matches one ABS has. More importantly, the new-episode check only considers episodes whose `pubDate` is **newer than the newest episode ABS already has**, so older episodes are never re-downloaded after a switch, whatever their URLs. Unchanged GUIDs make it safe regardless. | `server/models/PodcastEpisode.js` `checkMatchesGuidOrEnclosureUrl`; `server/managers/PodcastManager.js` `runEpisodeCheck`, `checkPodcastForNewEpisodes`; `client/components/widgets/PodcastDetailsEdit.vue` |
| Follows `itunes:new-feed-url` / redirects? | It follows HTTP redirects (axios default) but always **stores the URL it was given**: `getPodcastFeed` overwrites the parsed feed URL with the requested one. `itunes:new-feed-url` and `atom:link` are read but then discarded. | `server/utils/podcastUtils.js` lines ~131–135 and ~400 |
| Picks up an episode that appears late? | **Only if its `pubDate` is newer than a reference point**: the newest episode ABS already has — or, for a podcast with **no episodes downloaded yet**, the time of ABS's previous check, which moves forward every check. An episode held back while a newer one is published is never auto-downloaded; and for a podcast with nothing downloaded, any episode that reaches the feed after an ABS check but is dated before it is skipped too (found in live testing, see below). Also capped at `maxNewEpisodesToDownload` (default 3) per check. | `server/managers/PodcastManager.js` `runEpisodeCheck`, `checkPodcastForNewEpisodes` |
| Sends `Range`? Relies on `length`? | No `Range` — a single full GET, piped through `ffmpeg -c:a copy` (remux and re-tag, no re-encode). Enclosure `length` is used only for the progress estimate. The saved file's extension comes from the enclosure URL path (query string stripped), falling back to `mp3`. | `server/utils/ffmpegHelpers.js`; `server/objects/PodcastEpisodeDownload.js` `urlFileExtension` |

Other behaviour that matters:
- **SSRF filter:** ABS routes feed fetches and episode downloads through `ssrf-req-filter`, which refuses private and loopback addresses. A Solstein on the same Docker network or LAN (`http://solstein:8080`) is therefore **blocked by default**. The ABS operator must set `SSRF_REQUEST_FILTER_WHITELIST=solstein` (comma-separated hostnames, exact match on the URL's hostname) or `DISABLE_SSRF_REQUEST_FILTER=1`. Whitelisting the hostname is the narrower choice. (`server/Server.js`)
- **Timeout:** `PODCAST_DOWNLOAD_TIMEOUT`, default 30 s, applies to feed fetches and episode downloads. Solstein must answer a feed request well within that, including the first request for a new prefix URL, and in `stream` mode must start sending bytes quickly.
- **Failure counting:** a failed or unparseable feed fetch counts as a failed check; after `MAX_FAILED_EPISODE_CHECKS` (default 24) ABS turns off auto-download for that podcast. A feed with no `<item>` elements also counts as a failure.
- **URL encoding:** ABS runs `encodeURI` on enclosure URLs that don't look encoded, so Solstein URLs should use only URL-safe characters (e.g. base64url signatures).

### Verified live (2026-09-24)

ABS 2.36.1 and Solstein in Docker on one compose network, with the real Acast feed `Out of Place`:
- Without `SSRF_REQUEST_FILTER_WHITELIST`, ABS refused the feed (`Call to 172.22.0.3 is blocked`) and the request never reached Solstein. With `SSRF_REQUEST_FILTER_WHITELIST=solstein` it worked.
- ABS parsed the feed through the prefix URL (27 episodes, every enclosure on Solstein, GUIDs unchanged) and stored the prefix URL, token included, as the podcast's feed URL.
- ABS downloaded an episode through Solstein in 1.6 s; Solstein streamed it from Acast and cached it on the way. ABS kept the original GUID and remuxed the file (21,548,392 bytes against Solstein's 21,546,493, from its own tags).
- ABS's "check for new episodes" fetched the feed from Solstein (served in 4 ms from the stored document) and correctly found none. The token showed as `***` in Solstein's log.

**New episode held back until cached (live test, same day).** A controllable feed host on the compose network, ABS auto-download checking every minute, a podcast with nothing downloaded yet:
- Solstein found the new episode; its download failed (source answered 503), so the served feed correctly hid it, and ABS's next check saw only the old episode.
- Once the audio was available, Solstein's retry cached it and published it — but **ABS never downloaded it**: with nothing downloaded, ABS compared the episode's date (12:47) against its own previous check (12:49 and later), found it older, and skipped it for good.
- The same happens without any holding back: an episode Solstein's poll picks up after an ABS check, but dated before that check, is skipped as well. ABS reading the source directly never lags like that.
- **Fix:** the served `pubDate` of a non-backlog episode is never earlier than when Solstein first served it (`released_at`). After the fix, on the same stack, ABS's next check found the episode dated at its release and downloaded it from Solstein's cache in half a second.

### Consequences for the design

1. **Tokens in the URL** are confirmed as the auth mechanism.
2. **Publish in order.** Whenever publishing is delayed (hide until processed, or waiting for a `cache` download), never publish an episode while an *older* episode of the same feed is still pending, or ABS will skip the older one for good. A pending episode holds back everything newer than it until it is published or given up on (at which point the failure policy decides).
3. **"Publish immediately, swap later" doesn't help ABS:** it downloads once, matches by GUID afterwards and never fetches the processed version. For ABS, "hide until processed" is the only way to get processed audio. Keep the option for clients that re-download, but document this.
4. **The redirect idea doesn't hide the subscribe token from ABS** (it stores the original URL). For ABS, use the explicit subscribe flow and paste the signed feed URL if the token shouldn't live in ABS.
5. **Always return a valid feed with items:** serve the last good copy when the source fails, and never hide every item (if nothing is ready, keep the already-published episodes in the feed).
6. **The first request for a new prefix URL** fetches the source synchronously; it must stay well under 30 s, or it fails in ABS and counts towards the 24.
7. **`itunes:new-feed-url` / `atom:link` rewriting** is still right for other clients, even though ABS ignores them.
8. **Enclosure paths end in the real extension** (`/api/episodes/{feedID}/{episodeID}.mp3?sig=…`).
9. **`stream` mode is less risky with ABS** than feared, since ABS sends no `Range`; the concern remains for clients that seek or resume.
10. **Setup documentation** must cover the ABS SSRF whitelist for private-network deployments.
11. **Served dates are never earlier than the release to clients** (see the live test above). An episode shows in the client dated when it became available through Solstein, usually minutes after the source's date; backlog episodes keep their dates.

## Open questions

### Client behaviour
- Behaviour of other clients (e.g. Podcast Addict, AntennaPod, Pocket Casts via a public URL): whether they seek/resume with `Range`, re-download changed enclosures, or honour `itunes:new-feed-url`. Only matters for the `stream` mode default and the "publish immediately" policy.

### Core
- **Cache size cap.** Whether to add a size limit on top of the 14-day retention, and whether to evict an episode early once a client has fetched it completely.

### Region diff and exits
- **Rest of the brief.** The original specification was cut off mid-sentence in the region-diff section; everything after it is still to be supplied.
- **Acast stitching format.** Frame-level splice vs re-encode; whether ID3 tags, Xing/LAME headers or bit-reservoir boundaries differ between downloads. Needs empirical inspection of real downloads from two regions.
- **Other variance sources.** Whether ad selection also depends on User-Agent, cookies, time or random rotation (two downloads from the *same* region may differ); whether host-read/baked-in ads exist that no diff can catch. First data point (2026-09-24): one Acast episode (`Out of Place`, served from `sphinx.acast.com`) downloaded four times from Norway, twice direct and twice through Solstein, gave byte-identical files (same MD5). That show may carry no dynamic ads, so this says nothing yet about DAI shows; test with one that does.
- **Identical-download fallback.** Serve as-is, retry with a third exit, or flag for review.
- **Concurrent tunnels on one Proton key.** Plan limits are confirmed (Free 1, Plus 10), but not whether one key can hold tunnels to two servers at once or how that is counted. Design already handles either answer (list of keys); test empirically with the exits module. Region diff from Norway with `direct` plus one exit needs only one tunnel regardless.
- **Geolocation drift.** What decides the ads is how Acast geolocates the exit IP, not the country in the server list; VPN IPs are sometimes misplaced. Optional check via an IP-geolocation service through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.
- **Server-list resilience.** Behaviour if gluetun-servers changes schema version (its files carry a `version`); refresh interval; falling back to the embedded snapshot.
- **File-name patterns to recognise** for location inference, per provider's bulk-download naming.
