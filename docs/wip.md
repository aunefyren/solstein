# Work in progress

Everything about Solstein that isn't finished: known issues, gaps, open questions, ideas and planned work. The other documents in `docs/` describe only what is built (see [`README.md`](README.md)).

## Rules for this file

- **Add as you go.** An issue, question, idea or trade-off goes here as soon as it is discovered or discussed, even half-formed. Record what is known (measurements, where in the code, options considered) so it can be picked up later.
- **Nothing finished lives here.** When an item is resolved — built, decided, answered, or dropped — remove it from this file, and move what was learned into the document that covers that area (`feeds.md`, `episodes.md`, `region-diff.md`, …): the behaviour, the reason, and any live findings. A dropped idea worth remembering is noted there as "considered and not chosen", with why.
- **Items in progress** say so at the top of the item, with what is done and what is left.
- Group items by area, most pressing first within each.

## Region diff

### Why five backlog downloads were implausible (open question)

Five of 115 "It Was A Sh*t Show" episodes failed as implausible in production within a minute (2026-09-25, [`region-diff.md`](region-diff.md)); downloaded again, they diff cleanly. Most likely one side got a partial or wrong file during the burst of downloads, but it is unconfirmed. Re-downloaded through ABS 25 minutes later (14:24–14:25, one at a time, both tunnels opened cold), all five were cleaned in 3–12 s each, 3 breaks and about 3 minutes removed per episode, and none of the downloads looked incomplete: consistent with a passing problem on the host's side. Solstein now re-fetches downloads that look cut off, asks for fresh copies after a failure, and applies the failure policy after three failed attempts on request. To settle it: run with `keep_failed_downloads` on, and look at the kept files and note the next time it happens. Then decide whether anything else is needed (e.g. pacing large backlogs).

### Withheld episodes are never retried on their own (gap, planned, 2026-09-25)

