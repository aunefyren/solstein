# Exits (VPN) module

`modules/exits` adds exits beyond `direct`: routes out through WireGuard tunnels in other regions, used per feed and by region diff. **Any WireGuard VPN is supported:** plain WireGuard is the foundation, and provider integrations (Proton) are conveniences on top. Everything downstream of a provider — exits, location matching, server selection, health — works the same whatever the provider type.

The module plugs into the core as an `outbound.Provider` ([`architecture.md`](architecture.md)); it is off when `vpn.providers` is empty or none loads. Its configuration lives in `config.json` only: providers and exits are nested and list-shaped, which doesn't fit flags or environment variables. Plain data types are in `settings` (`settings.VPN`); validation is in the module (`exits.Load`), so the core doesn't depend on it.

## Configuration

```jsonc
"vpn": {
  "providers": {
    "proton": {
      "type": "protonvpn",
      "private_keys": ["env:PROTON_KEY_1", "env:PROTON_KEY_2"],
      "tier": "plus",                 // "free" limits the pool to free servers
      "max_tunnels": 2,               // defaults to the number of keys
      "fallback_dns": ["1.1.1.1", "9.9.9.9"],  // the default; [] turns it off
      "filter": {                     // optional; empty = everything the tier allows
        "countries": ["SE", "DE", "NL"],
        "cities": [],
        "servers": [],                // server names or hostnames, e.g. ["SE#12"]
        "secure_core": "exclude",     // exclude | include | only
        "tor": "exclude"
      }
    },
    "mullvad": {
      "type": "wireguard",
      "config_dir": "/app/config/wireguard/mullvad",
      "servers": {                    // location per .conf file (name without extension)
        "se-sto-wg-001": { "country": "SE", "city": "Stockholm" },
        "de-fra-wg-002": { "country": "DE", "city": "Frankfurt" }
      }
    },
    "vps": { "type": "wireguard", "config_file": "/app/config/wireguard/vps.conf", "country": "DE" }
  },
  "exits": {
    "sweden":  { "provider": "proton", "locations": ["SE"], "strict": true },
    "nordic":  { "provider": "proton", "locations": ["SE", "DK", "area:northern-europe"], "exclude": ["NO"] },
    "germany": { "provider": "mullvad", "locations": ["DE", "AT", "CH"], "selection": "sticky" },
    "europe":  { "provider": "proton", "locations": ["continent:europe"], "exclude": ["NO"] },
    "vps":     { "provider": "vps" }
  }
}
```

- A provider or exit with a problem is disabled on its own and reported at start-up; the rest keeps working.
- Private keys (and preshared keys) take `env:` / `file:` references as well as literals ([`security.md`](security.md)).
- Relative paths are resolved from the config directory. `UK` is accepted for `GB`.

## Providers

A provider is a pool of WireGuard servers plus the credentials to reach them.

### `wireguard` (generic)

Servers from standard wg-quick `.conf` files, as virtually every WireGuard VPN (and a self-hosted VPS) can export: `config_file`, `config_files` or `config_dir` (every `*.conf` in it; many providers offer a bulk download with one file per server).
- Read from the `.conf`: `[Interface]` `PrivateKey`, `Address` (IPv4/IPv6, several allowed), `DNS`, `MTU`; one `[Peer]` with `PublicKey`, `PresharedKey`, `Endpoint`, `AllowedIPs`, `PersistentKeepalive`. Host-network settings (`PreUp`, `PostUp`, `Table`, …) and DNS search domains are ignored with a warning: there is no host network to change. Errors never include key material.
- A `.conf` says nothing about location, so each server's `country` (and optionally `city`) is declared: `country`/`city` for a single `config_file`, `servers` by file name otherwise. A server without a location can only be used by name.
- `max_tunnels` caps concurrent tunnels; no limit by default.

### `protonvpn`

