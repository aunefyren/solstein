# Episodes

The `episodes` package prepares and serves episode audio: the download pipeline, running a processor, the cache, serving, and housekeeping.

## Delivery modes

How audio reaches the client when no processor handles the episode. Global `delivery_mode`, per-feed override:

| Mode | Behaviour | Trade-offs |
|---|---|---|
| `cache` (default) | New episodes are downloaded at poll time through the feed's exit and served from disk | Exact `length` in the feed, full `Range` support, one consistent file however often it is requested; uses disk, and downloads even if nobody plays it |
| `stream` | Fetched from the source through the feed's exit at request time and passed through, `Range` forwarded | No disk; the feed's `length` is the source's claim; with dynamic ads each (range) request may be stitched differently, which could break seeking in clients that use `Range` (ABS doesn't); a slow source hits the client, and ABS gives up after 30 s |
| `original` | The enclosure keeps pointing at the source; only the feed is proxied | Costs nothing; the audio bypasses Solstein and its exits, so the client's own address and region pick the ads |

A processor always produces a file, so an episode of a processed feed is served from the cache whatever the mode; the mode then only applies when processing has failed and the policy publishes it unprocessed.

## Episode states

```
discovered → acquiring → ready
     ↑           ↓
     └─ retry ───┤
                 ↓
               failed (published unprocessed, or withheld)
```

- **discovered:** waiting for a worker, possibly until a retry time (`next_attempt_at`).
- **acquiring:** a worker (or a request) is downloading or processing it.
- **ready:** published and served; `cache_file` is set unless the episode is served by streaming or its copy has expired.
- **failed:** given up on. Published and served unprocessed, unless `withheld`. A withheld episode is still tried again now and then in the background (below).
- Backlog episodes start **ready** without a file (or **discovered** when queued for processing up front).
- `prepared_with` records the settings the episode's file was made with (below).
- `next_attempt_at` on a ready or failed episode means it is queued for the background (below); `late_retries` counts those attempts since it failed.
- A processor's result is recorded on the episode: `process_note` (e.g. "removed 4m15s of ads in 4 breaks, comparing norway with sweden", or "no dynamic ads found") and `cache_seconds` (the processed duration). `source_seconds` holds the source's stated `itunes:duration`.

## The pipeline

- **Two workers.** Each claims the oldest waiting episode (`ClaimNextEpisode`) in one database transaction, so no episode is prepared twice and older episodes go first, matching the publish-in-order rule. Waiting means discovered, retry time reached, and the feed in `cache` mode or handled by the processor.
- Workers wake when a poll finds episodes, and otherwise check every 30 s for retries coming due.
- **Downloads** go through the feed's exit into `cache/{feedID}/{episodeID}.{ext}` via a uniquely named `.part` file, renamed into place once complete, so a partial file is never served and two downloads never share a file. The extension comes from the source URL and content type.
- A download is abandoned after **2 minutes without data** or **1 hour in total**, and capped at 2 GB.
- **Checks:** a response that isn't audio (HTML, XML, JSON, text) is refused, so a host's error page served with status 200 is never cached as an episode. An empty or short body (less than `Content-Length`) is retried.
- **Tracking redirects** (`episodes/source.go`, `feeds/trackers.go`): an episode's URL is often a chain of measurement services, each carrying the next URL in its path (`https://www.podtrac.com/pts/redirect.mp3/pdst.fm/e/…/chrt.fm/track/9DD8D/…/traffic.megaphone.fm/TPC1.mp3`). When one of them fails (an error status, a page that isn't audio, no response), the request goes to the URL it would have redirected to instead, logged as "Tracking redirect at chrt.fm failed (…); asking arttrk.com directly." Found on "The Always Sunny Podcast" (2026-09-26): Chartable, shut down by Spotify, answers every `chrt.fm` link with 404, so all 77 episodes failed although Megaphone still serves them; with the fallback, the chain's 53 MB episode came through in 5.2 s. A path segment counts as the next host when it is a domain name whose last label is letters and not a file extension (`pdst.fm`, `traffic.megaphone.fm`; not `redirect.mp3`, `TPC1.mp3` or `1.0.2`); the prefix's query string goes with it. At most 8 are skipped per request. An audio host's own 404 (nothing embedded in its URL) fails as before, permanently.
- **`skip_tracking_redirects`** (off by default) unwraps every prefix before the request, so episodes come from the audio host directly: fewer round trips (2.2 s against 5.2 s for the same episode), and the trackers never see the exits' addresses. Off by default because shows count their downloads through these services. It applies to downloads, streams and the redirect of `original` mode; the feed's enclosure URLs aren't rewritten.
- **A request that gets no response** — a connection dropped (seen through a VPN tunnel as a bare `EOF`), or no response headers within 30 s — is retried once after 0.5 s within the same attempt (streams too), on a new connection: the client's idle connections are closed first, since every request to a host shares one HTTP/2 connection and the retry mustn't go out on the one that just failed.
- **Retries** after 1 min, 5 min, 15 min, 1 h and 3 h. After a failed attempt, the next asks caches on the way not to answer from a stored copy (`Cache-Control: no-cache`), in case the host's CDN served a broken file; `failed_attempts` counts failures since the last success. Permanent failures (4xx other than 408/429, blocked destination, not audio, unknown exit, a processor's `ErrPermanent`) and the sixth failure mark the episode **failed**.
- **Recovery:** on start-up, episodes left acquiring are reset to discovered and leftover `.part` files removed. On shutdown, work in progress is abandoned and picked up again this way.