With `on_failure: hide`, an episode that fails for good is withheld (left out of the feed, `404`) until a settings change or a new diff version resets it ([`episodes.md`](episodes.md), Settings changes). Nothing retries it later. A new episode only gets there after the whole retry schedule (about 4½ hours), but a backlog episode requested by ABS is withheld after three failed attempts that can all fall within a minute (ABS retries once by itself) — so a passing problem at the host, like the burst failures above, can hide an episode indefinitely. Wanted:
- **A slow background retry for withheld episodes:** e.g. once a day for a week, with fresh downloads, before they stay withheld. Backlog episodes aren't claimed by the pipeline today, so this needs its own pass (the housekeeper's hourly sweep, or a pipeline query for withheld episodes whose next retry is due).
- **A manual retry:** an API action to retry a feed's (or all) failed episodes, e.g. after a fix, without the settings-change trick. Needs an entry in `openapi.yaml`.
- **A warning when `hide` is on:** at start-up (global or any feed's `region_diff_on_failure: hide`), say that episodes which can't be cleaned are kept out of the feed and when they are tried again; and when an episode is withheld, log how to get it back. Until the retry above exists, the warning should say plainly that it isn't retried on its own.

### Ads that are the same in every compared market (open, 2026-09-25)

Darknet Diaries (PRX Dovetail) keeps 2–3 minutes of ads per episode after cleaning: the cleaned files are that much longer than `itunes:duration`, and the audio that differs between Norway, Sweden and Germany is a single ~1-minute mid-roll. The rest is probably a pre-roll or campaign that runs in all three markets (or host-read ads, which no diff can find). Worth trying: a fallback in a more distant market (e.g. the US, where PRX sells most ads) to see whether that part differs there.

### Break markers encoded into show segments (open)

`trim_break_markers` removes only markers that are their own spliced segment ([`region-diff.md`](region-diff.md)). In "Corner Piece", the chime after each mid-roll is encoded together with the start of the next show segment, so one chime per break remains even with trimming on.

Cutting it would leave the first show frame without the bit-reservoir bytes it borrows from the chime's last frame: a 26 ms decode error, possibly a click. Ways round it, none tried: re-encode just that one frame (would need an MP3 encoder, which Solstein has none of and the no-re-encoding rule avoids), or replace the chime's frames with silent frames of the same size that still carry the borrowed bytes (keeps the timing; complex, and depends on the encoder's reservoir use). Only worth it if the remaining chime bothers in practice.

### `trim_break_markers` default (to decide after the trial)

Off by default because a show's own sting spliced in at breaks would go too. Revisit once it has run on real feeds for a while: if it never removes anything that isn't a host's marker, it could default to on.

### The home country of `direct` (idea)

The same-country checks can't see `direct`'s country (it would need an outside geolocation service, which is ruled out), so a pair like `["direct", "norway"]` from Norway is never flagged. An optional `home_country` setting, declared by the operator, would let the checks treat `direct` as that country. Not asked for yet.

### Keeping the raw downloads of successful diffs (idea)

`keep_failed_downloads` keeps them for failed diffs. Keeping them for successful ones too (the original `keep_sources` idea) would help debug a bad cut that passed the sanity checks; not built, and costs twice an episode's size per episode.

### Open questions

- **Other hosts than Acast:** whether they splice at frame level too. Region diff refuses anything that isn't MP3 rather than guessing; a host that re-encodes per listener can't be diffed this way at all.
- **Other variance in ads:** whether ad selection also depends on User-Agent, cookies or random rotation. Known so far: same region at the same moment gives byte-identical files (a show without dynamic ads on 2026-09-24, one with Norwegian ads on 2026-09-25); hours apart, and through a VPN exit instead of direct, the ads differ but the show audio doesn't. Not tested: different User-Agents, and how often the same campaign runs in several markets (which `fallback_exits` covers).
- **Geolocation drift:** what picks the ads is how Acast geolocates the exit IP, not the country in the server list, and VPN IPs are sometimes misplaced. An optional IP-geolocation check through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.

## Exits

### Open questions

- **Proton NO transfers cut off mid-body (2026-09-25):** three of about eight long downloads through the `norway` Proton exit (NO#23 in production) broke off partway with `unexpected EOF` or `connection reset by peer`; Sweden didn't. The pipeline retries them, but a server that does this often isn't benched, since the WireGuard handshake stays fresh and a mid-body reset looks like the destination's fault. Watch whether it keeps happening; if so, count mid-body resets against the server, or prefer another server in the country.
- **DNS through Proton times out now and then (2026-09-25):** four lookups through the `norway` tunnel's DNS (10.2.0.1) failed with `i/o timeout` in about 40 minutes of heavy downloading: a feed poll (the last good copy was served), a fallback download, the server-list refresh. Lookups already retry after 1 s and 2 s. If it keeps happening, retry longer, or cache answers for their TTL.
- **Proton connection counting:** one key holds several tunnels at once (verified), but whether Proton counts them as one connection or several against the plan limit (Free 1, Plus 10) is unknown.
- **Server-list resilience:** the refresh accepts only format version 4. If gluetun-servers moves to a new version, Solstein keeps the last good copy or the embedded snapshot indefinitely; it should at least warn when the list is getting old, and support the new format.

### Deferred ideas

- **Location inference for `.conf` files:** guess a server's country from common file-name patterns (e.g. `se-sto-wg-001`, `NO-FREE#12`), log the guess, and let a declaration always win. Needs the naming patterns of each provider's bulk download.
- **More server-list providers:** gluetun-servers also has WireGuard data for e.g. Mullvad, AirVPN and IVPN; each would be a new provider type with its own key and address rules.
- **Scheduled rotation for `sticky`:** optionally move a sticky exit to another server on a schedule. Only `random` rotates (hourly) today.

## Core

- **`disable_direct` on by default (wanted, 2026-09-25).** The maintainer wants the direct exit off unless switched on. To work out before building:
  - A fresh install has no VPN, so `disable_direct` with no `default_exit` can't simply stop start-up as it does today. Options: direct is disabled only once a VPN exit exists (and `default_exit` then defaults to the first, or must be named); or start-up refuses until either a VPN is set up or `disable_direct: false` is set explicitly (safe, but a harder first run).
  - Existing `config.json` files have `"disable_direct": false` written out, so a new default wouldn't reach them; only a missing field would take it. Possibly a tri-state (unset = default) or a one-time notice.
  - Region diff's `direct` home side and feeds with `exit: direct` then become invalid by default; the start-up warnings and the README would need to say how to opt back in.
- **Cache size cap (question):** whether to add a size limit on top of the 14-day retention, and whether to evict an episode early once a client has fetched it completely.
- **Publish immediately, swap later (idea, for clients other than ABS):** publish an episode of a processed feed at once with its ads, and swap in the processed file when ready. Useless for ABS, which downloads once and matches by GUID afterwards; only worth it for clients that re-download changed enclosures (unknown, see below).
- **Chaining processors (idea):** the processor interface allows one processor per feed; chaining would let e.g. a loudness pass run after region diff.
- **Web UI (idea):** there is none; feeds are managed through the API.

## Clients

- **Other clients** (e.g. Podcast Addict, AntennaPod, Pocket Casts via a public URL): whether they seek or resume with `Range` (matters for `stream` mode with dynamic ads), re-download changed enclosures (matters for "publish immediately"), or honour `itunes:new-feed-url`.
