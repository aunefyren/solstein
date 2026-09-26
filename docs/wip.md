# Work in progress

Everything about Solstein that isn't finished: known issues, gaps, open questions, ideas and planned work. The other documents in `docs/` describe only what is built (see [`README.md`](README.md)).

## Rules for this file

- **Add as you go.** An issue, question, idea or trade-off goes here as soon as it is discovered or discussed, even half-formed. Record what is known (measurements, where in the code, options considered) so it can be picked up later.
- **Nothing finished lives here.** When an item is resolved — built, decided, answered, or dropped — remove it from this file, and move what was learned into the document that covers that area (`feeds.md`, `episodes.md`, `region-diff.md`, …): the behaviour, the reason, and any live findings. A dropped idea worth remembering is noted there as "considered and not chosen", with why.
- **Items in progress** say so at the top of the item, with what is done and what is left.
- Group items by area, most pressing first within each.

## Region diff

### Connections through Proton exits fail intermittently beyond DNS (open, 2026-09-26)

Found while checking fallback DNS live (`modules/exits/live_dns_probe_test.go`, temporary, `live` tag: `TestLiveDNSProbe` resolves NRK's hosts and fetches, `TestLiveHTTPProbe` makes 18 ranged requests for an NRK episode per exit, on new and reused connections). With fallback DNS on, the CDN host resolved in most lookups through US-AZ#108, where Proton's resolver alone had failed 9 of 9; the remaining failures are of other kinds:
- **Whole-tunnel stalls:** for 10–15 s nothing gets through, not even the write of a DNS query ("write udp …: i/o timeout" from netstack), and the fallback resolvers time out too; lookups of cached names such as `feeds.acast.com` take 11 s meanwhile. Seen through US-AZ#108 (twice in about 10 minutes) and NO#23 (once). Unknown whether it is the servers, Proton's network, or our netstack/WireGuard device (a send path that blocks).
- **`podkast.nrk.no` (Akamai) cutting requests:** `EOF`, `unexpected EOF` or `connection reset by peer` within 1–5 s, on new and reused connections: 4 of 18 through NO#23, 1 of 18 through US. Its CDN (`nrk-pod-pd.telenorcdn.net`, the redirect target) never did this. Possibly Akamai treating VPN addresses with suspicion.
- **Reused connections after 20 s idle:** through US, one `http2: client connection lost` and one `timeout awaiting response headers` (30 s) out of 6.

Production copes through retries, and a fallback exit that fails no longer fails the attempt ([`region-diff.md`](region-diff.md)), but a failed home or partner download still does. To look into: whether other US and NO servers stall the same way (the exit stays on one server unless its handshake fails), a packet capture of a stall inside the device, and retrying a request once on `EOF` from the redirecting host. Delete the probe file once this is settled.

Open question: should a tunnel DNS failure with a fresh handshake count against the server? Today `through` treats it as the destination's problem, so the exit keeps the same server. Proton's resolver failed per name rather than per server (the same names through US and NO servers), and with fallback DNS the lookup mostly succeeds; the stalls above may argue for it.

### Comparing by audio: not checked in production yet (2026-09-26)

Built and checked offline on the kept Safety Third downloads ([`region-diff.md`](region-diff.md), Hosts that re-encode), but not yet run on the server against RedCircle and ABS. Worth watching: the time per episode on the server (about 7 s per episode on the development machine, for two ~80-minute downloads measured at once; a slower CPU may push episodes processed on request past the 20 s wait), memory (570 MB peak for two 80-minute downloads here, 150 MB above just holding them; a small server running two workers at once needs about twice that), and whether any host re-encodes with ads in both markets, so that cuts at breaks are made in production.

### Why five backlog downloads were implausible (open question)

Five of 115 "It Was A Sh*t Show" episodes failed as implausible in production within a minute (2026-09-25, [`region-diff.md`](region-diff.md)); downloaded again, they diff cleanly. Most likely one side got a partial or wrong file during the burst of downloads, but it is unconfirmed. Re-downloaded through ABS 25 minutes later (14:24–14:25, one at a time, both tunnels opened cold), all five were cleaned in 3–12 s each, 3 breaks and about 3 minutes removed per episode, and none of the downloads looked incomplete: consistent with a passing problem on the host's side. Solstein now re-fetches downloads that look cut off, asks for fresh copies after a failure, and applies the failure policy after three failed attempts on request. To settle it: run with `keep_failed_downloads` on, and look at the kept files and note the next time it happens. Then decide whether anything else is needed. Withheld episodes are now retried slowly, and a backlog can be cleaned ahead two at a time with `prepare` instead of through ABS's burst; ABS re-downloading a whole backlog still processes on request, as fast as it asks.

### Background queue: not checked live yet (2026-09-25)

Slow retries of withheld episodes, `POST /api/v1/feeds/{id}/retry` (and `/api/v1/retry`) and `POST /api/v1/feeds/{id}/prepare` ([`episodes.md`](episodes.md), Background queue) are built and tested, but haven't run against Acast and ABS yet. Worth checking: a `prepare` of a large region-diff backlog (does two at a time stay clear of the burst failures above?), and that a withheld new episode published by a late retry is picked up by ABS (it should be, being dated when first served).

### Ads that are the same in every compared market (open, 2026-09-25)

Darknet Diaries (PRX Dovetail) keeps 2–3 minutes of ads per episode after cleaning: the cleaned files are that much longer than `itunes:duration`, and the audio that differs between Norway, Sweden and Germany is a single ~1-minute mid-roll. The rest is probably a pre-roll or campaign that runs in all three markets (or host-read ads, which no diff can find). Worth trying: a fallback in a more distant market (e.g. the US, where PRX sells most ads) to see whether that part differs there.

### Break markers encoded into show segments (open)

`trim_break_markers` removes only markers that are their own spliced segment ([`region-diff.md`](region-diff.md)). In "Corner Piece", the chime after each mid-roll is encoded together with the start of the next show segment, so one chime per break remains even with trimming on.

Cutting it would leave the first show frame without the bit-reservoir bytes it borrows from the chime's last frame. Cuts at breaks now keep such frames as silent frames ([`region-diff.md`](region-diff.md), Write), so the way round it exists: trim the marker but keep its last one or two frames silent (26–52 ms of silence in place of 2.3 s of chime). Not built: marker trimming still requires a clean frame after the marker. Only worth it if the remaining chime bothers in practice.

### `trim_break_markers` default (to decide after the trial)

Off by default because a show's own sting spliced in at breaks would go too. Revisit once it has run on real feeds for a while: if it never removes anything that isn't a host's marker, it could default to on.

### Open questions

- **Other hosts:** Acast and Dovetail splice at frame level, RedCircle re-encodes (compared by audio). Others are untested. Region diff refuses anything that isn't MP3 (e.g. AAC) rather than guessing.
- **Other variance in ads:** whether ad selection also depends on User-Agent, cookies or random rotation. Known so far: same region at the same moment gives byte-identical files (a show without dynamic ads on 2026-09-24, one with Norwegian ads on 2026-09-25); hours apart, and through a VPN exit instead of direct, the ads differ but the show audio doesn't. Not tested: different User-Agents, and how often the same campaign runs in several markets (which `fallback_exits` covers).
- **Geolocation drift:** what picks the ads is how Acast geolocates the exit IP, not the country in the server list, and VPN IPs are sometimes misplaced. An optional IP-geolocation check through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.

## Exits

### Open questions

- **Proton NO transfers cut off mid-body (2026-09-25):** three of about eight long downloads through the `norway` Proton exit (NO#23 in production) broke off partway with `unexpected EOF` or `connection reset by peer`; Sweden didn't. The pipeline retries them, but a server that does this often isn't benched, since the WireGuard handshake stays fresh and a mid-body reset looks like the destination's fault. It happened again during the Safety Third burst the same evening: 19 downloads failed with `http2: client connection lost`, 13 through `norway` and 6 through `germany`. Watch whether it keeps happening; if so, count mid-body resets against the server, or prefer another server in the country.
- **DNS through Proton times out now and then (2026-09-25):** four lookups through the `norway` tunnel's DNS (10.2.0.1) failed with `i/o timeout` in about 40 minutes of heavy downloading: a feed poll (the last good copy was served), a fallback download, the server-list refresh. Lookups already retry after 1 s and 2 s. If it keeps happening, retry longer, or cache answers for their TTL.
- **Proton connection counting:** one key holds several tunnels at once (verified), but whether Proton counts them as one connection or several against the plan limit (Free 1, Plus 10) is unknown.
- **Server-list format:** the refresh accepts only format version 4. If gluetun-servers moves to a new version, Solstein keeps the last good copy and warns once it is 60 days old ([`exits.md`](exits.md)); reading the new format waits until there is one.

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
