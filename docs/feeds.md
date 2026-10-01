# Feeds

The `feeds` package subscribes to source feeds, polls them and renders the feed Solstein serves. The HTTP side is in `server` (`rss.go` for the prefix route, `api.go` for the feed API); parsing and rewriting are in `rss`.

## Subscribing

Two ways to add a feed, both ending in the same feed record:

1. **Prefix URL:** put Solstein in front of an existing feed URL and subscribe to that in the client:
   `https://solstein.example.com/api/rss/{token}/https://feeds.acast.com/public/shows/abc`
   The first request adds the feed; from then on Solstein polls it on its own schedule and the client is served from Solstein's copy. The prefix URL stays the feed's address in the client.
2. **Explicit:** add the feed through the feed API and get back a signed Solstein feed URL to paste into the client. This lets a feed be set up before any client sees it, and keeps the subscribe token out of the client (see [`security.md`](security.md)).

Parsing the embedded source URL:
- Catch-all route; the source URL is rebuilt from the **raw** path plus the raw query string, so the source's own query parameters survive and nothing is decoded twice.
- `https:/host/...` is accepted as well as `https://host/...` (reverse proxies such as nginx collapse `//` by default), and `https://` is assumed when no scheme is given, so `/api/rss/{token}/feeds.acast.com/...` also works.
- The source URL is normalised (scheme and host lower-cased, default port dropped), so the same feed added twice maps to one record.
- `allowed_source_hosts`, when set, limits which hosts can be subscribed to ([`security.md`](security.md)).

