# Docker test harness: Solstein + Audiobookshelf

A reusable live-test rig for exercising Solstein against a real Audiobookshelf
(ABS) instance and real podcast hosts, the way the "Verified live" sections in
`docs/region-diff.md`, `docs/exits.md` and `docs/clients.md` were checked.
Solstein is **built from this checkout** (not pulled from GHCR), so it tests
the code as it stands, uncommitted changes included. This exact one-key setup
is what found and confirmed the fix for the idle-connection bug described in
`docs/region-diff.md`, "A one-key setup, verified live through ABS" (2026-09-27).

Everything under `data/` is gitignored: Solstein's config directory (config,
database, log, cache, region-diff keep-failed/keep-successful downloads) and
ABS's config/metadata/podcasts. Nothing about a specific test run — feed URLs,
what a show turned out to need — belongs in this harness; that goes in
`docs/wip.md` or the relevant document under `docs/`, per `CLAUDE.md`.

## Prerequisites

- `.env` at the repo root with at least `PROTON_KEY_1` (the maintainer's
  Proton test keys; never opened or printed, only referenced by
  `env_file: ../.env`). `docs/development.md` and `docs/exits.md` describe it.
- Docker with Compose v2, and `curl`, `openssl` and `python3` on the host for `bootstrap.sh`.

## Run it

```
./setup.sh                        # first time only: creates ./data, seeds config.json
docker compose up -d --build
./bootstrap.sh                    # first time only: users, library, data/credentials.md
```

**Credentials are in `data/credentials.md`**, written by `bootstrap.sh`
(gitignored with the rest of `data/`; made readable only by you where the
file system has Unix permissions, which a Windows drive under `/mnt/c`
doesn't, so there it is as private as the folder): the Solstein
web UI user `tester` and the Audiobookshelf root user `root`, each with a
random password, and Solstein's API token (`auth_token` in
`data/solstein/config.json`, generated on first start). Nothing in the
harness has a fixed or committed password. `bootstrap.sh` does
Audiobookshelf's setup wizard through its API and creates a **Podcasts**
library on `/podcasts`; it refuses to run twice on the same `data/`.

Solstein: `http://localhost:8080` (web UI at `/ui`). Audiobookshelf:
`http://localhost:13378`.

Lost the credentials file? `rm -rf data`, then the three steps again; or,
keeping everything else, add another Solstein user with
`docker exec st-solstein /app/solstein user add <name>` (prints a one-time
password).

Tear down (keeps `./data`, so the next run picks up where this left off):

```
docker compose down
```

`rm -rf data` to start completely clean (a fresh Solstein token, a fresh ABS,
new credentials from `bootstrap.sh`).

## The shipped config: one key, region diff, on-request processing

`config.template.json` is the one-key setup from the README's
["one-key setup"](../README.md#the-one-key-setup-private-ad-free-from-scratch)
section: a single Proton key (`PROTON_KEY_1`; extra keys in `.env` are simply
not referenced, so the pool genuinely has one), region diff on with the pair
`mine` (`NO`) / `abroad` (`DE`), no fallback exits (a fallback costs a key
move on one key — see the README section above).

**`prepare_ahead` is deliberately `false`** here, unlike the README's advice
for a quiet one-key setup: this harness is for exercising the on-request path
— a client's episode request blocking while Solstein downloads and diffs it,
then either serving it or answering `503` — not for avoiding it. The
timeouts are set for that:

- Solstein: `SOLSTEIN_PROCESSING_WAIT=600` (seconds; `docker-compose.yml`).
- ABS: `PODCAST_DOWNLOAD_TIMEOUT=720000` (**milliseconds** — the setting that
  bit a previous test by being set to `600` meaning seconds instead of
  600000ms, see `docs/clients.md`). Keep it comfortably above
  `SOLSTEIN_PROCESSING_WAIT * 1000`.

One key means `region_diff.pair_downloads: auto` resolves to "in turn": the
two downloads (home, then the other market) happen one after another through
the single tunnel, so an episode can take a few minutes rather than tens of
seconds (`docs/exits.md`, `docs/region-diff.md`). That is what makes this
setup worth raising the timeouts for in the first place; with two or more
keys, drop both back toward the defaults (or switch `prepare_ahead: true` and
stop caring about client timeouts at all, as the main README describes).

To test the quiet, nothing-times-out setup instead, set `prepare_ahead: true`
in `data/solstein/config.json` (or `SOLSTEIN_PREPARE_AHEAD=true`) and use
`POST /api/v1/feeds/{id}/prepare` to clean a backlog in the background before
pointing ABS at it — see `docs/clients.md`, "A bulk download of a region-diff
backlog needs `prepare` first".

## Adding a feed

Solstein first (its token is in `data/solstein/config.json`, `auth_token`):

```
token=$(grep -o '"auth_token": *"[^"]*"' data/solstein/config.json | cut -d'"' -f4)
curl -s -H "Authorization: Bearer $token" -H "Content-Type: application/json" \
     -d '{"source_url": "<the show'"'"'s feed URL>"}' \
     http://localhost:8080/api/v1/feeds | tee /tmp/feed.json
```

The response's `feed_url` is what to give Audiobookshelf — it carries a
signature instead of the raw token. In ABS, signed in as `root`: the
**Podcasts** library (made by `bootstrap.sh`, on `/podcasts`) **→ Add
Podcast → paste `feed_url`**. Don't switch on auto-download until a backlog has been
prepared or cleaned; select episodes explicitly to control how many run
through region diff at once (the point of this harness is usually 10–20).

## The web UI

On in `config.template.json`, at `http://localhost:8080/ui`; sign in as
`tester` with the password in `data/credentials.md`. More users are added on
the console:

```
docker exec st-solstein /app/solstein user add <name>   # prints a one-time password
```

See `docs/web-ui.md` and `docs/sign-in.md`.

## Watching it work

```
docker compose logs -f solstein
```

Start-up says which exit provider has enough tunnels for the configured
exits, and whether region diff had to fall back to "one episode at a time"
or "in turn" downloads. Per-episode lines name the pair, the note ("removed
Xm of ads in N breaks…", "no dynamic ads found…", "…by audio" for hosts that
re-encode), and timings. `data/solstein/regiondiff-failures/` (kept 14 days
with `keep_failed_downloads: true`) holds both downloads of anything that
came out implausible, for a closer look.
