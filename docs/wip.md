# Work in progress

Everything about Solstein that isn't finished: known issues, gaps, open questions, ideas and planned work. The other documents in `docs/` describe only what is built (see [`README.md`](README.md)).

## Rules for this file

- **Add as you go.** An issue, question, idea or trade-off goes here as soon as it is discovered or discussed, even half-formed. Record what is known (measurements, where in the code, options considered) so it can be picked up later.
- **Nothing finished lives here.** When an item is resolved — built, decided, answered, or dropped — remove it from this file, and move what was learned into the document that covers that area (`feeds.md`, `episodes.md`, `region-diff.md`, …): the behaviour, the reason, and any live findings. A dropped idea worth remembering is noted there as "considered and not chosen", with why.
- **Items in progress** say so at the top of the item, with what is done and what is left.
- Group items by area, most pressing first within each.

## Region diff

### Break markers encoded into show segments (open)

`trim_break_markers` removes only markers that are their own spliced segment ([`region-diff.md`](region-diff.md)). In "Corner Piece", the chime after each mid-roll is encoded together with the start of the next show segment, so one chime per break remains even with trimming on.

Cutting it would leave the first show frame without the bit-reservoir bytes it borrows from the chime's last frame: a 26 ms decode error, possibly a click. Ways round it, none tried: re-encode just that one frame (would need an MP3 encoder, which Solstein has none of and the no-re-encoding rule avoids), or replace the chime's frames with silent frames of the same size that still carry the borrowed bytes (keeps the timing; complex, and depends on the encoder's reservoir use). Only worth it if the remaining chime bothers in practice.

### `trim_break_markers` default (to decide after the trial)

Off by default because a show's own sting spliced in at breaks would go too. Revisit once it has run on real feeds for a while: if it never removes anything that isn't a host's marker, it could default to on.

### The home country of `direct` (idea)

The same-country checks can't see `direct`'s country (it would need an outside geolocation service, which is ruled out), so a pair like `["direct", "norway"]` from Norway is never flagged. An optional `home_country` setting, declared by the operator, would let the checks treat `direct` as that country. Not asked for yet.

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
