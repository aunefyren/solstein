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
- **`podkast.nrk.no` (Akamai) cutting requests** (`EOF` within 1–5 s, 4 of 18 through NO#23): the original measurement ran while production shared the probe's keys, which confounded it. Production and testing now confirmed on separate keys, so that confound is gone, but it hasn't actually been rechecked yet: `prod.log` (2026-09-27) shows no NRK activity, but a feed that polls cleanly with nothing new logs nothing at all (`feeds/poller.go`), so that's not evidence either way — NRK feeds are still subscribed in production. Watch the next time an NRK episode is actually downloaded.
  - **Rechecked on `docker-test` (2026-09-28), two keys, nothing else on them:** 20 NRK episodes (two shows through ABS, [`clients.md`](clients.md)), each downloaded through `NO#23` and `DE#187` at the same moment — 40 downloads of 47–93 MB from `podkast.nrk.no`, **no `EOF`, no break, no resume, no retry**. Points strongly at the original 4 of 18 being the shared-key confound rather than Akamai. Not closed yet: that was a 1-request-a-second probe over minutes, this was 40 full downloads over 3 minutes, so a different load shape; the maintainer to decide whether this settles it (and the probe files below can go).
- **Probe files** `modules/exits/live_dns_probe_test.go`, `live_stall_probe_test.go` and `live_keymove_repeat_test.go` (temporary, `live` tag): delete once the above is settled.

Open question: should a tunnel DNS failure with a fresh handshake count against the server? The stalls turned out to be key conflicts, where switching servers moves the key and makes things worse, so probably not — and now settled that it's chance per session rather than a pause-length threshold (`exits.md`, keyMoveSettle), which only reinforces "probably not"; left open until the NRK/Akamai recheck above is settled too.

### Comparing by audio: what the RedCircle run left open (2026-09-26)

Run live against RedCircle and ABS on 15 "Safety Third" episodes ([`region-diff.md`](region-diff.md)): about a minute each, 805 MB peak for one pair of up to 220 MB downloads, no failure. Left:
- **A slower server** may push an episode of this size past the 20 s wait more often than the 22 s best case here; with `prepare_ahead` that no longer matters.

### Why five backlog downloads were implausible (open question)

Five of 115 "It Was A Sh*t Show" episodes failed as implausible in production within a minute (2026-09-25, [`region-diff.md`](region-diff.md)); downloaded again, they diff cleanly. Most likely one side got a partial or wrong file during the burst of downloads, but it is unconfirmed. Re-downloaded through ABS 25 minutes later (14:24–14:25, one at a time, both tunnels opened cold), all five were cleaned in 3–12 s each, 3 breaks and about 3 minutes removed per episode, and none of the downloads looked incomplete: consistent with a passing problem on the host's side. Solstein now re-fetches downloads that look cut off, asks for fresh copies after a failure, and applies the failure policy after three failed attempts on request. Withheld episodes are now retried slowly, and a backlog can be cleaned ahead two at a time with `prepare` instead of through ABS's burst; ABS re-downloading a whole backlog still processes on request, as fast as it asks.

**Tried and inconclusive (2026-09-27):** a comparable burst on `docker-test` — 31 backlog episodes queued at once, `keep_failed_downloads: true` — reproduced nothing: no implausible result, no failure that stuck, one mid-body break resumed cleanly by the fix built since (`region-diff.md`, Retried since). Doesn't confirm or rule out a specific cause for the original five; it does show the current code already handles the failure shape (a cut-off download) that would explain them, without needing to reproduce the exact cause. Left open: whether the original burst hit something this one didn't (more concurrency, a specific host state that evening) — watch for a recurrence in production rather than trying to force another one here.

### Open questions

- **Other hosts:** Acast and Dovetail splice at frame level, RedCircle re-encodes (compared by audio). Megaphone serves MP3s the diff handles but with nothing regional to cut in 15 episodes ([`region-diff.md`](region-diff.md), 2026-09-26). Others are untested. Region diff refuses anything that isn't MP3 (e.g. AAC) rather than guessing.
- **Other variance in ads:** whether ad selection also depends on User-Agent, cookies or random rotation. Known so far: same region at the same moment gives byte-identical files (a show without dynamic ads on 2026-09-24, one with Norwegian ads on 2026-09-25); hours apart, and through a VPN exit instead of direct, the ads differ but the show audio doesn't. Not tested: different User-Agents, and how often the same campaign runs in several markets (which `fallback_exits` covers).
- **Geolocation drift:** what picks the ads is how Acast geolocates the exit IP, not the country in the server list, and VPN IPs are sometimes misplaced. An optional IP-geolocation check through the tunnel (off by default, as it adds an external dependency)? The real test remains whether the two downloads differ.

### Deferred ideas

- **Skip comparing a feed that never has ads at home** (`region-diff.md`, RedCircle, compared by audio): if a show's home download is reliably ad-free (RedCircle's "Safety Third", 15 of 15), the pair's second download costs bandwidth for a result that's already known. A cost optimization, not a correctness one — ad removal already works either way. Needs a way to notice it (some run count of clean results?) and to un-notice it if the host ever starts inserting ads again. NRK (public broadcaster, no ads at all) is the clearest case: 20 of 20 "no dynamic ads found" on 2026-09-28 (`clients.md`), each costing a second download of 47–93 MB. Simplest version for such hosts: switch region diff off for that feed (`region_diff: off`), which already exists; the question is only whether Solstein should notice it by itself.

