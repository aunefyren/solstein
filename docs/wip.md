# Work in progress

Everything about Solstein that isn't finished: known issues, gaps, open questions, ideas and planned work. The other documents in `docs/` describe only what is built (see [`README.md`](README.md)).

## Rules for this file

- **Add as you go.** An issue, question, idea or trade-off goes here as soon as it is discovered or discussed, even half-formed. Record what is known (measurements, where in the code, options considered) so it can be picked up later.
- **Nothing finished lives here.** When an item is resolved — built, decided, answered, or dropped — remove it from this file, and move what was learned into the document that covers that area (`feeds.md`, `episodes.md`, `region-diff.md`, …): the behaviour, the reason, and any live findings. A dropped idea worth remembering is noted there as "considered and not chosen", with why.
- **Items in progress** say so at the top of the item, with what is done and what is left.
- Group items by area, most pressing first within each.

## Region diff

### Proton keys and stalls: what's left (open, 2026-09-26)

Built: one tunnel per Proton key, key affinity, and resuming broken downloads ([`exits.md`](exits.md), [`episodes.md`](episodes.md)). Left:
- **How long a moved key stays disturbed:** `keyMoveSettle` (3 minutes) is a guess from WireGuard's 180 s session lifetime. Measured 2026-09-27 with the controlled probe (pauses on A of 30 s, 1, 2, 3 and 5 minutes, then 4 minutes of 1-request-a-second probing on B, `docs/exits.md`): failures didn't fall off with a longer pause — 30 s had 1 of 216 requests fail, 1 and 3 minutes had none, **2 minutes had 4 of 194**, and 5 minutes had none again. Noisy and non-monotonic, one run per pause, consistent with the original finding that stalls take turns rather than following a clean threshold; doesn't overturn the 3-minute default (nothing at or above it failed here) but doesn't prove it necessary either (30 s barely failed, 2 minutes did). A larger sample per pause, or several repeats, would be needed to say more.
- **`podkast.nrk.no` (Akamai) cutting requests** (`EOF` within 1–5 s, 4 of 18 through NO#23): measured while production shared the probe's keys, so recheck with clean keys. If it remains, find out whether it's the VPN addresses or our client (User-Agent), and compare with direct. The existing one retry per request covers a single cut.
- **Production** needs fresh keys, one per exit used at the same time (the maintainer is generating them); then watch the NRK feeds process.
- **Probe files** `modules/exits/live_dns_probe_test.go` and `live_stall_probe_test.go` (temporary, `live` tag): delete once the above is settled.

Open question: should a tunnel DNS failure with a fresh handshake count against the server? The stalls turned out to be key conflicts, where switching servers moves the key and makes things worse, so probably not; left open until the above is settled.

### Comparing by audio: what the RedCircle run left open (2026-09-26)

Run live against RedCircle and ABS on 15 "Safety Third" episodes ([`region-diff.md`](region-diff.md)): about a minute each, 805 MB peak for one pair of up to 220 MB downloads, no failure. Left:
- **Why RedCircle leaves Norway alone** is unknown: no advertiser for the market, or no ad server for it at all. If it holds for every RedCircle show, region diff has nothing to do on them and the pair's downloads cost twice the bandwidth for nothing — worth a way to notice a feed that never has ads at home and stop comparing it (and what would then make it start again).
- **A slower server** may push an episode of this size past the 20 s wait more often than the 22 s best case here; with `prepare_ahead` that no longer matters.

### Why five backlog downloads were implausible (open question)

Five of 115 "It Was A Sh*t Show" episodes failed as implausible in production within a minute (2026-09-25, [`region-diff.md`](region-diff.md)); downloaded again, they diff cleanly. Most likely one side got a partial or wrong file during the burst of downloads, but it is unconfirmed. Re-downloaded through ABS 25 minutes later (14:24–14:25, one at a time, both tunnels opened cold), all five were cleaned in 3–12 s each, 3 breaks and about 3 minutes removed per episode, and none of the downloads looked incomplete: consistent with a passing problem on the host's side. Solstein now re-fetches downloads that look cut off, asks for fresh copies after a failure, and applies the failure policy after three failed attempts on request. To settle it: run with `keep_failed_downloads` on, and look at the kept files and note the next time it happens. Then decide whether anything else is needed. Withheld episodes are now retried slowly, and a backlog can be cleaned ahead two at a time with `prepare` instead of through ABS's burst; ABS re-downloading a whole backlog still processes on request, as fast as it asks.

### Ads that are the same in every compared market (open, 2026-09-25)

Megaphone's "The Always Sunny Podcast" is the strongest case: 15 episodes, all byte-identical through `norway`, `germany` and the `us` fallback (2026-09-26, [`region-diff.md`](region-diff.md)). Either the host inserts no dynamic ads on this show, or the same campaign runs in every market Solstein reaches; a market further out, or a direct download from the host's own region, would tell them apart.

Darknet Diaries (PRX Dovetail) keeps 2–3 minutes of ads per episode after cleaning: the cleaned files are that much longer than `itunes:duration`, and the audio that differs between Norway, Sweden and Germany is a single ~1-minute mid-roll. The rest is probably a pre-roll or campaign that runs in all three markets (or host-read ads, which no diff can find). Worth trying: a fallback in a more distant market (e.g. the US, where PRX sells most ads) to see whether that part differs there.

### `trim_break_markers` default (to decide after the trial)

Off by default because a show's own sting spliced in at breaks would go too. Revisit once it has run on real feeds for a while: if it never removes anything that isn't a host's marker, it could default to on.

### Open questions

- **Other hosts:** Acast and Dovetail splice at frame level, RedCircle re-encodes (compared by audio). Megaphone serves MP3s the diff handles but with nothing regional to cut in 15 episodes ([`region-diff.md`](region-diff.md), 2026-09-26). Others are untested. Region diff refuses anything that isn't MP3 (e.g. AAC) rather than guessing.
- **Other variance in ads:** whether ad selection also depends on User-Agent, cookies or random rotation. Known so far: same region at the same moment gives byte-identical files (a show without dynamic ads on 2026-09-24, one with Norwegian ads on 2026-09-25); hours apart, and through a VPN exit instead of direct, the ads differ but the show audio doesn't. Not tested: different User-Agents, and how often the same campaign runs in several markets (which `fallback_exits` covers).
- **Geolocation drift:** what picks the ads is how Acast geolocates the exit IP, not the country in the server list, and VPN IPs are sometimes misplaced. An optional IP-geolocation check through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.

## Exits

### Batching the background queue: not yet run live (2026-09-27)

Built: a small version, scoped to what the open questions below had already settled. Short of tunnels, the pipeline's background queue (never work on request) takes up to 3 of a feed's queued episodes together (`region_diff.batchSize`, `episodes.BatchProcessor`, `database.Store.ClaimQueuedEpisodesForFeed`): every home download first, one key move, then every partner ([`region-diff.md`](region-diff.md), Batching the background queue; [`episodes.md`](episodes.md), Background queue). Covered by synthetic tests (fetch order, one key move per batch, claiming, that on-request work never batches) but not yet run against a real feed and real keys — the next `docker-test` run against a one-key setup with several backlog episodes queued at once would show it.

Settled by the questions below, rather than left open: batching only ever applies when `outbound.Budgeter` says the exits can't have a tunnel each (`BatchSize`) — with a key per exit it changes nothing. The batch size is a fixed 3 (`batchSize` in `modules/regiondiff/processor.go`), not a setting; downloads stay in memory (each still capped at 512 MB), so 3 is deliberately small enough that the worst case (three 220 MB episodes) stays reasonable. Left open still:
- **Whether it's actually worth it.** The live one-key run this fixed the tunnel bug on (2026-09-27) didn't have more than one feed's episodes queued at once, so there's no live measurement yet of how much a batch of 3 actually saves over three separate attempts, only the synthetic guarantee that it fetches homes before partners and moves the key once.
- **A larger, disk-spooled batch.** `episodes.Job.Fetch` holds each download in memory; a batch bigger than a handful would want to spool to disk and read back at diff time, which reaches into `episodes.Job` as well as the processor. Only worth building if 3 in memory turns out not to be enough once this has run live.
- **What happens when a new episode arrives mid-batch.** Not specially handled: a new episode already takes priority over any queued work (workers claim it first), so a batch already running just finishes on its own claimed episodes; nothing pre-empts it partway through, and nothing needed to.

### Tunnel budget: what the exits need is now said, but not measured (2026-09-26)

Built after the live bulk download ran a three-key pool out of tunnels: the start-up warning that adds up the exits in use and says how many keys are missing, the wait for a tunnel to free up, region diff preparing one episode at a time when the exits don't fit, and an unusable exit no longer unwrapping tracking redirects ([`exits.md`](exits.md), [`region-diff.md`](region-diff.md)). Left:

- **Whether a minute is the right wait** (`tunnelWait`). It rides out one finishing download; a burst of long episodes may need longer, and an on-request episode waiting a minute has already answered the client `503`. Measured 2026-09-27 on the one-key setup ([`region-diff.md`](region-diff.md)): with the idle-connection fix in, the pool's one tunnel switched between the pair's two exits about 30 times across 15 episodes (both directions) and freed up in the same second every time — the wait was never actually used. Before the fix was complete, one switch took 27 s of the minute, freed by Go's own 90 s idle-connection timeout rather than the wait itself. Still unmeasured: a poll or the server-list refresh genuinely overlapping a region-diff download already under way (not just a released idle connection) — the contention the wait exists for.
- **Where the real ceiling is:** two episodes downloading through one exit share its tunnel and its bandwidth. Whether that, rather than the tunnel count, is what should limit concurrency is unmeasured — with a key per exit, nothing limits how many episodes share a tunnel.
- **Production** runs four exits on three keys, so it prepares one episode at a time until a fourth key is added; the maintainer is adding one. Then watch that the warning is gone and two are prepared at once.
- Checked live on 2026-09-26 under the load that first broke it (15 backlog episodes of a RedCircle show through ABS, three keys): the start-up warning named the missing key, attempts took turns, and there was **no `tunnel limit reached`, no key-move warning and no crash** — against 97 tunnel-limit failures and one panic in the run before the fixes.

### Open questions

- **Proton NO transfers cut off mid-body (2026-09-25):** three of about eight long downloads through the `norway` Proton exit (NO#23 in production) broke off partway with `unexpected EOF` or `connection reset by peer`; Sweden didn't. The pipeline retries them, but a server that does this often isn't benched, since the WireGuard handshake stays fresh and a mid-body reset looks like the destination's fault. It happened again during the Safety Third burst the same evening: 19 downloads failed with `http2: client connection lost`, 13 through `norway` and 6 through `germany`. Watch whether it keeps happening; if so, count mid-body resets against the server, or prefer another server in the country.
- **DNS through Proton times out now and then (2026-09-25):** four lookups through the `norway` tunnel's DNS (10.2.0.1) failed with `i/o timeout` in about 40 minutes of heavy downloading: a feed poll (the last good copy was served), a fallback download, the server-list refresh. Lookups already retry after 1 s and 2 s. If it keeps happening, retry longer, or cache answers for their TTL. Happened again 2026-09-27, one-key live run below: one lookup of `audio4.redcircle.com` through `NO#23` failed on the tunnel's own resolver and both fallbacks (1.1.1.1, 9.9.9.9) alike; the pipeline's own retry (3 attempts) got it on the second try, 35 s later.
- **Proton connection counting:** one key holds several tunnels at once (verified), but whether Proton counts them as one connection or several against the plan limit (Free 1, Plus 10) is unknown.
- **Server-list format:** the refresh accepts only format version 4. If gluetun-servers moves to a new version, Solstein keeps the last good copy and warns once it is 60 days old ([`exits.md`](exits.md)); reading the new format waits until there is one.

### Deferred ideas

- **Location inference for `.conf` files:** guess a server's country from common file-name patterns (e.g. `se-sto-wg-001`, `NO-FREE#12`), log the guess, and let a declaration always win. Needs the naming patterns of each provider's bulk download.
- **More server-list providers:** gluetun-servers also has WireGuard data for e.g. Mullvad, AirVPN and IVPN; each would be a new provider type with its own key and address rules.
- **Scheduled rotation for `sticky`:** optionally move a sticky exit to another server on a schedule. Only `random` rotates (hourly) today.

## Core

- **Publish immediately, swap later (idea, for clients other than ABS):** publish an episode of a processed feed at once with its ads, and swap in the processed file when ready. Useless for ABS, which downloads once and matches by GUID afterwards; only worth it for clients that re-download changed enclosures (unknown, see below).
- **Chaining processors (idea):** the processor interface allows one processor per feed; chaining would let e.g. a loudness pass run after region diff.
- **Web UI (idea):** there is none; feeds are managed through the API.

## Clients

- **Other clients** (e.g. Podcast Addict, AntennaPod, Pocket Casts via a public URL): whether they seek or resume with `Range` (matters for `stream` mode with dynamic ads), re-download changed enclosures (matters for "publish immediately"), or honour `itunes:new-feed-url`.
