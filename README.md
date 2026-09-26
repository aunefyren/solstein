# Solstein

[![CI](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/go.yml?branch=main&style=for-the-badge&label=CI)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![Coverage](https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/aunefyren/28cb38a6289c7b2b21694175a243e7eb/raw/solstein-coverage.json&style=for-the-badge)](https://github.com/aunefyren/solstein/actions/workflows/go.yml)
[![CodeQL](https://img.shields.io/github/actions/workflow/status/aunefyren/solstein/codeql-analysis.yml?branch=main&style=for-the-badge&label=CodeQL)](https://github.com/aunefyren/solstein/actions/workflows/codeql-analysis.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/aunefyren/solstein?style=for-the-badge)](https://go.dev/dl/)

A self-hosted podcast RSS proxy that sits between podcast hosts and Audiobookshelf, and does two things for you:

**1. Nothing reaches the podcast industry from your address.** Feeds and episodes are fetched through WireGuard tunnels Solstein runs inside itself — no `NET_ADMIN`, no `/dev/net/tun`, no gluetun sidecar. Podcast hosts, CDNs and the measurement services chained in front of every episode (Podtrac, Chartable, Podscribe, Claritas…) see a VPN address, never yours. DNS goes through the tunnel too, `HTTP_PROXY` is ignored, and with a VPN configured your own connection is switched off as a route by default: if the VPN is down, requests fail rather than leak. The only thing leaving your address is the WireGuard handshake to the VPN itself.

**2. The ads that are inserted per listener are removed.** Solstein downloads each episode through two exits in different ad markets and keeps only the audio both have. The show is left byte for byte as published: what you keep is your own download's own MP3 frames, cut at frame boundaries, never re-encoded or transcoded. (On a host that re-encodes every copy, the two are lined up by their loudness over time to find the ads — but the file you get is still your download, untouched where it is kept.) The feed carries the real length and duration of what you get. It works both on hosts that splice ads into the file (Acast, PRX) and on hosts that re-encode the whole episode around them (RedCircle).

**You need one VPN key to run both.** More keys make it faster, and [How many VPN keys](#how-many-vpn-keys) says exactly what each one buys.

The name comes from the Viking sunstone (Iceland spar), which shows everything twice through double refraction.

> **Status:** early development, but the parts above are built and checked against real hosts and a real client. Ad removal has run end to end with Audiobookshelf on Acast, PRX and RedCircle shows — including a backlog of 30 episodes in one evening, cutting ads at frame level and, on the re-encoding host, by audio. VPN exits work with any WireGuard VPN through `.conf` files, and with Proton VPN from its server list. What is measured, and what is only reasoned, is stated where it is claimed; what is unfinished is in [`docs/wip.md`](docs/wip.md), and how it all works in [`docs/`](docs/README.md).

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

## The one-key setup: private, ad-free, from scratch

Everything below assumes Audiobookshelf as the client and one WireGuard key from a VPN that has servers in your own country and at least one other. The whole point of this setup is that **nothing waits on a deadline**, so one key is enough.

**1. `docker-compose.yml`:**

```yaml
services:
  solstein:
    image: ghcr.io/aunefyren/solstein:latest
    env_file: .env            # holds PROTON_KEY, see below
    environment:
      SOLSTEIN_EXTERNAL_URL: http://solstein:8080
      SOLSTEIN_TIMEZONE: Europe/Oslo
      PUID: 1000
      PGID: 1000
    volumes:
      - ./solstein:/app/config
    ports:
      # Only so you can reach the API from the host to add feeds; Audiobookshelf
      # reaches Solstein by service name on the compose network either way.
      - 8080:8080
    restart: unless-stopped

  audiobookshelf:
    image: ghcr.io/advplyr/audiobookshelf:latest
    environment:
      # Audiobookshelf refuses private addresses unless the host is allowed.
      SSRF_REQUEST_FILTER_WHITELIST: solstein
    volumes:
      - ./abs/config:/config
      - ./abs/metadata:/metadata
      - ./abs/podcasts:/podcasts
    ports:
      - 13378:80
    restart: unless-stopped
```

Nothing else is needed on the Audiobookshelf side — in particular **not** `PODCAST_DOWNLOAD_TIMEOUT`, because with this setup no episode is ever published before it is ready, so Audiobookshelf never waits for one.

**2. `.env` next to it**, holding the key (only the `PrivateKey` line from a WireGuard config; Proton generates them under Downloads → WireGuard configuration):

```
PROTON_KEY=cH5...=
```

**3. Start once** (`docker compose up -d`) to let Solstein write its `config.json` into `./solstein/`, then stop it and fill in the parts that can't be guessed:

```jsonc
{
  "external_url": "http://solstein:8080",
  "default_exit": "mine",          // feed polls and the VPN server list go out here too
  "prepare_ahead": true,           // nothing is published before it is ready
  "vpn": {
    "providers": {
      "proton": { "type": "protonvpn", "private_keys": ["env:PROTON_KEY"] }
    },
    "exits": {
      "mine":   { "provider": "proton", "locations": ["NO"], "strict": true },
      "abroad": { "provider": "proton", "locations": ["DE"], "strict": true }
    }
  },
  "region_diff": {
    "enabled": true,
    "exits": ["mine", "abroad"],   // your country first: its copy is the one you keep
    "fallback_exits": [],          // see below — leave empty on one key
    "pair_downloads": "auto"       // becomes "in turn" by itself on one key
  }
}
```

Solstein already knows both exits' countries from the VPN's server list, so it refuses a pair that would compare two exits in the same country — there is nothing to declare. (`home_country` exists only to describe your *own* connection, for setups that use `direct` as one side of the pair; here it is off and the setting does nothing.) Your own connection needs no switching off either: `direct_exit` is `auto`, which turns it off as soon as `config.json` has VPN exits. Start Solstein again and it will say so:

```
The direct exit is off (direct_exit: off): nothing goes out on this host's own connection except the VPN tunnels themselves.
Region diff on: comparing mine (home) with abroad; the two downloads are made in turn, one tunnel at a time (pair_downloads auto, and the exits can't have a tunnel each)
Preparing ahead: every episode is downloaded (and cleaned) before any client asks for it, and appears in its feed only once ready.
```

**4. Add a feed** and give Audiobookshelf the URL it returns (`feed_url`), which keeps your token out of the client:

```
curl -H "Authorization: Bearer <auth_token>" -H "Content-Type: application/json" \
     -d '{"source_url": "https://feeds.example.com/your-show"}' \
     http://localhost:8080/api/v1/feeds
```

`auth_token` is in `config.json`. In Audiobookshelf: **Add Podcast → RSS feed URL**, paste `feed_url`, and don't switch on auto-download until the backlog has been cleaned.

**What to expect on one key.** Episodes are cleaned one at a time, in the background, oldest first, and each appears in the feed the moment it is ready — so the feed fills up gradually rather than all at once. Each episode is downloaded twice in turn (about twice the wall-clock time of a setup that fetches both at once), and the single key has to move between the two servers, which can leave the new tunnel unsettled for a minute or two. Expect **a few minutes per episode** on a large backlog, running quietly for as long as it takes. Nothing times out, because nothing is waiting. (That estimate is doubled from runs measured with the pair fetched together; a one-key setup hasn't been timed yet — see [`docs/wip.md`](docs/wip.md).)

Two things to know:
- **Feed polls share the one tunnel.** While a batch runs, a poll may wait and then fail; the last good copy of the feed keeps being served and the next poll picks it up. Adding a *new* feed while a batch runs can fail too (it fetches the source within 20 s) — add feeds when it is idle, or just try again.
- **`fallback_exits` costs a key move each.** With one key, leave it empty. The trade-off: when your two markets happen to carry the *same* ads, Solstein can't tell that from "this show has no dynamic ads", and the episode is kept as it is. A third market would settle it — which is what the second and third key buy.

## How many VPN keys

One key runs everything; each extra one removes a bottleneck. A tunnel belongs to a server, and every episode going out through the same exit shares it, so what counts is **how many exits can be in use at the same moment** — not how many episodes you have. Solstein works this out at start-up and tells you what is missing.

| Keys | What runs | What it buys |
|---|---|---|
| **1** | The pair, downloaded in turn | Privacy and ad removal, with `prepare_ahead`. Slowest: two downloads back to back per episode, plus the key moving between servers. No fallback market. |
| **2** | The pair, downloaded **at the same moment** | About half the time per episode, and the two copies are fetched from the same instant, so only the region differs. The key stops moving between servers, so no settling. |
| **3** | The pair plus one fallback market | "No dynamic ads found" can be confirmed in a third market instead of assumed. Measured on 80-minute episodes (65–220 MB) with the pair together and one fallback: **23–71 s each**, start to cached, one episode at a time. |
| **4–5** | The pair, two fallbacks, and the default exit | Nothing is ever evicted and no key ever moves between servers, and **two episodes are prepared at once** instead of one. |

**Adding a key later changes almost nothing in the configuration.** Put it in the list:

```jsonc
"private_keys": ["env:PROTON_KEY", "env:PROTON_KEY_2"]
```

and Solstein uses it: `pair_downloads: auto` switches from "in turn" to "at the same moment" on its own, and says so at start-up. From the third key on, add the fallback markets you now have room for (`"fallback_exits": ["us"]`), and the start-up warning tells you if you have asked for more exits than keys:

```
VPN: provider 'proton' can hold 3 tunnels at once (one per private key), but 4 of its exits can be in
use at the same moment: … Add 1 more private key so each exit has its own tunnel …
```

With two or more keys, on-demand preparation becomes practical as well, so a client can be pointed at a backlog and download it without `prepare_ahead` — see [Operating styles](#operating-styles). Keeping `prepare_ahead` on is still the quieter choice: nothing then depends on how patient your client is.

Proton's plan limits apply (Free 1 simultaneous connection, Plus 10), and the home exit has to be a country your plan actually has servers in.

## Privacy: what leaves your machine

Who sees what, once the setup above is running:

| Party | Sees |
|---|---|
| Podcast host, CDN | A VPN address in the exit's country, a fixed `Solstein/<version>` User-Agent, and the episode request. Never your address, and no cookies — Solstein sends none and passes none back. |
| Measurement services in front of episodes (Podtrac, Chartable, Podscribe, Claritas, arttrk…) | The same VPN address. They are followed by default, so the show still counts your download; `skip_tracking_redirects: true` cuts them out entirely, which is faster and shows them nothing, at the cost of the show's numbers. |
| Your VPN provider | That you connect, and the tunnelled traffic, as with any VPN. This is the trade you are making: the industry sees a VPN, the VPN sees you. |
| Your own connection | Only WireGuard handshakes to the VPN servers, which necessarily leave from your address. Everything else — feed polls, episode downloads, DNS, the VPN server-list refresh — goes through a tunnel. |
| Your podcast client | Solstein, over your own network. Audiobookshelf talking to `http://solstein:8080` never touches the internet. |

How that is held up, rather than merely intended:
- **No silent fallback.** With a VPN configured, `direct_exit` is `auto`, which switches your own connection off as a route. If the tunnel is down, requests **fail**; if `default_exit` names an exit that didn't load, Solstein **refuses to start**. Leaking is treated as worse than not running.
- **DNS through the tunnel**, using the VPN's own resolver, so names aren't looked up by your host's resolver. (A fallback resolver over TCP *through the same tunnel* is used when the VPN's own resolver fails on a name — still inside the tunnel.)
- **`HTTP_PROXY`/`HTTPS_PROXY` are ignored**, so a proxy in the environment can't quietly carry traffic out another way.
- **Private addresses are refused** on every outbound request, redirects included, checked per IP actually dialled — so a feed can't point Solstein at your router.
- **Solstein itself isn't an open proxy:** the subscribe token is needed for every feed, every URL it writes out is HMAC-signed, and `allowed_client_networks` can limit who may reach it at all. Logs record paths only — never query strings, so tokens and signatures stay out of them.

What this does **not** do: hide anything from your client's own services (Audiobookshelf's metadata lookups go out on your connection, not Solstein's), hide you from the VPN provider, or make a public Solstein anonymous — a URL you share is a URL someone can fetch.

The details are in [`docs/security.md`](docs/security.md).

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
| `POST /api/v1/feeds` | Subscribe: `{"source_url", "exit", "delivery_mode", "poll_interval_minutes", "region_diff", "region_diff_exits", "region_diff_on_failure", "region_diff_trim_break_markers", "region_diff_compare_by_audio"}`; `201` when new, `200` when already subscribed |
| `GET /api/v1/feeds/{id}` | One feed |
| `PATCH /api/v1/feeds/{id}` | Change any of the settings above; an empty value clears the override |
| `DELETE /api/v1/feeds/{id}` | Unsubscribe and delete everything stored for the feed |
| `POST /api/v1/feeds/{id}/retry` | Try the feed's failed episodes again in the background (withheld, or published with ads), e.g. after a fix; `{"queued": n}` |
| `POST /api/v1/retry` | The same for every feed; `{"queued": n}` in total |
| `POST /api/v1/feeds/{id}/prepare` | Download (or clean) the feed's episodes that have no file yet — backlog, or expired from the cache — in the background, before anyone plays them; `{"newest": n}` limits it to the newest `n`. `{"queued": n}` |

The API takes the token as `Authorization: Bearer <token>` or `?token=<token>`.

The full HTTP API — every route, parameter, response and status code, including the feed and episode URLs clients use — is described in OpenAPI 3.1 format in [`docs/openapi.yaml`](docs/openapi.yaml). Open it in any OpenAPI viewer (Swagger Editor, Redocly, or your editor's OpenAPI preview), or generate a client from it.

### Audiobookshelf on the same Docker network

Audiobookshelf refuses to fetch from private addresses by default, so a Solstein reached as `http://solstein:8080` is blocked until you allow it on the Audiobookshelf container:

```yaml
    environment:
      SSRF_REQUEST_FILTER_WHITELIST: solstein
      # Milliseconds, not seconds; 30000 by default. Only needed with a
      # raised processing_wait_seconds, see Operating styles.
      PODCAST_DOWNLOAD_TIMEOUT: 600000
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

**Keeping your own address out of it.** This is the default once you set up a VPN: `direct_exit` is `auto`, which switches the direct exit off as soon as `config.json` has VPN exits, so nothing goes out on your own connection. Feeds without an exit of their own, and the server-list refresh, take `default_exit`, or when that is empty, the first VPN exit by name. A feed or setting naming `direct` is refused. To use `direct` alongside a VPN anyway, set `direct_exit: on`; `off` keeps it off even without a VPN. Solstein never falls back to `direct` when the VPN is down; requests fail instead, and if the VPN exits in `config.json` fail to load (or `default_exit` names one that doesn't exist), Solstein doesn't start. Only the WireGuard connections to the VPN servers themselves leave from your address, as they must.

## Removing ads (region diff)

Hosts such as Acast insert ads per listener region. Region diff downloads each episode through two exits in different ad markets at the same moment, keeps the audio both share and drops the rest: the show stays byte for byte as published, without re-encoding. It works both with hosts that splice ads in (Acast, PRX) and with hosts that re-encode the whole episode around them (RedCircle), which it compares by audio instead. It needs two exits, typically one in your own country and one abroad (see **VPN exits**), and works on MP3 episodes.

```json
"region_diff": {
  "enabled": true,
  "exits": ["norway", "sweden"],
  "fallback_exits": ["germany"],
  "on_failure": "publish",
  "backlog": 0,
  "pair_downloads": "auto",
  "trim_break_markers": false,
  "compare_by_audio": true,
  "keep_failed_downloads": false,
  "keep_successful_downloads": false
}
```

- `exits`: the pair, your home region first (its download is the one kept, with its tags). `direct` works as the home side (set `home_country` so Solstein can check it isn't paired with an exit in the same country), but shows the host your own address, and needs `direct_exit: on` once you have VPN exits; otherwise use a VPN exit in your own country.
- `fallback_exits`: tried in turn when the pair's downloads are identical (no dynamic ads, or the same campaign in both markets). If every one agrees, the episode is kept as it is.
- `enabled`: region diff for every feed that doesn't set its own `region_diff`. With it `false`, feeds can still switch it on one by one.
- `on_failure`: `publish` serves an episode that can't be cleaned (not MP3, implausible result) with its ads; `hide` keeps it out of the feed, and tries it again after 1 hour, after 6 hours, then daily for about a week (Solstein says so at start-up). After that it stays out until `POST /api/v1/feeds/{id}/retry` or a change of settings.
- `backlog`: how many of a new feed's newest existing episodes are cleaned straight away. The rest are cleaned the first time they are played: the client waits a few seconds (about 5 for a 40-minute episode). If it takes over 20 seconds, it gets `503` and `Retry-After`, and the next attempt gets the clean file. To clean a feed's older episodes ahead instead, use `POST /api/v1/feeds/{id}/prepare`.
- `pair_downloads`: whether an episode's two downloads are made `together` (a tunnel for each exit at the same moment) or `in_turn` (one after the other, so **one VPN key is enough**). `auto`, the default, downloads them together when the exits can have a tunnel each and in turn when they can't; Solstein says which at start-up. In turn takes about twice as long per episode, plus any time a VPN key needs to settle after moving to another server, so it belongs with `prepare_ahead` — on request such an episode rarely makes the 20 second wait. The diff doesn't mind the gap: it keeps what the two downloads share, and a show's audio doesn't change between them.
- `trim_break_markers`: also remove the short chime or sting a host splices in around ad breaks, where it can be cut without a glitch (it has to repeat identically, be under 5 seconds and sit at a break). Off by default, since a show's own sting at breaks would go too.
- `compare_by_audio`: on by default. Some hosts (RedCircle) re-encode the whole episode when they insert ads, so the two downloads share no MP3 frame; Solstein then compares their loudness over time instead, read from the compressed audio (a few seconds of CPU per episode, a couple of hundred MB of memory while it runs), and still cuts only the home download's own frames, without re-encoding. Off, such episodes fail as they did before (`on_failure` applies).
- `min_shared_seconds` (default `2`) and `max_removed_share` (default `0.3`) tune the diff and its sanity check.
- An episode that can't be cleaned on request (ABS gets `503`) is tried again on each request, and after three failed attempts `on_failure` applies. A download that looks cut off is fetched again before the diff.
- `keep_failed_downloads`: keep both downloads of an episode whose diff failed, with a note, in `regiondiff-failures/` in the config directory for 14 days, to see what the host sent. Off by default; episodes are large.
- `keep_successful_downloads`: the same for every diff that worked, in `regiondiff-successes/`, with a note of where each ad was cut: to look into a bad cut. Off by default; it costs twice each episode's size for 14 days.
- The two exits must come out in different countries: two in the same one get the same ads. Solstein checks this for VPN exits, and for `direct` when you set `home_country`: at start-up it stays off if both can only be in one and the same country, and before each download it skips an exit that has fallen back to the home exit's country.
- Changing a setting that affects the result (exits, diff settings, `trim_break_markers`, switching region diff on or off, a feed's exit or delivery mode) clears the cached episodes made with the old settings; they are prepared again the next time they are played.
- New episodes appear in the feed once cleaned, whatever the feed's `delivery_mode`, and the feed carries the cleaned file's size and duration.
- Per feed (API): `region_diff` (`on`, `off`, or empty for the global setting), `region_diff_exits` (its own pair), `region_diff_on_failure`, `region_diff_trim_break_markers` and `region_diff_compare_by_audio`. The feed's `region_diff_in_use` shows the result. `pair_downloads` is global only: it follows the keys, not the feed.
- If the exits don't exist, region diff stays off and Solstein logs why at start-up.
- **Upgrading to this version:** region diff's algorithm changed (version 4: comparing by audio, silent frames at cuts), so at the first start every cleaned episode's cached file is cleared and the episode cleaned again the next time it is played or prepared, and episodes that failed before are tried again. Audiobookshelf keeps the copies it has already downloaded, so this is no mass re-download; `POST /api/v1/feeds/{id}/prepare` cleans a feed's episodes ahead if you want them cached again.

## Operating styles

The guide above sets up one particular style. Solstein can be run in quite different ways, though, and what each needs — VPN keys, patience, the kind of client — differs. Two settings pick the style, and [How many VPN keys](#how-many-vpn-keys) says what the keys change:

| Style | Settings | VPN keys | Suits |
|---|---|---|---|
| **Plain proxy** | region diff off | none | Any client, including ones that stream every play |
| **Region diff, on demand** | the defaults | one per exit in use at once | Any client, if you have the keys |
| **Region diff, prepared ahead** | `prepare_ahead: true` | one per exit in use at once | A client that downloads each episode once, such as Audiobookshelf |
| **Region diff on one key** | `prepare_ahead: true`, and `region_diff.pair_downloads` left at `auto` (or set to `in_turn`) | one | The same, when you have one key and can wait — [the guide above](#the-one-key-setup-private-ad-free-from-scratch) |

**On demand** (the default) prepares a new episode when the feed is polled and publishes it once ready, and prepares a backlog episode — or one whose cached copy expired — **when a client asks for it**, in about 5–20 s. Clients don't wait long: Solstein answers `503` with `Retry-After` after 20 s and carries on in the background, and Audiobookshelf gives each episode only two attempts before moving on. That is **40 seconds per episode**, whatever the episode costs: 15 backlog episodes of a RedCircle show took 22–95 s each to clean (about a minute on average), and **1 of 15** arrived on the first pass. The ways round it are below, in order of how little they ask of you: prepare ahead, `POST /api/v1/feeds/{id}/prepare` before selecting anything (the downloads then come from the cache in under a second), or raise the wait so the client sits through it.

**Or let the client wait it out.** `processing_wait_seconds` (default 20) is how long Solstein holds a request before answering `503`; it is 20 because Audiobookshelf allows a download 30 seconds. Raise both — `processing_wait_seconds: 300` in Solstein and `PODCAST_DOWNLOAD_TIMEOUT=600000` on the Audiobookshelf container (**milliseconds**, 30000 by default: set it in seconds by mistake and every download fails in under a second) — and the client waits while the episode is cleaned instead of being sent away, so a bulk download need not be prepared first. Audiobookshelf downloads one episode at a time anyway, so a held request costs it nothing. Raising Solstein's wait *without* the client's timeout makes things worse, not better: the client gives up on its own and never sees the `503` it could have retried, so Solstein warns at start-up when the wait is over 25 seconds.

**Prepared ahead** (`prepare_ahead: true`) removes that race: every episode is downloaded and cleaned before any client asks, and **appears in its feed only once it is ready** — the backlog included, queued as soon as the feed is subscribed to (or at the next start-up for feeds you already have). Nothing then waits on a deadline, so it doesn't matter how long an episode takes. Per feed with the API's `prepare_ahead` (`on`, `off`, or empty to follow the global setting); `prepare_ahead_in_use` shows the result. Episodes are still prepared on request when one is asked for anyway — a copy that expired from the cache — so a streaming client keeps working.

**What the client does decides how much that matters.** Audiobookshelf downloads each episode once and plays from its own disk, so it never comes back for an expired copy: preparing ahead covers it completely. A client that streams from Solstein on every play asks again whenever the 14-day cache has let an episode go, so it wants enough keys to clean within the deadline.

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
| `direct_exit` | `-directexit` | `SOLSTEIN_DIRECT_EXIT` | `auto` | Whether this host's own connection may be used: `auto` (off as soon as VPN exits are set up), `on` or `off`. Replaces `disable_direct`, which is migrated (`true` becomes `off`, `false` `auto`); `-disabledirect` and `SOLSTEIN_DISABLE_DIRECT` still work. |
| `home_country` | `-homecountry` | `SOLSTEIN_HOME_COUNTRY` | `""` (unknown) | Country code this host's own connection comes out in, e.g. `NO`. Lets region diff notice when `direct` is paired with an exit in the same country. |
| `skip_tracking_redirects` | `-skiptrackingredirects` | `SOLSTEIN_SKIP_TRACKING_REDIRECTS` | `false` | Fetch episodes from the audio host directly, skipping the tracking redirects (Podtrac, Chartable, …) chained in front of their URLs. Faster, and the trackers don't see your exits, but the shows' download counts don't see you either. A tracking redirect that fails is skipped either way. |
| `delivery_mode` | `-deliverymode` | `SOLSTEIN_DELIVERY_MODE` | `cache` | Default for feeds: `cache` (download and serve from disk), `stream` (pass through live) or `original` (only proxy the feed). |
| `poll_interval_minutes` | `-pollinterval` | `SOLSTEIN_POLL_INTERVAL` | `15` | Minutes between feed polls. |
| `cache_retention_days` | `-cacheretention` | `SOLSTEIN_CACHE_RETENTION` | `14` | Days cached episodes are kept on disk. Expired episodes stay in the feed and are fetched from the source again if played. |
| `processing_wait_seconds` | `-processingwait` | `SOLSTEIN_PROCESSING_WAIT` | `20` | Seconds a client is held while an episode it asked for is being prepared, before it gets `503` and `Retry-After` (the work carries on either way). The default fits inside Audiobookshelf's 30-second download timeout. Raise it to let a client wait out a whole diff — and raise the client's own timeout to match (`PODCAST_DOWNLOAD_TIMEOUT` on Audiobookshelf), or the request fails on its side instead. At most 3600. |
| `prepare_ahead` | `-prepareahead` | `SOLSTEIN_PREPARE_AHEAD` | `false` | Prepare every episode — download, or clean — before any client asks for it, and let it appear in the feed only once ready. Off by default: episodes are prepared when a feed is polled, and a backlog episode when it is first played. See **Operating styles**. |
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
