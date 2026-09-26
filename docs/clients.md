# Clients

Solstein must work with any client that can subscribe to an RSS URL; nothing may depend on one client's behaviour. Audiobookshelf (ABS) is the primary client, so its behaviour is known in detail and shapes several rules. What other clients do is still open ([`wip.md`](wip.md)).

## Audiobookshelf

Verified against the ABS source at v2.36.1 (commit `d22c468`, 2026-09-23); file references are to that tree.

| Question | Finding | Where |
|---|---|---|
| Custom headers or cookies? | **No.** Feed fetches and episode downloads send fixed headers only (`Accept`, `Accept-Encoding`, an `audiobookshelf (+https://audiobookshelf.org…)` User-Agent). URL tokens are the only practical auth. | `server/utils/podcastUtils.js` `getPodcastFeed`; `server/utils/ffmpegHelpers.js` `downloadPodcastEpisode` |
| Episode matching when the feed URL changes? | An episode counts as present if its **GUID** or its **exact enclosure URL** matches one ABS has. The new-episode check only considers episodes **newer than the newest episode ABS has**, so older episodes are never re-downloaded after a switch. | `server/models/PodcastEpisode.js` `checkMatchesGuidOrEnclosureUrl`; `server/managers/PodcastManager.js` `runEpisodeCheck`, `checkPodcastForNewEpisodes` |
| Follows `itunes:new-feed-url` / redirects? | Follows HTTP redirects but always **stores the URL it was given**; `itunes:new-feed-url` and `atom:link` are read and discarded. | `server/utils/podcastUtils.js` ~131–135, ~400 |
| Picks up an episode that appears late? | **Only if its `pubDate` is newer than a reference point:** the newest episode ABS has, or — for a podcast with nothing downloaded — the time of its previous check, which moves forward every check. Capped at `maxNewEpisodesToDownload` (default 3) per check. | `server/managers/PodcastManager.js` |
| Sends `Range`? Relies on `length`? | No `Range`: one full `GET`, piped through `ffmpeg -c:a copy` (remux and re-tag, no re-encode). `length` is only used for the progress estimate. The saved file's extension comes from the enclosure URL path, falling back to `mp3`. | `server/utils/ffmpegHelpers.js`; `server/objects/PodcastEpisodeDownload.js` |

Also:
- **SSRF filter:** ABS refuses private and loopback addresses for feed fetches and downloads, so a Solstein on the same Docker network or LAN is **blocked by default**. The ABS operator sets `SSRF_REQUEST_FILTER_WHITELIST=solstein` (hostnames, exact match) — narrower than `DISABLE_SSRF_REQUEST_FILTER=1`. (`server/Server.js`)
- **Timeout:** `PODCAST_DOWNLOAD_TIMEOUT`, default 30 s, for feed fetches and episode downloads. Solstein's `processing_wait_seconds` (default 20) is kept inside it; raising one without the other means the request fails on ABS's side instead of coming back as a `503` it can retry ([`episodes.md`](episodes.md)).
- **Failure counting:** a failed or unparseable feed fetch, or a feed with no `<item>`, counts as a failed check; after `MAX_FAILED_EPISODE_CHECKS` (default 24) ABS turns off auto-download for that podcast.
- **URL encoding:** ABS runs `encodeURI` on enclosure URLs that don't look encoded, so Solstein's URLs use only URL-safe characters (base64url signatures).
- **A failed episode download is retried once**, straight away, with a different User-Agent (seen live 2026-09-25). A `503` from Solstein then gets a second chance; after that, ABS doesn't retry on its own. So an episode gets **two attempts about 20 s apart — 40 s in all** — and is then abandoned until someone selects it again: the new-episode check won't come back for it, being older than the newest episode ABS holds. That is the whole reason a backlog bulk download can arrive empty, and what `prepare_ahead` and a raised `processing_wait_seconds` are for ([`episodes.md`](episodes.md)).

### What Solstein does because of it