Checked against Acast's CDN (`sphinx.acast.com`): a new episode was found by the poller, 20.5 MB downloaded in about a second as a valid MP3, and the served feed listed it with its real byte length.

## Processors in the pipeline

For a feed the processor handles (see [`architecture.md`](architecture.md) for the interface):
- Its new episodes are claimed whatever the feed's delivery mode, and the processor runs instead of the plain download. Its downloads (`Job.Fetch`) go through the same checks as the pipeline's own, into memory, at most 512 MB each.
- The output is cached like a download (`.part`, then renamed), with its duration.
- The episode is published once processed ([`feeds.md`](feeds.md)). A retryable failure holds the feed like a pending download. After the last retry, or at once for a permanent error, the episode is failed: published unprocessed (`HideOnFailure` false), or **withheld**: left out of the feed and answered `404`.

### Processing on request

An episode of a processed feed can be requested before it has its processed file: a backlog episode, one whose processed copy has expired from the cache, or one still waiting for its turn or retry (e.g. published by the never-empty rule). Serving the version with ads would defeat the processor, so:
- The request goes to `Pipeline.Prepare`, which processes the episode in the background under the pipeline's lifetime. The request waits up to **20 s** (`ProcessingWait`), then gets the processed file from the cache, `Range` included.
- **At most two at once** (`RequestWorkers`), besides the two background workers: a client downloading a whole backlog would otherwise start a job for every episode, and comparing by audio takes seconds of CPU and a few hundred MB each. Further episodes wait for a slot, registered already, so a second request for one joins it; the client meanwhile gets `503` and `Retry-After` as usual, and the waiting job runs when a slot frees. For a large backlog, `prepare` (below) is kinder still.
- If that takes longer, the client gets **`503` with `Retry-After: 30`** while processing carries on, and the next request gets the file. This is the one case where a client waits on processing; it is bounded, well inside ABS's 30 s.
- **One job per episode:** workers and on-request work register the episodes they are preparing; a request joins a running job instead of starting another. A waiting episode is claimed in the database first (`ClaimEpisode`, ignoring its retry time), so a worker can't take it too. A worker that has claimed it but not yet registered gives `503`.
- **Outcomes:** success caches the file (the episode stays ready, or becomes ready). A permanent failure applies the failure policy: publish (stream the source; in cache mode the stream is cached, and from then on that version is served) or withhold (`404`). A retryable failure answers `503` and leaves a published episode as it is; the next request tries again, asking for fresh copies. Published episodes get no retry schedule, since clients don't re-request on their own — so after **three failed attempts in a row** the failure policy applies, as if the failure were permanent. Without that limit, an episode that fails the same way every time would never be served. A settings change resets the count.
- **Lifetime:** `Pipeline.Run` returns only after on-request work has stopped, so the database is never closed under it, and refuses new work (`ErrBusy`) once stopping. A cancelled job records nothing.

## Background queue

Episodes that are already published (or withheld) can be queued for an attempt in the background, without changing their state: they stay in the feed as they are meanwhile, and nothing holds newer episodes back. What queues them:
- **Withheld episodes are retried slowly:** after **1 hour**, **6 hours**, then **daily for about a week** (8 attempts), with fresh downloads. Without it, a passing problem at the host could hide an episode for good: a backlog episode requested by ABS is withheld after three failures that can all fall within a minute. After the last attempt it stays withheld until a retry through the API or a change of settings. Episodes withheld before this existed get their first retry at the next start-up.
- **`POST /api/v1/feeds/{id}/retry`** queues every failed episode of the feed, withheld or published unprocessed, e.g. after a fix; `POST /api/v1/retry` does it for every feed. A withheld one that fails again starts the slow retries afresh; one published unprocessed stays as it was.
- **`POST /api/v1/feeds/{id}/prepare`** (optionally `{"newest": n}`) queues the feed's newest episodes that are published without their file — backlog, or expired from the cache — to be downloaded or processed before any client asks. One that fails is left for its next request, counting as one of its three attempts. `region_diff.backlog` does this once, at subscription; this does it for a feed already added, or after its files were cleared.

