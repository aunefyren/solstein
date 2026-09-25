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
- `private_keys` is a list; each opening tunnel takes the least-used key, and `max_tunnels` defaults to the number of keys, so by default every tunnel has its own. One key can hold several tunnels at once (verified live, below), so `max_tunnels` may exceed the number of keys. Proton's plan limits on simultaneous connections: Free 1, Plus 10.
- `tier: "free"` limits the pool to free servers. `filter` narrows it by `countries`, `cities` or `servers` (Proton names like `SE#12`, or hostnames); Secure Core and Tor servers are excluded unless `secure_core` / `tor` is `include` or `only`.
- **Server list:** `pkg/servers/protonvpn.json` from `github.com/qdm12/gluetun-servers` (MIT, updated monthly). Only that file is embedded (gzip, 57 KB, in `modules/exits/data/` with gluetun-servers' licence; keep the attribution), not the Go module, which would embed every provider's list (8.7 MB). How to update the snapshot: `modules/exits/data/README.md`.
- **Refresh:** start-up uses the cached copy in `vpn/protonvpn.json` when it is valid and newer than the snapshot. A refresh a minute after start (unless the cache was checked within a day), then daily, fetches the list through the default exit with the core's safeguards. It is accepted only if it is format version 4, has at least 100 WireGuard servers and is newer; then it is saved atomically and the providers' servers rebuilt in place. Otherwise the last good copy (or the snapshot) stays.
- Proton's country names are mapped to ISO codes through the UN names plus 15 aliases (`Korea` → KR, `United Kingdom` → GB, …); all 148 countries in the snapshot map, checked by a test and against Proton's hostnames.
- Server names aren't unique in Proton's list (Secure Core routes reuse them), nor are hostnames; the IP is. A server's internal name is Proton's name, or `name@ip` where shared; `server:` locations and the `servers` filter still match Proton's name and hostname.
- The August 2026 snapshot: 1,048 WireGuard servers in 148 countries, each with country (English name), city, name, hostname, IP and public key, plus `free` (50 servers in 10 countries), `secure_core`, `tor`, `stream` and `port_forward` flags.

## Tunnels

- Embedded userspace WireGuard: `golang.zx2c4.com/wireguard` with `tun/netstack`, one tunnel per server. No gluetun sidecar, no `NET_ADMIN`, no `network_mode` coupling. Builds for every release platform including 32-bit ARM.
- **DNS goes through the tunnel**, to the config's `DNS` server; a tunnel without DNS refuses lookups rather than leak them to the host's resolver (a CDN that picks its edge by resolver location could otherwise serve the wrong region). Lookups retry after 1 s, then 2 s: the first query through a new tunnel is sometimes lost (seen with Proton), which would otherwise cost the resolver's 5-second timeout.
- The VPN server's own endpoint name is resolved through the host (the tunnel isn't up yet), preferring IPv4.
- **Lifecycle:** a tunnel opens on first use and closes after **5 minutes idle**, so a configured but unused exit costs nothing. It counts its open connections and is only idle once all are closed, so a long download is never cut. At `max_tunnels`, the least recently used idle tunnel is closed to make room; if all are busy, the request fails with `ErrTunnelLimit` rather than cut a transfer. All tunnels close on shutdown.
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
- **Region diff:** a pair of exits, home region first ([`region-diff.md`](region-diff.md)).
- **Wiring:** `exits.Setup` loads the block, reads the `.conf` files, builds the module and returns warnings; `main.go` logs them, registers the module with `outbound.Manager` and runs it with the other background loops. Tested end to end: a feed on a host that exists only inside a WireGuard tunnel was subscribed, polled and rendered through an exit, and was unreachable through `direct`.

## Testing

- Tunnel tests run a real WireGuard peer inside the test process (its own netstack device on a local UDP port, with a web server and DNS server reachable only through the tunnel): handshake, DNS, HTTP, a wrong peer key failing cleanly, a DNS server that drops the first query. Pool tests use a fake opener.
- **Live tests** (`live_test.go`, build tag `live`) run against real Proton servers, ifconfig.co and Acast, in a Go container started with `--env-file .env` ([`development.md`](development.md)); never in CI. The maintainer's Proton keys are in `.env` at the repository root (`PROTON_KEY_1`, `PROTON_KEY_2`, generated for Solstein with NetShield off), which is ignored by git and excluded from the Docker build context. It is referenced only by path and through `env:` references, never opened or printed.

Live results (2026-09-25):
- Requests through a `SE` exit left from a Proton IP (M247 Europe) that ifconfig.co geolocates to SE; direct requests showed NO. Handshakes took 26–53 ms on every server tried (SE, DE, NL, free and plus).
- **One key holds two tunnels at once:** with a single key, tunnels to SE and DE worked side by side, the first kept working after the second came up, and six interleaved requests all left from the right country. Two keys behave the same. Proton gave the same DE server a different exit IP per session.