What a new subscription does:
- Fetches the source at once, with a 20 s limit (ABS gives up after 30 s), and stores the feed only if it parses as RSS — together with its document and episodes, in one transaction — so a mistyped URL leaves nothing behind.
- Every episode already in the feed is stored as **backlog**: published at once and fetched (or processed) when first requested. Clients don't auto-download old episodes anyway.
- For a feed region diff handles, the newest `region_diff.backlog` backlog episodes (by date; undated count as oldest) are stored as waiting instead, so the pipeline processes them straight away. They stay published while they wait.
- **Preparing ahead** (`prepare_ahead`, or the feed's own setting) stores the **whole** backlog as waiting instead, for a feed in `cache` mode as well as a processed one, and publishes none of it until its file is there (below). Nothing is then prepared because a client asked for it; left published and unprepared, a backlog episode would be hidden and so never asked for. Feeds subscribed before the setting was switched on are queued at the next start-up, and when it is switched on through the API ([`episodes.md`](episodes.md), Background queue).

## Feed records and settings

- Each feed has a stable UUID, independent of its source URL.
- Per-feed settings default to the global ones when empty:

| Setting | Values | Global setting |
|---|---|---|
| `exit` | an exit name | `default_exit` |
| `delivery_mode` | `cache`, `stream`, `original` ([`episodes.md`](episodes.md)) | `delivery_mode` |
| `poll_interval_minutes` | minutes | `poll_interval_minutes` |
| `region_diff` | `on`, `off` | `region_diff.enabled` |
| `region_diff_exits` | two different exits, home region first | `region_diff.exits` |
| `region_diff_on_failure` | `publish`, `hide` | `region_diff.on_failure` |
| `region_diff_trim_break_markers` | `on`, `off` | `region_diff.trim_break_markers` |
| `region_diff_compare_by_audio` | `on`, `off` | `region_diff.compare_by_audio` |
| `prepare_ahead` | `on`, `off` | `prepare_ahead` |

- `feeds.ValidateSettings` refuses an unknown `prepare_ahead` value, an exit that doesn't exist (including `direct` while the direct exit is off, `direct_exit`), an unknown delivery mode, a negative poll interval, `region_diff: on` while region diff isn't running, unknown region-diff values, and a `region_diff_exits` that isn't two different existing exits.
- At start-up, `main.go` warns about feeds whose exit (or region-diff exit) no longer exists, and feeds with region diff switched on while it is off.

## Feed API

A small JSON API behind the subscribe token (`Authorization: Bearer` or `?token=`), for the explicit flow. No web UI. The full specification, with every field and status code, is [`openapi.yaml`](openapi.yaml).

| Route | Does |
|---|---|
| `GET /api/v1/feeds` | List feeds |
| `POST /api/v1/feeds` | Subscribe: `source_url` plus any per-feed settings. `201` when new, `200` (unchanged) when already subscribed |
| `GET /api/v1/feeds/{id}` | One feed |
| `PATCH /api/v1/feeds/{id}` | Change settings; an omitted field is left alone, an empty one clears the override. Cached episodes made with the old settings are cleared at once ([`episodes.md`](episodes.md)), and a feed that now prepares ahead has its episodes without a file queued |
| `DELETE /api/v1/feeds/{id}` | Remove the feed, its episodes and its cached audio |
| `POST /api/v1/feeds/{id}/retry` | Queue the feed's failed episodes for another attempt ([`episodes.md`](episodes.md), Background queue); `{"queued": n}` |
| `POST /api/v1/retry` | The same for every feed whose episodes are prepared; `{"queued": n}` in total |
| `GET /api/v1/feeds/{id}/rules` | The feed's rules, in order ([Rules](#rules)) |
| `PUT /api/v1/feeds/{id}/rules` | Replace them: `{"rules": [...]}`, an empty list removes them. An invalid list is refused (`400`, naming the rule) and the old rules stay |
| `POST /api/v1/feeds/{id}/prepare` | Queue the feed's newest episodes without their file to be prepared ahead; optional body `{"newest": n}`, all when omitted; `{"queued": n}`. `400` for a feed nothing is prepared for (stream or original mode, not processed) |

Responses carry the feed's settings plus `delivery_mode_in_use`, `region_diff_in_use`, `prepare_ahead_in_use` (the global and per-feed settings combined) and `feed_url` (signed). Invalid settings are `400`; internal error text goes to the log, never to the client.

## Polling

- The poller checks every minute which feeds are due (per-feed interval, else `poll_interval_minutes`, default 15) and refreshes them one at a time, so requests aren't burst at hosts.
- Polls go through the feed's exit and are conditional (`If-None-Match` / `If-Modified-Since`).
- **A feed URL behind a tracking prefix** (Podtrac's `rss.pdrl.fm/ce1080/feeds.megaphone.fm/…`, as "Answer for It!" is subscribed): when the request fails at a tracker whose target is in its URL — an error status, an HTML parking page, no response — it goes to that URL directly, logged "Tracking redirect in front of feed '…' failed at pdrl.fm (…); asking feeds.megaphone.fm directly." The same unwrapping as for episodes ([`episodes.md`](episodes.md), Tracking redirects); `skip_tracking_redirects` doesn't apply to feeds, which keep their subscriber counts. An HTML page from the feed host itself (nothing embedded) is still refused as not RSS.
- **A request that gets no response at all** — a connection dropped (seen through a VPN tunnel as a bare EOF), no headers in time — is tried once more on a fresh connection, the same as an episode download ([`episodes.md`](episodes.md)); an unusable exit (no tunnel free, no server) isn't retried, since a new attempt would go the same way. Found missing live (2026-09-27, `prod.log`): 8 poll failures in ~17 hours across three hosts, all a bare `EOF`, each falling straight through to the last good copy with no retry at all.
- New episodes are stored as waiting for the pipeline when the feed is in `cache` mode or processed, and as ready otherwise (`stream`, `original`: nothing to prepare). They wake the pipeline at once.
- A failed poll keeps the last good document (clients keep being served) and records the error on the feed. Clients count failed fetches: ABS disables auto-download after 24.

## Feed rewriting

The served feed is rendered on each request from the last good source document and the episodes' current state, so state changes show up at once. The `rss` package copies the source's **bytes** through and splices in only the changed values, using the decoder's byte offsets: every element and namespace (`itunes:`, `podcast:`, `acast:` …) is kept exactly, and a rewrite with no changes is byte-identical to the input. (Unmarshalling into structs drops what isn't modelled, and `encoding/xml`'s encoder renames namespace prefixes, which breaks ABS: it looks elements up by literal prefix.) Non-UTF-8 feeds (ISO-8859-1/15, Windows-1252) are converted to UTF-8 first.

What changes:
- **Enclosure URL** → a signed Solstein URL with stable IDs, `/api/episodes/{feedID}/{episodeID}.{ext}?sig=…`, ending in the real extension. Every other attribute in the item holding the same audio URL (`<media:content url>`, `<podcast:source uri>`) is rewritten too, so no route to the original audio remains. Left alone in `original` mode.
- Items with no `<enclosure>` but an audio `<media:content>` use that as their audio, as ABS does.
- **`length` and `itunes:duration`** → the cached file's real size, and its real duration when a processor changed it.
- **`itunes:new-feed-url` and `atom:link rel="self"`** → Solstein's signed feed URL, so clients that follow them stay on Solstein.
- **`itunes:episodeType`** → what a `tag` rule sets ([Rules](#rules)): replaced where the item has one, otherwise added after the item's last child, indented like its other children, with the prefix the feed binds the iTunes namespace to (declared on the element itself when the feed doesn't bind it).
- **`guid`** is **never** changed, so a client switched over to Solstein recognises episodes it already has.
- **`pubDate`** of a non-backlog episode → the time Solstein first served it (`released_at`), when that is later than the source's date. Backlog episodes keep their dates. Why: [`clients.md`](clients.md).
- Items not yet published (below) are left out.

Checked against real feeds (NPR, 355 items; Acast, 27 items): only the enclosure URLs, `atom:link rel="self"` and `itunes:new-feed-url` differed from the source; everything else was byte-identical.

## Publish rules

`publishedEpisodes` decides which episodes appear. When episodes need preparing — `cache` mode, or a processor handles the feed:
- An episode appears once it is **ready** (cached or processed).
- **In order:** episodes are published in `pubDate` order, and a pending episode holds back every newer one. ABS only picks up episodes newer than the newest it has, so publishing Tuesday's before Monday's would make it skip Monday's for good.
- **Backlog** episodes are always published; they are fetched or processed on demand. **Preparing ahead** they are not: they wait their turn like any other episode, and hold back newer ones in the same way, since nothing is prepared on demand.
- **Failed** episodes (retries exhausted, or a permanent error) are published and served unprocessed, so one bad episode can't hold a feed back forever.
- **Withheld** episodes (a processor's failure policy `hide`) are left out, backlog included, without holding newer ones back. So are episodes a rule **hides**.
- **Never an empty feed:** if nothing else would be published, the oldest episode is (the oldest neither withheld nor hidden, if any), because ABS treats a feed without items as a failed check.

In `stream` and `original` mode with no processor, everything is published at once.

## Rules

A feed can have rules that hide or tag some of its episodes. A rule matches episodes, and its action says what happens to them. Prompted by NRK's "Lørdagsrådet" (an unofficial feed, `sindrel.github.io/nrk-pod-feeds`), which mixes the Saturday show (2–2.5 h, titled with the guest list `A / B / C`) with short midweek clips (~15 min) and carries no `<itunes:episodeType>` at all. ABS shows full, bonus and trailer episodes from that tag; it doesn't work the type out itself.

- **Stored in the database** (`feed_rules`, one row per rule, deleted with the feed), not `config.json`, and edited as a whole list, through the feed API (`GET`/`PUT /api/v1/feeds/{id}/rules`, [`openapi.yaml`](openapi.yaml)) or the feed page. At most 50 per feed.
- **Matching:** rules are tried in order; the first one an episode matches decides, and a later one never applies. Conditions, all optional, all of which must hold:
  - `title_matches`: a regular expression (Go syntax), case-insensitive, matched anywhere in the title (`^`/`$` to anchor).
  - `min_seconds` / `max_seconds`: bounds on the source's stated duration (`itunes:duration`), inclusive; 0 leaves that side open. An episode without a stated duration never matches a rule with a bound.
  - A rule with no conditions matches everything, so it can close the list as a catch-all.
- **Actions:**
  - `hide`: left out of the served feed, and never prepared in the background: the pipeline doesn't claim it, and neither preparing ahead nor `prepare` queues it, so a hidden episode costs no download (and, with region diff, not two). It doesn't hold newer episodes back. A client asking for it anyway (it had the URL from before, or it is the one a feed of nothing but hidden episodes publishes so as not to be empty) is served as usual.
  - `tag` with `episode_type` `full`, `bonus` or `trailer`: sets `<itunes:episodeType>` in the served feed (Feed rewriting, above). Applied when the feed is served, so it changes nothing stored.
- **Applied to stored episodes too:** saving rules sets each stored episode's `hidden` flag in the same transaction. One no longer hidden is prepared as a new one would be (the workers are woken, and a feed preparing ahead queues it). New episodes get the flag when a poll stores them. The flag is matched against the title and duration stored when the episode was found, and only the rules set it: `UpdateEpisode` never writes it, so a download saving a copy it loaded earlier can't undo a rule change.
- **In the web UI** the feed page edits the rules (Rules, [`web-ui.md`](web-ui.md)), saving through the same code as the API, and shows a hidden episode as **Hidden**.

Example: Lørdagsrådet's clips as bonus episodes, everything else as full ones:
```json
{"rules": [
  {"max_seconds": 3600, "action": "tag", "episode_type": "bonus"},
  {"action": "tag", "episode_type": "full"}
]}
```

Rejected alternative: publishing an episode at once and swapping in the processed file later. ABS downloads once and matches by GUID afterwards, so it would keep the version with ads (the idea is kept for other clients in [`wip.md`](wip.md)).