- **Tokens in the URL** for auth, and signed URLs for everything written out ([`security.md`](security.md)).
- **Publish in order:** a pending episode holds back every newer one, or ABS would skip it for good ([`feeds.md`](feeds.md)).
- **Hide until processed:** ABS downloads once and matches by GUID afterwards, so a processed version swapped in later would never reach it.
- **Served dates are never earlier than the release to clients** (`released_at`): an episode shows dated when it became available through Solstein, usually minutes after the source's date; backlog episodes keep their dates.
- **Always a valid feed with items:** the last good document when the source fails, and never every item hidden.
- **Stay well under 30 s:** the first request for a new prefix URL fetches the source synchronously with a 20 s limit; processing on request waits at most `processing_wait_seconds` (default 20) before answering `503` ([`episodes.md`](episodes.md)). An operator who raises ABS's `PODCAST_DOWNLOAD_TIMEOUT` can raise that wait to match and have ABS sit through a whole diff.
- **Enclosure paths end in the real extension** (`/api/episodes/{feedID}/{episodeID}.mp3?sig=…`).
- `itunes:new-feed-url` / `atom:link` are still rewritten, for other clients. Redirecting the prefix URL to the signed URL wouldn't hide the token from ABS, which stores the URL it was given; the explicit subscribe flow does.
- `stream` mode is safe with ABS, which sends no `Range`.

### Verified live

ABS 2.36.1 and Solstein in Docker on one compose network (2026-09-24), with the Acast feed "Out of Place":
- Without `SSRF_REQUEST_FILTER_WHITELIST`, ABS refused the feed (`Call to 172.22.0.3 is blocked`) and the request never reached Solstein; with `SSRF_REQUEST_FILTER_WHITELIST=solstein` it worked.
- ABS parsed the feed through the prefix URL (27 episodes, every enclosure on Solstein, GUIDs unchanged) and stored the prefix URL, token included, as the feed URL.
- ABS downloaded an episode in 1.6 s; Solstein streamed it from Acast and cached it on the way. ABS kept the GUID and remuxed the file (21,548,392 bytes against Solstein's 21,546,493, from its own tags).
- "Check for new episodes" fetched the feed from Solstein (4 ms from the stored document) and found none. The token showed as `***` in Solstein's log.

**An episode held back until cached** (same day; a controllable feed host, ABS checking every minute, a podcast with nothing downloaded): Solstein found the new episode, its download failed, and the feed correctly hid it. Once the audio was available Solstein cached and published it — but ABS never downloaded it: it compared the episode's date (12:47) with its own previous check (12:49 and later) and skipped it for good. The same happens without holding back, whenever Solstein's poll picks up an episode after an ABS check but dated before it. With the served `pubDate` never earlier than `released_at`, ABS's next check found the episode and downloaded it from the cache in half a second.

**A bulk download of a region-diff backlog needs `prepare` first** (2026-09-26, ABS 2.36.1 and Solstein in Docker, "The Always Sunny Podcast" behind the Podtrac chain, Proton `norway`/`germany` and three keys). 15 backlog episodes were selected in ABS at once:
- **First pass: none arrived.** ABS downloads one episode at a time and gives each two attempts, ~20 s apart, so each episode got 40 s — while cleaning one took 20 s at best and minutes when it had to walk the fallback markets under a tunnel shortage. All 30 attempts got `503` with `Retry-After`, and ABS stopped asking after 9 minutes. Solstein carried on: the jobs it had registered kept running two at a time long after the client had gone, so the requests were not wasted.
- **`POST /api/v1/feeds/{id}/prepare` with `{"newest": 15}`** queued the rest (`{"queued": 7}`), which the background queue cleaned two at a time.
- **Second pass: 15 of 15, in 16 seconds** — 0.6–1.0 s per episode from the cache, no failed attempt, each episode remuxed by ABS with its GUID kept.

Checked again on 2026-09-26 with RedCircle's "Safety Third" (15 backlog episodes, 65–220 MB, about a minute each to clean): **1 of 15** arrived on the first pass — the one episode cleaned in 22 s, inside ABS's two attempts — and after the cache was full a re-queue brought all 15 down in **25 seconds**. A re-queued episode ABS already had is downloaded again and stored twice: the explicit download endpoint doesn't check what is there, so only ask for what is missing.

So for a backlog, `prepare` (or `region_diff.backlog` at subscription) and then the client's bulk download; selecting a backlog in ABS on its own only warms Solstein's cache for the next attempt. ABS's own limit is the cause — two attempts and no queue position it can come back to — so nothing in Solstein can make the first pass succeed.

Region diff through ABS: [`region-diff.md`](region-diff.md).