Credentials once, servers from Proton's published list:
- One WireGuard private key (from a config generated in Proton's dashboard; only the `PrivateKey` line matters) works on every Proton server. Each server gets address `10.2.0.2/32`, DNS `10.2.0.1`, port 51820 and its public key from the list. Every Proton config carries the same address, which blocks a second tunnel on routers; Solstein isn't affected, since each netstack tunnel is its own isolated network stack.
- `private_keys` is a list, and **each tunnel open at the same time needs its own key**: a Proton key works on one server at a time. `max_tunnels` defaults to the number of keys; set higher, it is limited to it, with a warning. Region diff's pair alone needs two keys, plus one for each exit used alongside them (a fallback exit, a feed's own exit). Proton's plan limits on simultaneous connections: Free 1, Plus 10.
- **Measured live on 2026-09-26** (one 4 KB request a second to NRK's CDN, with the tunnel's handshake time and byte counters sampled every 250 ms):
  - **What a stall looks like:** a session stops delivering. We send about 3 KB and get back only a 92-byte handshake response, until WireGuard re-handshakes after about 15 s without a reply. The new session works at once, so the handshake ends a stall rather than causing it.
  - **A key used by two tunnels at once:** 9 failed requests in 8 tunnel-minutes; the tunnels knocked each other out in turn. **A key each:** 1 in 8. **One key on one server, used nowhere else:** 479 requests in 8 minutes, none slow or failed, with handshakes every ~109 s.
  - **Moving a key between servers** (four servers, 3 minutes each, with another Solstein just stopped on the same key) also stalled on every server: 8 failures in 12 minutes. An earlier live check that one key holds several tunnels at once was too short to show any of this.
