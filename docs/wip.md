# Work in progress

Everything about Solstein that isn't finished: known issues, gaps, open questions, ideas and planned work. The other documents in `docs/` describe only what is built (see [`README.md`](README.md)).

## Rules for this file

- **Add as you go.** An issue, question, idea or trade-off goes here as soon as it is discovered or discussed, even half-formed. Record what is known (measurements, where in the code, options considered) so it can be picked up later.
- **Nothing finished lives here.** When an item is resolved — built, decided, answered, or dropped — remove it from this file, and move what was learned into the document that covers that area (`feeds.md`, `episodes.md`, `region-diff.md`, …): the behaviour, the reason, and any live findings. A dropped idea worth remembering is noted there as "considered and not chosen", with why.
- **Items in progress** say so at the top of the item, with what is done and what is left.
- Group items by area, most pressing first within each.

## Region diff

### Break markers are kept (issue, proposed fix)

The cleaned "Corner Piece" episode has no ads left, but the short chime around each ad break is still in it.

**Cause:** the chime is its own spliced segment, and identical in both regions, so the diff keeps it as shared audio attached to the show segment next to it. Measured on the live Norway/Sweden pair (`config/live/`):
- It lasts 2.27 s (87 frames) and starts and ends on clean frames (`main_data_begin = 0`), as ads do.
- It appears four times: after the pre-roll (frames 3471–3558), and before the first mid-roll, the second mid-roll and the post-roll (frames 34070–34157, 71199–71286 and 99325–99412). Its audio data is byte-identical at all four.
- There is no chime after the two mid-rolls: the show resumes directly.
- Most likely a break bumper configured in Acast rather than part of the recording, but that can't be proven from the files alone.

**Proposal:** at the edge of a kept run, next to a removed break, drop a segment when all of these hold:
- it starts and ends on clean frames;
- it is shorter than about 5 s;
- the same audio (compared without headers and side information) appears at two or more break edges in the episode.

The repetition rule means a short one-off stretch of real show audio next to a break is never removed. Setting `trim_break_markers`, default on. Trade-off: a show that uses its own branded sting at breaks loses that sting too.

**Where:** `modules/regiondiff/diff.go`, after the kept runs are trimmed to segment boundaries and before the sanity checks. Test with synthetic splices, and assert on the live pair that exactly four 87-frame pieces are dropped.

### Switching region diff on for a feed with cached episodes (gap)

When region diff is switched on for an existing feed (per feed, or through `enabled`), episodes already cached unprocessed stay cached and keep being served with their ads until their cache copy expires (`cache_retention_days`); after that they are processed on the next request. Only new and uncached episodes are cleaned at once.

**Option:** when a feed's `region_diff_in_use` turns on, drop its unprocessed cache copies (cached, no `process_note`), so the next request processes them. A change of the global `enabled` would need the same check at start-up. Not needed while feeds are set up with region diff from the start.

### Warn when both sides of a pair can be the same country (planned)

A pair whose two exits can resolve to the same country — e.g. `direct` from Norway plus a loose exit that can fall back to `NO` — would diff two copies of the same ads and find nothing. Start-up should warn about it. Not built: `regiondiff.Setup` only checks that the two exits exist and differ.

### Keeping the raw downloads (idea)

`keep_sources`: keep both regions' downloads next to the cleaned file, to debug a bad cut. Proposed in the design, not built; the live-test downloads in `config/live/` cover development for now.

### Open questions

- **Other hosts than Acast:** whether they splice at frame level too. Region diff refuses anything that isn't MP3 rather than guessing; a host that re-encodes per listener can't be diffed this way at all.
- **Other variance in ads:** whether ad selection also depends on User-Agent, cookies or random rotation. Known so far: same region at the same moment gives byte-identical files (a show without dynamic ads on 2026-09-24, one with Norwegian ads on 2026-09-25); hours apart, and through a VPN exit instead of direct, the ads differ but the show audio doesn't. Not tested: different User-Agents, and how often the same campaign runs in several markets (which `fallback_exits` covers).
- **Geolocation drift:** what picks the ads is how Acast geolocates the exit IP, not the country in the server list, and VPN IPs are sometimes misplaced. An optional IP-geolocation check through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.

## Exits

### Open questions

- **Proton connection counting:** one key holds several tunnels at once (verified), but whether Proton counts them as one connection or several against the plan limit (Free 1, Plus 10) is unknown.
- **Server-list resilience:** the refresh accepts only format version 4. If gluetun-servers moves to a new version, Solstein keeps the last good copy or the embedded snapshot indefinitely; it should at least warn when the list is getting old, and support the new format.

### Deferred ideas

- **Location inference for `.conf` files:** guess a server's country from common file-name patterns (e.g. `se-sto-wg-001`, `NO-FREE#12`), log the guess, and let a declaration always win. Needs the naming patterns of each provider's bulk download.
- **More server-list providers:** gluetun-servers also has WireGuard data for e.g. Mullvad, AirVPN and IVPN; each would be a new provider type with its own key and address rules.
- **Scheduled rotation for `sticky`:** optionally move a sticky exit to another server on a schedule. Only `random` rotates (hourly) today.

## Core

- **Cache size cap (question):** whether to add a size limit on top of the 14-day retention, and whether to evict an episode early once a client has fetched it completely.
- **Publish immediately, swap later (idea, for clients other than ABS):** publish an episode of a processed feed at once with its ads, and swap in the processed file when ready. Useless for ABS, which downloads once and matches by GUID afterwards; only worth it for clients that re-download changed enclosures (unknown, see below).
- **Chaining processors (idea):** the processor interface allows one processor per feed; chaining would let e.g. a loudness pass run after region diff.
- **Web UI (idea):** there is none; feeds are managed through the API.

## Clients

- **Other clients** (e.g. Podcast Addict, AntennaPod, Pocket Casts via a public URL): whether they seek or resume with `Range` (matters for `stream` mode with dynamic ads), re-download changed enclosures (matters for "publish immediately"), or honour `itunes:new-feed-url`.