How it runs:
- The workers take queued episodes only when no new episode is waiting, so new episodes still go out first. Two at a time, which also keeps a large backlog from bursting downloads at the host. Queued at the same time, the newest goes first: there is no publish order to keep, and listeners start from the newest.
- **Claiming** moves the episode's `next_attempt_at` on by 3 hours in the same transaction (`ClaimQueuedEpisode`), so no two workers take it; if the attempt never records an outcome (a crash), it simply comes due again then. Queueing only writes the queueing fields of episodes still in the right state, so it can't overwrite one prepared meanwhile.
- **One job per episode**, as for work on request: a request for a queued episode being prepared joins that job; a worker that finds a request's job already running leaves it to that. While it runs, a stream of the episode isn't written to the cache, and settings checks leave it alone until it is done.
- On success the episode is ready (a withheld one is published; being new to clients, it is dated when first served, so ABS picks it up). A processed file replaces an unprocessed one kept from the failure policy.

## Settings changes

Each episode records `prepared_with`: a description of the settings its file was made with — the processor's recipe (for region diff: the exits, fallbacks, diff settings, `trim_break_markers` and an algorithm version), or `download through <exit>` for a plain download; a failed episode records the settings it failed under and what the failure policy did with it. `Pipeline.Reconcile` compares that with the feed's settings now, at start-up (for every feed, since `config.json` may have changed), right after a feed is changed through the API, and for each episode as it is served:
- **A cached file made with other settings is deleted:** another exit, `delivery_mode` changed, region diff switched on or off, other region-diff settings, a new diff algorithm. The episode stays published; the next request prepares it again the current way — processed on request, re-downloaded while streaming, or just streamed.
- **A failed episode whose settings or failure policy changed gets a fresh start** (its late retries too): withheld or not, it is retried from the first attempt — by the pipeline, or, for backlog, on request.
- Episodes being prepared are left alone; a file they record with the old settings is caught when it is served.
- Episodes from before settings were recorded are taken to match the current ones, except a processed feed's cached file with no processor's note, which wasn't processed: it is cleared, so switching region diff on for a feed cleans its cached episodes too.
- Not recorded, so changing them clears nothing: `poll_interval_minutes`, `cache_retention_days`, `region_diff.backlog`.

## Serving

`GET`/`HEAD /api/episodes/{feedID}/{episodeID}.{ext}?sig=…`, the signature checked over that exact path ([`security.md`](security.md)):
- **Withheld:** `404`.
- **Settings checked:** a cached file made with settings that have since changed is cleared first (above).
- **Cached:** served from disk with `http.ServeContent` (`Range`, `If-Range`, conditional requests, `HEAD`). A cached file that has gone missing is forgotten and the episode handled as uncached.
- **Processed feed, not failed:** processed on request (above).
- **`original` mode:** `302` to the source.
- **Otherwise streamed** from the source through the feed's exit. `Range` is forwarded; only `Content-Type`, `Content-Length`, `Content-Range`, `Accept-Ranges` and `Last-Modified` are passed back (no cookies or tracking headers). A source error or non-audio response becomes `502` before anything is sent.
- **Tee into the cache:** in cache mode, a full (non-`Range`) `GET` of an episode the pipeline won't download is written to the cache while it streams: backlog, failed, or ready but uncached — for a processed feed, failed ones only, so the unprocessed version can never take a processed file's place. The source request then runs on its own context, so the download completes even if the listener leaves, and the next play comes from disk. One tee per episode at a time; a second listener meanwhile gets a plain stream. A failed download cached this way becomes ready; a processed feed's failed episode stays failed, so a change of settings retries it.

Checked against Acast's CDN: a backlog episode (21.5 MB) streamed and cached in 0.9 s, the second play came from the cache, and a `Range` request returned `206`.

## Housekeeping

An hourly sweep, and once at start-up:
- **Retention:** cache copies older than `cache_retention_days` (default 14, by `cached_at`) are deleted and the episode's cache fields cleared. The episode stays published; a later play streams and re-caches it, or, for a processed feed, processes it again. A file that can't be deleted (e.g. being served, on Windows) is left for the next sweep rather than forgotten.
- **Strays:** files no episode refers to — a deleted feed's audio, abandoned `.part` files — are removed, then empty feed directories. Files younger than 70 minutes (the longest a download can run, plus a margin) are left alone, so a finished download not yet recorded is never removed.

Deleting a feed through the API removes its cache directory at once.
