# Architecture

Solstein is a podcast RSS proxy between podcast hosts (Acast first) and podcast clients. Audiobookshelf (ABS) is the primary client and the one tested against, but nothing may depend on ABS-specific behaviour: any client that can subscribe to an RSS URL should work (see [`clients.md`](clients.md)).

The proxy is the core. Features beyond it are modules that can be switched off independently:

| Module | Purpose | Depends on | Off when |
|---|---|---|---|
| Exits (VPN) — [`exits.md`](exits.md) | Fetch through other regions via embedded WireGuard: any WireGuard VPN, with Proton as a convenience | — | No usable VPN provider configured |
| Region diff — [`region-diff.md`](region-diff.md) | Strip dynamically inserted ads by comparing downloads from two regions | Two distinct exits (usually from the exits module) | `region_diff.exits` not set, or not two usable exits |

## Design rules

- The core builds, runs and is useful with every module off.
- The core defines the interfaces; modules implement them. The core never imports a module package; `main.go` is the only place modules are wired in.
- A disabled module costs nothing at runtime: no tunnels opened, no server list fetched, no double downloads.
- A module that can't run logs a warning and stays off; it doesn't stop start-up. (The exception is a `default_exit` that doesn't exist: see [`security.md`](security.md).)
- Modules can be enabled globally and overridden per feed.
- Solstein should ideally run on a private network only, but must be safe when exposed to the internet (see [`security.md`](security.md)).

## Core

| Package | Role | Document |
|---|---|---|
| `feeds` | Subscribing, polling, rendering the served feed, publish rules | [`feeds.md`](feeds.md) |
| `episodes` | Download pipeline, cache, serving audio, housekeeping | [`episodes.md`](episodes.md) |
| `outbound` | Every outgoing request: exits, guarded dialling | [`security.md`](security.md) |
| `server` | HTTP routes, access checks, feed API | [`feeds.md`](feeds.md), [`security.md`](security.md) |
| `rss` | Byte-preserving feed parsing and rewriting | [`feeds.md`](feeds.md) |
| `signing` | HMAC signatures for the URLs Solstein writes out | [`security.md`](security.md) |
| `database`, `models` | SQLite storage | below |
| `settings` | `config.json`, flags and environment variables | [`development.md`](development.md), README |
| `mp3` | MP3 frame reader, no Solstein dependencies | [`region-diff.md`](region-diff.md) |

## Extension points

### Exit providers (`outbound`)

The core asks `outbound.Manager` for an HTTP client by exit name. The core itself provides `direct`; the exits module registers more (`sweden`, `nordic`, …) through a `Provider`. All outbound traffic — feed polls, cache downloads, streams, processor downloads — goes through this, so the outbound safeguards live in one place.

A module supplies a **dialer**, not a ready-made HTTP client: if modules built their own clients, the core couldn't enforce the private-address block, timeouts, User-Agent or proxy handling on them. The core builds every client on top of the module's dialer.

```go
type Dialer interface {
    LookupIP(ctx context.Context, host string) ([]netip.Addr, error) // through the tunnel's DNS
    Dial(ctx context.Context, network string, address netip.AddrPort) (net.Conn, error)
}
type Provider interface {
    Exits() []string
    Dialer(exit string) (Dialer, error) // wraps ErrExitUnavailable when the tunnel is down
}
```

The dialer is looked up per connection, so a module can switch servers or restore a tunnel without the core rebuilding clients.

A provider can also implement `outbound.Locator`, saying which countries an exit may come out in and which one its next connection uses; `Manager.ExitCountries` / `ExitCountry` pass that on (`direct` is always unknown). Region diff uses it to avoid comparing two exits in the same country.

### Episode processors (`episodes`)

The core hands a processor a job and caches what it returns. The processor downloads the source through any exit via the job, so region diff needs nothing else from the core.

```go
type Processor interface {
    Name() string
    Handles(feed models.Feed) bool       // which feeds it applies to
    HideOnFailure(feed models.Feed) bool // failure policy: withhold, or publish unprocessed
    Recipe(feed models.Feed) string      // the settings that shape the output, with a version
    Process(ctx context.Context, job Job) (Processed, error)
}
// Job carries the feed, the episode, its stated duration and
// Fetch(ctx, exit), which downloads the source through an exit into memory
// with the pipeline's own download checks. Processed is the audio, its
// content type, its new duration (zero if unchanged) and a note.
```

- Errors are retried with the pipeline's back-off unless they wrap `episodes.ErrPermanent`.
- When a feed's `Recipe` changes, episodes processed with the old one are processed again ([`episodes.md`](episodes.md)).
- `feeds` can't import `episodes`, so `feeds.Options.Processed` (the processor's `Handles`) tells the feed side which feeds are processed, and `RegionDiffAvailable` / `ProcessBacklog` carry the module's settings it needs.
- At most one processor per feed; there is one processor (region diff).

How the pipeline runs a processor: [`episodes.md`](episodes.md).

## Storage

- SQLite through GORM on the CGO-free `modernc.org/sqlite` driver, in `solstein.db` in the config directory. Pragmas and the immediate-transaction rule: [`development.md`](development.md).
- Every table has a UUID primary key, assigned on create. No soft delete.
- Feeds are data, not configuration: they live in the database, not `config.json`.
- Tables: `feeds`, `episodes`, `feed_documents` (the last good source document per feed).
- Audio lives in `cache/{feedID}/{episodeID}.{ext}` in the config directory.

## Background work

`main.go` starts these loops; all stop with the signal context and are waited for before the database closes:
- `feeds.Poller` — polls feeds when due ([`feeds.md`](feeds.md)).
- `episodes.Pipeline` — download and processing workers, plus work started on request ([`episodes.md`](episodes.md)).
- `episodes.Housekeeper` — cache retention and stray files ([`episodes.md`](episodes.md)).
- `exits.Module` — closes idle tunnels, refreshes the Proton server list; only when the VPN module is on ([`exits.md`](exits.md)).

Start-up order: settings → time zone → logger → database → exits module → `outbound.Manager` → region diff → feeds service (and a check of every feed's settings against what is available) → cache and pipeline recovery → reconciling episodes with the settings → HTTP server.