## Exits

### Open questions

- **Why US-AZ#108 → NRK crawls** (2026-09-28): 95 KB/s in production and ~160–200 KB/s on `docker-test`, while `DE#187` fetched the same files at several MB/s at the same moment, so it isn't NRK slowing every non-Norwegian address. That one server, the transatlantic path to Telenor's CDN edge, or NRK throttling the US specifically: untested (another US server, and a nearer fallback like `sweden`, would tell them apart). Region diff now gives up on a slow fallback ([`region-diff.md`](region-diff.md)), so it costs at most the limit; whether a slow exit should also count against its server (as a failed one does) is left open with the similar DNS question under Region diff.
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
- **Web UI: what's left** (the feed list with its region diff and prepare-ahead switches is built, [`web-ui.md`](web-ui.md); built to [`style-guide.md`](style-guide.md)):
  - **Adding and removing feeds, and showing a feed's signed URL** for a client to subscribe to: held back while there was no sign-in, since subscribing makes Solstein fetch a URL on the caller's behalf. Open: whether every signed-in user may do it, or that waits for roles (below).
  - **A status page:** what prompted the UI (a production episode that looked stuck for 15 minutes on a slow fallback, `region-diff.md`): each feed's episodes with state and note, and jobs in progress with exit, bytes so far and rate (what `logProgress` logs). Needs a read-side the core doesn't expose yet (episodes per feed, running jobs). The tunnels half of it is built: the exits page shows each tunnel's users and last handshake (`web-ui.md`).
  - **More per-feed settings** as needed (exit, delivery mode, region diff exits and failure policy): the API has them all already.
  - Agreed (2026-09-28): per-feed tweaking isn't the fix for a slow exit; Solstein handles that itself (the fallback limit, and possibly "Skip comparing a feed that never has ads", above).

## Sign-in

Built: users on the console, passwords, optional TOTP, sessions on an OAuth-shaped token model ([`sign-in.md`](sign-in.md)). Planned on top of it, in no fixed order:

- **Personal access tokens:** a user creates a named, scoped token under Account (a `models.Token` of kind `pat`, shown once), used as `Authorization: Bearer` on the feed API next to the subscribe token. Open: scopes (`feeds:read`, `feeds:write`?), expiry by default or not, and whether they should replace the single `auth_token` for the API in time.
- **OIDC sign-in:** sign in through the operator's identity provider (Authentik, Keycloak, Authelia, …) with the authorization-code flow and PKCE, run on the server; the callback ends in the same session as a local sign-in. Open: how an OIDC identity maps to a user (created on first sign-in, or only for users added on the console?), settings in `config.json` (issuer, client ID, client secret as an `env:`/`file:` reference), and dependencies (`golang.org/x/oauth2`, `github.com/coreos/go-oidc`) against "keep the list short".
- **OAuth 2.0 endpoints**, only if something outside Solstein needs to ask for tokens: a token endpoint with refresh tokens (kinds `access`/`refresh` on the same model), revocation (RFC 7009). Nothing needs them yet.
- **Roles:** every user can do everything today. If adding feeds (above) comes, a read-only role might be wanted.
- **Seeing and ending one's sessions** under Account (the token table has them: created, last used); today changing the password ends the others, and `reset-password` ends all.
- **Sign-in attempt limits survive a restart?** In memory today (`auth/limiter.go`); a restart forgets them, which only matters against someone who can also restart Solstein.

## Clients

- **Other clients** (e.g. Podcast Addict, AntennaPod, Pocket Casts via a public URL): whether they seek or resume with `Range` (matters for `stream` mode with dynamic ads), re-download changed enclosures (matters for "publish immediately"), or honour `itunes:new-feed-url`.