- **Key affinity:** a tunnel gets the least-used key. Among those, it prefers the key last used on the same server, then one not used on another server for 3 minutes (or never used), then the one that left another server longest ago. So keys stay on their servers, for example when region diff's fallback exit opens after the pair. A key moved sooner is logged ("key 2 moves from US-CO#147 to NO#23 after 40s; the new tunnel may stall now and then …"). The 3 minutes is a guess from WireGuard's 180-second session lifetime, not a measurement.
- `tier: "free"` limits the pool to free servers. `filter` narrows it by `countries`, `cities` or `servers` (Proton names like `SE#12`, or hostnames); Secure Core and Tor servers are excluded unless `secure_core` / `tor` is `include` or `only`.
- **Server list:** `pkg/servers/protonvpn.json` from `github.com/qdm12/gluetun-servers` (MIT, updated monthly). Only that file is embedded (gzip, 57 KB, in `modules/exits/data/` with gluetun-servers' licence; keep the attribution), not the Go module, which would embed every provider's list (8.7 MB). How to update the snapshot: `modules/exits/data/README.md`.
- **Refresh:** start-up uses the cached copy in `vpn/protonvpn.json` when it is valid and newer than the snapshot. A refresh a minute after start (unless the cache was checked within a day), then daily, fetches the list through the default exit with the core's safeguards. It is accepted only if it is format version 4, has at least 100 WireGuard servers and is newer; then it is saved atomically and the providers' servers rebuilt in place. Otherwise the last good copy (or the snapshot) stays.
- **Age warning:** after each refresh, if the list in use is more than 60 days old (gluetun's maintainers update it about monthly, so two updates missed), Solstein warns that newer servers can't be used and removed ones fail to connect, and says why no newer list came: a new format version (a newer Solstein is needed), a failing refresh, or gluetun-servers not having been updated.
- Proton's country names are mapped to ISO codes through the UN names plus 15 aliases (`Korea` → KR, `United Kingdom` → GB, …); all 148 countries in the snapshot map, checked by a test and against Proton's hostnames.
- Server names aren't unique in Proton's list (Secure Core routes reuse them), nor are hostnames; the IP is. A server's internal name is Proton's name, or `name@ip` where shared; `server:` locations and the `servers` filter still match Proton's name and hostname.
- The August 2026 snapshot: 1,048 WireGuard servers in 148 countries, each with country (English name), city, name, hostname, IP and public key, plus `free` (50 servers in 10 countries), `secure_core`, `tor`, `stream` and `port_forward` flags.

## Tunnels

- Embedded userspace WireGuard: `golang.zx2c4.com/wireguard` with `tun/netstack`, one tunnel per server. No gluetun sidecar, no `NET_ADMIN`, no `network_mode` coupling. Builds for every release platform including 32-bit ARM.
- **DNS goes through the tunnel**, to the config's `DNS` server; a tunnel without DNS refuses lookups rather than leak them to the host's resolver (a CDN that picks its edge by resolver location could otherwise serve the wrong region). Lookups retry after 1 s, then 2 s: the first query through a new tunnel is sometimes lost (seen with Proton), which would otherwise cost the resolver's 5-second timeout.
- **Fallback DNS:** when the tunnel's DNS server still hasn't answered (anything but an answer, "no such host" included, counts as failing), the provider's `fallback_dns` resolvers are asked in turn, 5 s each, over TCP through the same tunnel, so the lookup still leaves from the exit's region and nothing reaches the host's resolver. The default is 1.1.1.1 and 9.9.9.9; `[]` turns it off, and then a last attempt at the tunnel's own server gets whatever time the request has left, as before. The trade-off is that a name the VPN's resolver fails on is also sent to that third party. Answers are limited to the address families the tunnel has. TCP because a tunnel connection doesn't expose `net.PacketConn`, so Go's resolver would frame UDP queries as TCP.
  - Why: measured live on 2026-09-26, Proton's resolver (10.2.0.1) failed on NRK's CDN host `nrk-pod-pd.telenorcdn.net` (a four-step CNAME chain over nextra.no, AWS Route 53 and Oracle Cloud DNS, with TTLs under a minute at the end) in 9 of 9 lookups through a US server, over UDP and TCP alike, and intermittently on `podkast.nrk.no` through US and Norwegian servers. 1.1.1.1, 9.9.9.9 and 8.8.8.8 over TCP through the same tunnels answered all 48 lookups, in at most 5.4 s and mostly under 1.5 s. Names Proton had cached, such as `feeds.acast.com`, never failed.
- The VPN server's own endpoint name is resolved through the host (the tunnel isn't up yet), preferring IPv4.
- **Lifecycle:** a tunnel opens on first use and closes after **5 minutes idle**, so a configured but unused exit costs nothing. It counts its **users** — open connections, and the operation the pool handed it to — and is only idle once all are gone, so a long download is never cut. At `max_tunnels`, the least recently used idle tunnel is closed to make room; if every tunnel is busy, the request **waits up to a minute** for one to free up (`tunnelWait`, checked every 100 ms) and only then fails with `ErrTunnelLimit`, rather than cut a transfer. Waiting is what the caller usually wants: the tunnels in use are serving downloads that end, and the caller is often one half of a region-diff pair, where failing at once compares another market or fails the episode. All tunnels close on shutdown.
- **One key is enough with `region_diff.pair_downloads` in turn** ([`region-diff.md`](region-diff.md)): the pair is then downloaded through one exit after the other, so only one tunnel is ever open, and `auto` chooses that by itself when the keys are short. It costs time rather than keys, and wants `prepare_ahead` so no client waits for it ([`episodes.md`](episodes.md)).
- **How many tunnels a setup needs: one per exit that can be in use at the same moment.** A tunnel belongs to a server, so every episode going out through one exit shares its tunnel: the count doesn't grow with how many episodes are being prepared. What does add up is the exits in play at once — region diff's pair, each `fallback_exits` entry (tried in turn while the pair's tunnels are still open), `default_exit` for feed polls and the server-list refresh, and any feed's own exit. At start-up Solstein adds them up per provider and **warns with the number** when the provider can't hold them all: "provider 'proton' can hold 3 tunnels at once (one per private key), but 4 of its exits can be in use at the same moment: … Add 1 more private key". Short of tunnels, region diff also **prepares one episode at a time** ([`region-diff.md`](region-diff.md)), since two attempts would close and reopen each other's tunnels and move keys between servers.
- **An unusable exit is not the destination's fault:** when a request fails with `ErrExitUnavailable` (no tunnel free after the wait, no server, an unknown exit), the core neither retries it on a new connection nor unwraps the next tracking redirect — the next host would be asked through the same exit and fail the same way. Before that, one short tunnel budget walked a whole Podtrac chain, a request and a log line per tracker.
- **A tunnel is never written to after it is closed.** `pool.get` hands a tunnel over **already in use**, and `exitDialer.attempt` gives that use back when the handshake and the operation are done, so nothing can close the tunnel between the pool handing it out and the first packet. Closing (a failed server, an idle tunnel, shutdown) marks the tunnel shut at once, so no new user starts and the pool forgets it, but the WireGuard device is closed by the **last user to leave**. Writing into a closed device is not an error but fatal: netstack's `Close` shuts the channel outgoing packets go into, and the next packet panics the process. That is what happened live on 2026-09-26, when a burst of region-diff episodes ran the three-key pool out of tunnels: `awaitHandshake` was the one operation that wrote without counting as a user, so the pool closed the tunnel it had just handed out ("Closing idle tunnel to DE#187 to make room for NO#23") while the handshake packet was going into it, and Solstein panicked with `send on closed channel` in `netTun.WriteNotify`. Covered by tests for the handshake on a shut tunnel, the pool refusing to evict a tunnel it has handed over, and a close racing a handshake under `-race`.
- **Handshake before use:** a new tunnel sends one throwaway packet (to TEST-NET-1, discard port) to trigger the WireGuard handshake and waits up to 6 s for it (WireGuard resends a lost handshake after 5).
- The core's private-address block applies inside tunnels too.

## Exits

An exit is a named route that feeds and region diff refer to; it picks servers from one provider by location.
- **`locations`** is an ordered preference list: country (`SE`), city (`SE/Stockholm`), server (`server:SE#12`), area (`area:northern-europe`) or continent (`continent:europe`, plus `north-america` and `south-america`). Omitted means anywhere the provider allows. Country codes are ISO 3166-1 alpha-2. The country and region tables are generated from the UN M49 table (`countries_table.go`, 250 entries including TW and XK, which M49 omits); areas are M49 sub-regions and intermediate regions, continents are M49 regions plus the two American aliases. Unknown codes and names are rejected when the config loads, listing the valid ones.
- **Strict vs loose:** `strict: true` uses only the first location, so the exit is unavailable when nothing there is healthy — right when the location is the point, as for region diff. Loose (default) works down the list.
- **`exclude`** lists countries never to use, whatever `locations` says.
- **Tiers:** servers are grouped by the first location they match. The exit keeps its current server while it is healthy **and** in the best tier that has a healthy server, so it moves back to a preferred location once that recovers.
- **`selection`** of a replacement: `sticky` (default: the first by name, kept until it fails — keeps the ad market stable and avoids reconnecting), `random` (rotates hourly), or `least-failed` (fewest failures in the last hour).
- **Health:** a failing server is benched for 1 minute, doubling per failure in a row up to 30 minutes; a success resets it. A failure only counts against the server when the tunnel is at fault: a failed connection over a tunnel with a fresh handshake is the destination's problem and benches nothing. No test requests to third-party sites are made.
- **Failover:** each lookup or dial tries the chosen server, then one replacement, so a dead server is failed over within the same request (about 6 s).

Where exits are used:
- **Per feed:** a feed's `exit` (default `default_exit`, else `direct`) carries its polls, downloads and streams: another region's ads, or geo-blocked feeds, without region diff.
- **Region diff:** a pair of exits, home region first ([`region-diff.md`](region-diff.md)). The module implements `outbound.Locator` for it: `ExitCountries` lists every country an exit's servers are in (all tiers, after `exclude` and the provider's filter; unknown if any server has no location), and `ExitCountry` the country of the server the exit would use now.
- **Wiring:** `exits.Setup` loads the block, reads the `.conf` files, builds the module and returns warnings; `main.go` logs them, registers the module with `outbound.Manager` and runs it with the other background loops. Tested end to end: a feed on a host that exists only inside a WireGuard tunnel was subscribed, polled and rendered through an exit, and was unreachable through `direct`.

## Testing

- Tunnel tests run a real WireGuard peer inside the test process (its own netstack device on a local UDP port, with a web server and DNS server reachable only through the tunnel): handshake, DNS, HTTP, a wrong peer key failing cleanly, a DNS server that drops the first query, and one that never answers, with a DNS server over TCP at a second address inside the peer as the fallback. Pool tests use a fake opener.
- **Live tests** (`live_test.go`, build tag `live`) run against real Proton servers, ifconfig.co and Acast, in a Go container started with `--env-file .env` ([`development.md`](development.md)); never in CI. The maintainer's Proton keys are in `.env` at the repository root (`PROTON_KEY_1`, `PROTON_KEY_2`, generated for Solstein with NetShield off), which is ignored by git and excluded from the Docker build context. It is referenced only by path and through `env:` references, never opened or printed.

Live results (2026-09-25):
- Requests through a `SE` exit left from a Proton IP (M247 Europe) that ifconfig.co geolocates to SE; direct requests showed NO. Handshakes took 26–53 ms on every server tried (SE, DE, NL, free and plus).
- **One key on two tunnels at once** worked in a short check: with a single key, tunnels to SE and DE worked side by side, and six interleaved requests all left from the right country. A longer measurement (above, 2026-09-26) showed such tunnels taking turns stalling within a minute or two, so each tunnel now gets its own key. Proton gave the same DE server a different exit IP per session.
