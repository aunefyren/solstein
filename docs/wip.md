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
- **How long a moved key stays disturbed:** `keyMoveSettle` (3 minutes) is a guess from WireGuard's 180 s session lifetime. Measured directly now (`exits.md`, Measured live), but noisily and non-monotonically — doesn't overturn the 3-minute default, doesn't prove it necessary either. A larger sample per pause, or several repeats, would be needed to say more.
- **`podkast.nrk.no` (Akamai) cutting requests** (`EOF` within 1–5 s, 4 of 18 through NO#23): the original measurement ran while production shared the probe's keys, which confounded it. Production and testing now confirmed on separate keys, so that confound is gone, but it hasn't actually been rechecked yet: `prod.log` (2026-09-27) shows no NRK activity, but a feed that polls cleanly with nothing new logs nothing at all (`feeds/poller.go`), so that's not evidence either way — NRK feeds are still subscribed in production. Watch the next time an NRK episode is actually downloaded.
- **Probe files** `modules/exits/live_dns_probe_test.go` and `live_stall_probe_test.go` (temporary, `live` tag): delete once the above is settled.

Open question: should a tunnel DNS failure with a fresh handshake count against the server? The stalls turned out to be key conflicts, where switching servers moves the key and makes things worse, so probably not; left open until the above is settled.

### Comparing by audio: what the RedCircle run left open (2026-09-26)

Run live against RedCircle and ABS on 15 "Safety Third" episodes ([`region-diff.md`](region-diff.md)): about a minute each, 805 MB peak for one pair of up to 220 MB downloads, no failure. Left:
- **Why RedCircle leaves Norway alone** is unknown: no advertiser for the market, or no ad server for it at all. If it holds for every RedCircle show, region diff has nothing to do on them and the pair's downloads cost twice the bandwidth for nothing — worth a way to notice a feed that never has ads at home and stop comparing it (and what would then make it start again).
- **A slower server** may push an episode of this size past the 20 s wait more often than the 22 s best case here; with `prepare_ahead` that no longer matters.

### Why five backlog downloads were implausible (open question)

Five of 115 "It Was A Sh*t Show" episodes failed as implausible in production within a minute (2026-09-25, [`region-diff.md`](region-diff.md)); downloaded again, they diff cleanly. Most likely one side got a partial or wrong file during the burst of downloads, but it is unconfirmed. Re-downloaded through ABS 25 minutes later (14:24–14:25, one at a time, both tunnels opened cold), all five were cleaned in 3–12 s each, 3 breaks and about 3 minutes removed per episode, and none of the downloads looked incomplete: consistent with a passing problem on the host's side. Solstein now re-fetches downloads that look cut off, asks for fresh copies after a failure, and applies the failure policy after three failed attempts on request. Withheld episodes are now retried slowly, and a backlog can be cleaned ahead two at a time with `prepare` instead of through ABS's burst; ABS re-downloading a whole backlog still processes on request, as fast as it asks.

**Tried and inconclusive (2026-09-27):** a comparable burst on `docker-test` — 31 backlog episodes queued at once, `keep_failed_downloads: true` — reproduced nothing: no implausible result, no failure that stuck, one mid-body break resumed cleanly by the fix built since (`region-diff.md`, Retried since). Doesn't confirm or rule out a specific cause for the original five; it does show the current code already handles the failure shape (a cut-off download) that would explain them, without needing to reproduce the exact cause. Left open: whether the original burst hit something this one didn't (more concurrency, a specific host state that evening) — watch for a recurrence in production rather than trying to force another one here.

### Ads that are the same in every compared market (open, 2026-09-25)

Megaphone's "The Always Sunny Podcast" is the strongest case: 15 episodes, all byte-identical through `norway`, `germany` and the `us` fallback (2026-09-26, [`region-diff.md`](region-diff.md)). Either the host inserts no dynamic ads on this show, or the same campaign runs in every market Solstein reaches; a market further out, or a direct download from the host's own region, would tell them apart.

Darknet Diaries (PRX Dovetail) keeps 2–3 minutes of ads per episode after cleaning: the cleaned files are that much longer than `itunes:duration`, and the audio that differs between Norway, Sweden and Germany is a single ~1-minute mid-roll. The rest is probably a pre-roll or campaign that runs in all three markets (or host-read ads, which no diff can find). Worth trying: a fallback in a more distant market (e.g. the US, where PRX sells most ads) to see whether that part differs there.

### Open questions

- **Other hosts:** Acast and Dovetail splice at frame level, RedCircle re-encodes (compared by audio). Megaphone serves MP3s the diff handles but with nothing regional to cut in 15 episodes ([`region-diff.md`](region-diff.md), 2026-09-26). Others are untested. Region diff refuses anything that isn't MP3 (e.g. AAC) rather than guessing.
- **Other variance in ads:** whether ad selection also depends on User-Agent, cookies or random rotation. Known so far: same region at the same moment gives byte-identical files (a show without dynamic ads on 2026-09-24, one with Norwegian ads on 2026-09-25); hours apart, and through a VPN exit instead of direct, the ads differ but the show audio doesn't. Not tested: different User-Agents, and how often the same campaign runs in several markets (which `fallback_exits` covers).
- **Geolocation drift:** what picks the ads is how Acast geolocates the exit IP, not the country in the server list, and VPN IPs are sometimes misplaced. An optional IP-geolocation check through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.

## Exits

### Open questions

- **Proton connection counting:** one key holds several tunnels at once (verified), but whether Proton counts them as one connection or several against the plan limit (Free 1, Plus 10) is unknown.
- **Server-list format:** the refresh accepts only format version 4. If gluetun-servers moves to a new version, Solstein keeps the last good copy and warns once it is 60 days old ([`exits.md`](exits.md)); reading the new format waits until there is one.

### Deferred ideas

- **A larger, disk-spooled batch** (beyond the fixed 3, in-memory, `docs/region-diff.md`, Batching the background queue): only worth it once 3 turns out not to be enough — unmeasured, but the verified side-by-side (`region-diff.md`, 2026-09-27) found batching's saving comes from fewer key moves, not throughput, so a bigger batch would need more episodes short of a tunnel each at once to matter, which a busier setup could still reach.
- **Location inference for `.conf` files:** guess a server's country from common file-name patterns (e.g. `se-sto-wg-001`, `NO-FREE#12`), log the guess, and let a declaration always win. Needs the naming patterns of each provider's bulk download.
- **More server-list providers:** gluetun-servers also has WireGuard data for e.g. Mullvad, AirVPN and IVPN; each would be a new provider type with its own key and address rules.
- **Scheduled rotation for `sticky`:** optionally move a sticky exit to another server on a schedule. Only `random` rotates (hourly) today.

## Core

- **Publish immediately, swap later (idea, for clients other than ABS):** publish an episode of a processed feed at once with its ads, and swap in the processed file when ready. Useless for ABS, which downloads once and matches by GUID afterwards; only worth it for clients that re-download changed enclosures (unknown, see below).
- **Chaining processors (idea):** the processor interface allows one processor per feed; chaining would let e.g. a loudness pass run after region diff.
- **Web UI (idea):** there is none; feeds are managed through the API.

## Clients

- **Other clients** (e.g. Podcast Addict, AntennaPod, Pocket Casts via a public URL): whether they seek or resume with `Range` (matters for `stream` mode with dynamic ads), re-download changed enclosures (matters for "publish immediately"), or honour `itunes:new-feed-url`.
