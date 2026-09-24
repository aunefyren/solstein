# Development

How to write code in Solstein: layout, conventions, dependencies, build, test and release. Architecture and product decisions live in `docs/design.md`; this file covers *how* to build it. Much of it is borrowed from Pønskelisten (`github.com/aunefyren/poenskelisten`) so the two projects feel the same to work on.

Anything marked **(proposed)** has not been agreed yet. Confirm it, or move it to `docs/design.md` as an open question, before relying on it.

## Toolchain

- Go 1.26, module path `aunefyren/solstein`.
- Single static binary: `CGO_ENABLED=0` everywhere, including local builds and Docker. Any dependency that needs CGO is out (so SQLite, if used, goes through `modernc.org/sqlite`).
- Target platforms: `linux/amd64`, `linux/arm64`, `linux/arm/v7` (matching Pønskelisten's image), plus native builds for local development on Windows/macOS.

## Repository layout

```
main.go            wiring only: resolve config, init logger, build core, register enabled modules, run server
settings/          Config struct, config.json load/save, flag + env overrides      (exists)
logger/            logrus wrapper, logger.Log                                      (exists)
server/            Gin router, access checks, feed routes, feed API               (exists)
models/            persisted records (Base with UUID ID, Feed, Episode) and their GORM mapping (exists)
feeds/             core: polling, parsing, rewriting feeds
episodes/          core: download pipeline, cache, serving audio (cache, stream, tee)  (exists)
database/          SQLite via GORM: Store with named query functions, one file per model   (exists)
rss/               feed parsing and byte-preserving rewriting; no Solstein dependencies   (exists)
feeds/             core: source URLs, subscribe, refresh, render (publish rules, signed URLs)   (exists)
signing/           HMAC signatures for feed and episode URLs                               (exists)
outbound/          core: exit Manager, direct exit, guarded dialling (private-address block) (exists)
modules/exits/     module: config, .conf parsing, netstack tunnels, pool, geography, server selection and health (exists); wiring into main (next)
modules/exits/wireguard/  generic provider: servers from wg-quick .conf files
modules/exits/proton/     server-list provider for Proton VPN (gluetun-servers data)
modules/regiondiff/     module: dual download, diff engine, cutting
mp3/               MP3 frame parsing and splicing, no Solstein dependencies
utilities/         small shared helpers
docs/              design.md, development.md, wip.md
config/            default local config directory (config.json, database, log, cache); gitignored. /app/config in Docker
Dockerfile, entrypoint.sh, .github/workflows/   packaging and CI                  (exists)
```

Packages marked "exists" are in the skeleton; the rest are the planned layout.

### Layering rules

- Dependencies flow one way: `main.go` → `server` → core (`feeds`, `episodes`) → `database` → `models`. Handlers never talk to the database directly.
- **The core never imports anything under `modules/`.** The core defines the interfaces it needs (for example an exit provider that hands out an `*http.Client` for a named exit, and an episode processor hook). Modules implement them, and `main.go` wires the enabled ones in based on config.
- Modules may import the core and each other only along declared dependencies (`regiondiff` → `exits`, never the reverse).
- A disabled module is never constructed: no goroutines, no tunnels, no network calls.
- Low-level packages with no Solstein knowledge (`mp3`) import nothing from the rest of the repo, so they can be tested and reasoned about in isolation.
- Every package has a package doc comment on one file explaining its role.

## Dependencies

Keep the list short; every new dependency needs a reason.

| Purpose | Package | Status |
|---|---|---|
| HTTP router | `github.com/gin-gonic/gin` | In use |
| Logging | `github.com/sirupsen/logrus` + `github.com/t-tomalak/logrus-easy-formatter` | In use |
| WireGuard | `golang.zx2c4.com/wireguard` incl. `tun/netstack` | Agreed |
| WireGuard (in use) | `golang.zx2c4.com/wireguard` `device` + `tun/netstack` (pulls in gVisor's network stack); `golang.org/x/crypto/curve25519` for public keys | In use |
| VPN server lists | `github.com/qdm12/gluetun-servers` (MIT; embedded snapshot, refreshed at runtime) | Proposed |
| Database | `gorm.io/gorm` + `gorm.io/driver/sqlite` on the CGO-free `modernc.org/sqlite` connection, as Pønskelisten | In use |
| IDs | `github.com/google/uuid` | In use |
| Feed rewriting | Own `rss` package on `encoding/xml` `RawToken` + byte offsets; `golang.org/x/text/encoding/charmap` for Latin-1/Windows-1252 feeds | In use |
| MP3 frames | Own `mp3` package | Proposed |

Notes:
- **Feed rewriting:** the proxied feed must keep every element and namespace the source had (`itunes:`, `podcast:`, `acast:` …). Unmarshalling into structs drops what we didn't model, and `encoding/xml`'s encoder rewrites namespace prefixes — which breaks ABS, since it looks elements up by literal prefix (`itunes:new-feed-url`). So `rss` copies the **original bytes** through and splices in only the changed values (enclosure `url`/`length`, `media:content`/`podcast:source` URLs, `itunes:duration`, self-links), using the decoder's byte offsets. A rewrite with no changes is byte-identical to the input; a test enforces it. Elements are matched by namespace URI, not prefix. Non-UTF-8 feeds (ISO-8859-1/15, Windows-1252) are converted to UTF-8 first and the declaration updated.
- **MP3:** frame parsing (sync word, header, frame length, Xing/LAME/ID3 handling) is small enough to own, and the diff engine needs exact byte-level control. Evaluate an existing library only if we end up needing decoding for audio-level alignment.
- Run `go mod tidy` after adding or removing imports; commit `go.sum`.

## Code conventions

- **camelCase, not snake_case**, for locals, parameters and unexported fields. Exported names are PascalCase. Acronyms stay uppercase as one unit: `episodeID`, `feedURL`, `httpClient`, `wgPrivateKey` — not `episodeId`/`feed_url`/`HttpClient`.
- **Short, clear names** over abbreviations, except the Go-standard ones (`ctx`, `err`, `req`, `resp`).
- **Errors:**
  - Return errors; no `panic`, `log.Fatal` or `os.Exit` outside `main`. Start-up errors bubble up to `run()`, which logs them and returns the exit code.
  - Wrap with context using `%w`: `fmt.Errorf("fetch feed %s: %w", feedURL, err)`.
  - "Not found" and "failed" must stay distinguishable: lookups that match nothing return a package-level sentinel (`ErrFeedNotFound`, `ErrEpisodeNotFound`, `ErrExitUnavailable` …), checked with `errors.Is`. Never an ad-hoc `errors.New` at the call site.
- **Handler shape** (Gin), as in Pønskelisten: log first, then respond, then abort and return.
  ```go
  logger.Log.Error("Failed to load episode. Error: " + err.Error())
  context.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load episode."})
  context.Abort()
  return
  ```
  `400` for caller mistakes, `404` for unknown feed/episode IDs, `500` for internal failures. Internal error text goes to the log, never to the client.
- **Serving audio:** use `http.ServeContent` (via `context.Writer`/`context.Request`) for cached files. It handles `Range`, `If-Range`, `HEAD` and `Content-Length` correctly; don't reimplement range parsing.
- **Context and cancellation:** every network call and long-running job takes a `context.Context` and respects cancellation. No `http.Get`/`http.DefaultClient` — always a client from `outbound.Manager.Client(exit)` (the `direct` exit included) so traffic never leaves through an unintended route and the safeguards always apply. Those clients have no overall timeout (episode downloads are large); bound each request with a context deadline.
- **Concurrency:** goroutines are owned by something that can stop them (a context plus `sync.WaitGroup` or `errgroup`). Shared state is guarded by a mutex or owned by a single goroutine; `go test -race` must stay clean.
- **Secrets:** WireGuard private keys and similar never appear in logs, errors or API responses. Redact them in any config dump.
- **Comments explain *why*, not *what*.** Use them for non-obvious constraints (why the diff runs on frames, why a header is stripped), not to restate code.
- **gofmt is enforced.** Run `gofmt -w .` before handing work over.

## Access control

- Three checks, in `server/access.go`: the **client network** check (every route but `/api/health`), the **subscribe token** (the `/api/rss/{token}/…` prefix route and the `/api/v1` feed API), and **URL signatures** (the feed and episode URLs Solstein writes out).
- Tokens are compared with `subtle.ConstantTimeCompare`. Signatures are HMAC-SHA256 over the canonical path (`signing` package), URL-safe base64 in the `sig` query parameter; the handler checks the request path equals the canonical path, so a differently spelled path can't reuse a signature.
- `disable_auth` turns off token and signature checks (the client network check stays). With it on, the token segment of the prefix route is optional.
- `X-Forwarded-For` is only believed from `trusted_proxies` (via Gin's `SetTrustedProxies`), and `X-Forwarded-Proto`/`-Host` only from them too (for building links when `external_url` is unset).
- **Never log secrets.** The request logger logs the path only (never the query string, which carries signatures and API tokens) and redacts the prefix route's token segment. Source URLs are logged without their query string, since private feeds often carry an access token there.

## Secrets

- Any secret field in `config.json` may be a literal, `env:NAME` or `file:PATH`; `settings.ResolveSecret` turns it into the value. `config.json` keeps the reference.
- Errors about secrets name the variable or file, never the value. A resolved key is held in a type that prints as `[redacted]` (`exits.Key`); tests assert keys never appear in `%v`, `%+v`, `%#v` or JSON output.
- Never read, print or copy the maintainer's secret files (`.env`) while working; refer to them by path or reference only.

## Outbound requests

- `outbound.Manager` is the only source of HTTP clients for outgoing requests. `main.go` builds it; code that fetches takes it (or a client from it) as a dependency.
- Every client, for every exit, is built in `outbound` on top of the exit's `Dialer`. It:
  - resolves the host through the exit and checks **each resolved IP** before dialling, refusing loopback, private, link-local, CGNAT, documentation, multicast and reserved ranges (including IPv4-mapped, NAT64 and 6to4 forms) unless `allow_private_destinations` is on. Redirects are dialled the same way, so a public URL that redirects inward is refused too;
  - allows only `http` and `https`, at most 10 redirects;
  - ignores `HTTP_PROXY`/`HTTPS_PROXY`, which would carry traffic out of another route;
  - sets `Solstein/<version> (+https://github.com/aunefyren/solstein)` as User-Agent unless the request sets one;
  - has dial, TLS-handshake and response-header timeouts, but no overall timeout.
- Errors to branch on with `errors.Is`: `outbound.ErrUnknownExit`, `ErrExitUnavailable`, `ErrDestinationBlocked`.
- Tunnel tests in `modules/exits` run a real WireGuard peer inside the test process (its own netstack device on a local UDP port, with a web server and a DNS server reachable only through the tunnel), so the handshake, DNS and HTTP paths are tested for real without root or network setup. Pool tests use a fake opener.
- Tests use a fake `Dialer` that resolves made-up hostnames to chosen IPs and connects to a local `httptest` server, so public/private behaviour is tested without real network.

## Background work

- `main.go` starts three long-running loops, all stopped by the signal context and waited for before the database closes: `feeds.Poller.Run`, `episodes.Pipeline.Run` and `episodes.Housekeeper.Run`.
- Loops take a clock (`Now func() time.Time`) and expose a single-step function (`Poller.PollDue`, `Pipeline.ProcessNext`, `Housekeeper.Sweep`), so tests drive them deterministically instead of waiting on timers.
- The poller wakes the pipeline through a callback (`pipeline.Wake`), so `feeds` doesn't import `episodes`.
- On shutdown, in-flight downloads are abandoned; `Pipeline.Recover` at the next start resets them and removes `.part` files.

## Database

- `database.Open(configDir)` opens `solstein.db` in the config directory and auto-migrates every model. The connection is opened with `modernc.org/sqlite` and handed to GORM's SQLite dialector, so the binary stays CGO-free (`CGO_ENABLED=0 go build` must keep working).
- Pragmas are set in the DSN so every pooled connection gets them: `busy_timeout(5000)`, `journal_mode(WAL)`, `foreign_keys(1)`, plus `_txlock=immediate`.
- **Transactions start with the write lock** (`_txlock=immediate`). Most transactions read then write (check for a duplicate, then insert); in SQLite's default deferred mode a transaction that has read can't wait to become a writer and fails at once with `SQLITE_BUSY`, whatever `busy_timeout` says. A concurrency test (`TestClaimNextEpisodeConcurrent`) catches regressions.
- **No database global.** `main.go` opens a `*database.Store` and passes it to what needs it. (Pønskelisten's `database.Instance` global is what forces its tests to share and restore state.)
- **Only `database` imports `gorm.io/*`.** Everything else calls named methods on `Store` (`CreateFeed`, `GetEpisode`, …), one file per model.
- Every persisted model embeds `models.Base`: a UUID primary key assigned by a `BeforeCreate` hook, plus timestamps. Never set IDs by hand at call sites. There is **no soft delete**: a removed feed must be re-addable under the same source URL, which a soft-deleted row would block through the unique index.
- Lookups that match nothing return the package's sentinel (`ErrFeedNotFound`, `ErrEpisodeNotFound`); duplicate inserts return `ErrFeedExists` / `ErrEpisodeExists`, checked in a transaction rather than by parsing driver-specific constraint errors.
- `Update*` functions save every field (zero values included, so a per-feed override can be cleared) and return the not-found sentinel instead of inserting. Exception: `UpdateEpisode` never writes `released_at` — only `MarkReleased` does — because a download or stream holding a copy loaded before the release would otherwise clear it.
- Every query takes a `context.Context`.
- Tests open a fresh file database in `t.TempDir()` (not `:memory:`, which gives each pooled connection its own empty database).

## Configuration

Implemented in the `settings` package. **`config.json` in the config directory is the source of truth.** Flags and environment variables are ways to change it: at start-up they are applied on top of the file and the result is written back, so the file always holds the configuration Solstein is running with.

When both are given, the flag wins over the environment variable (`-port 9000` over `SOLSTEIN_PORT=9100`). An empty env var counts as unset.

Rules:
- Start-up order: `Load` (read the file, fill defaults) → apply env vars and flags → `Validate` → `Save`. Validation runs before saving, so a bad flag or env var stops start-up and leaves `config.json` untouched.
- Removing a flag or env var does not revert its value; the saved value stays until it is changed in the file or overridden again.
- `config.json` is created on first run and rewritten only when its bytes would change (new fields filled in with defaults, or an override that differs from the file). Writes are atomic (temp file + rename) and the file is `0600`, since it will hold WireGuard keys.
- Defaults live in one place, `Config.applyDefaults`. `Config.Validate` runs after overrides, so a bad flag or env var stops start-up with a clear error.
- **Adding a setting:** add the field to `Config` (snake_case JSON tag), a default in `applyDefaults` if it needs one, a check in `Validate`, and one entry in the `settings` table in `settings/flags.go`. That entry declares the flag and env var together, so they can't drift apart, and the help text lists both. Flag names are lowercase without separators (`externalurl`), env vars are `SOLSTEIN_` plus upper snake case (`SOLSTEIN_EXTERNAL_URL`).
- **Time zone:** `timezone` / `-timezone` / `SOLSTEIN_TIMEZONE`, an IANA name. Empty means the system zone, so Docker's standard `TZ` also works. `main` sets `time.Local` from it before the logger starts; the binary embeds `time/tzdata`, so it works on hosts without a zone database. An unknown name fails validation rather than silently falling back (Pønskelisten resets to Europe/Paris).
- Process-level options that can't live in `config.json` go in `Startup`: `-configdir` / `SOLSTEIN_CONFIG_DIR` (default `./config` locally, relative to the working directory; `/app/config` in Docker) and `-version`.
- The resolved `Config` is a value passed explicitly to what needs it (`server.New(cfg, …)`); there is no mutable package-level config global, so tests don't need to restore shared state.
- Each module gets its own nested block with an `enabled` switch when it is built; the exits module is also off whenever no VPN provider is configured. Per-feed overrides live with the feed's config.


## Logging

- `logger.Log` (logrus) writes to stdout and `solstein.log` in the config directory, with the easy-formatter layout `[%lvl%]: %time% - %msg%`. Before `logger.Init` runs it writes to stdout at info, so it is always safe to use.
- `logger.Init` returns an error instead of exiting, and the log file is `0640`.
- Log level from config (`log_level` / `-loglevel` / `SOLSTEIN_LOG_LEVEL`), default `info`.
- Gin's own logger is not used (`gin.New`, not `gin.Default`); the `requestLogger` middleware logs requests through logrus: `debug` for success, `warn` for 4xx, `error` for 5xx.
- `info` for lifecycle events (feed polled, episode processed, tunnel up/down); `debug` for per-request and per-segment detail; `warn` for degraded but working states (exit unavailable, diff skipped); `error` for failures.
- Log messages are full sentences starting with a capital letter and naming the object: `"Failed to poll feed 'Example Show'. Error: ..."`.
- Errors before the logger exists (config failures) go to stderr with `fmt.Fprintln`.


## Build and run

```
go build ./...                 # compile everything
go run .                       # run locally; serves on :8080, config in ./config
go run . -h                    # list every flag and its env var
go vet ./...                   # must be clean
gofmt -l .                     # must print nothing; gofmt -w . to fix
go mod tidy                    # keep go.mod/go.sum in sync (CI checks this)
```

Release-style local build:
```
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=v1.0.0" -o solstein .
```

**Versioning:** `main.version` defaults to `dev` and is set at build time with `-ldflags "-X main.version=<tag>"` (the Dockerfile's `VERSION` build argument, the release workflow's tag). It is never written to `config.json` or into source files. (Pønskelisten `sed`s a placeholder in `config.go` and has a workflow that force-pushes rewritten tags; neither is needed here.)

**Start-up and shutdown:** `main` calls `run()` and exits with its return code, so deferred cleanup runs. The HTTP server has a `ReadHeaderTimeout` but deliberately no `WriteTimeout` (large, slow episode downloads), and shuts down gracefully on SIGINT/SIGTERM with a 30-second grace period. `/api/health` reports `{"status":"ok","version":…}` for health checks.


## Testing

Commands:
```
go test ./...                                   # whole module
go test ./mp3/...                               # one package
go test ./modules/regiondiff/... -run TestDiff -v    # one test, verbose
go test -race -covermode=atomic -coverprofile=coverage.out ./...   # what CI runs
go tool cover -func=coverage.out | tail -1      # total coverage
```

Conventions:
- Tests are colocated: `foo.go` → `foo_test.go`, same package (white-box), so internal helpers are tested directly.
- Table-driven tests (`cases := []struct{...}{...}`) for pure functions: frame header parsing, duration calculation, feed rewriting, country filtering.
- **No real network in tests.** Use `httptest.Server` for podcast hosts and the gluetun server list; inject HTTP clients rather than reaching for globals. Tests must pass offline and in CI.
- Tunnels are tested behind the exit interface with a fake exit that returns an `httptest` client. Anything that genuinely needs a live WireGuard endpoint is an integration test behind a build tag (`//go:build integration`) and never runs in CI.
- **Audio fixtures** live in `testdata/` next to the test. Keep them tiny (a few seconds, generated or openly licensed — never downloaded episodes of real shows). Include cases for: identical files, one inserted segment, segments at start/middle/end, different ID3 tags, and truncated/corrupt frames.
- **Feed fixtures**: real-world-shaped RSS (with `itunes:`/`podcast:` namespaces) in `testdata/`, anonymised. Rewriting tests assert that everything except the fields we change survives byte-for-byte or element-for-element.
- Prefer real dependencies over hand-rolled fakes when they're cheap (in-memory SQLite, real files in `t.TempDir()`).
- Use `t.TempDir()` for anything written to disk; never write into the repo during tests.

## CI

`.github/workflows/`:

- `go.yml` — on push and pull request to `main` and `dev`: gofmt check, `go mod tidy` check, build, vet, `go test -race` with coverage in the job summary, the coverage badge (push to `main` only) and, last, the coverage gate. The gate's minimum is `COVERAGE_MIN` in that file (65% while the total is ~70%); raise it as the suite grows, never lower it to get a change through.
- `codeql-analysis.yml` — CodeQL with the `security-extended` queries, on push/PR to `main` and `dev` and weekly.
- `docker-image-beta.yml` — on push to `main`: multi-arch image `ghcr.io/aunefyren/solstein:beta`, version `beta-<sha>`.
- `docker-image.yml` — on a published release: multi-arch image tagged with the release and `latest`, on GHCR only (no Docker Hub).
- `release.yaml` — on a published release: binaries for linux/windows/darwin, version stamped via ldflags.

Secrets and variables:
- `GIST_TOKEN` (secret, personal access token with the `gist` scope) and `COVERAGE_GIST_ID` (repository variable) for the coverage badge. The badge step skips itself until both exist, so CI stays green before it is set up. The badge in `README.md` reads gist `28cb38a6289c7b2b21694175a243e7eb`; if the gist ever changes, update both the variable and the README URL.
- GHCR and release uploads use the built-in `GITHUB_TOKEN` (the workflows request `packages: write` / `contents: write`); no other secrets are needed.


## Docker

- Multi-stage build. The builder runs on `$BUILDPLATFORM` and cross-compiles with `CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v}`, so multi-arch images don't compile under QEMU. Platforms: `linux/amd64`, `linux/arm64`, `linux/arm/v7`.
- Runtime on Alpine with `ca-certificates`, `tzdata` and `su-exec`. `SOLSTEIN_CONFIG_DIR=/app/config`, declared as a volume: config, database, log and cache all live there.
- `entrypoint.sh` only handles privileges: when started as root it fixes ownership of the config directory and drops to `PUID`/`PGID` (default `1000`); started as non-root it runs as-is. A non-flag first argument runs that command instead (`docker run -it <image> sh`). Settings come from `SOLSTEIN_*` env vars, which the binary reads itself, so the entrypoint doesn't map each one to a flag (Pønskelisten's entrypoint does, and every new setting had to be added in two places).
- `/app/config` is created in the image owned by `1000:1000`. Docker copies an image directory's ownership into a fresh named volume on first mount, so a root-owned `/app/config` would make `user: "1000:1000"` fail with "permission denied" on a new volume.
- `entrypoint.sh` sets `umask 027`, so the database, log and cache aren't world-readable (the database holds feed URLs, which can carry private-feed tokens). `config.json` is `0600` regardless.
- Tested with Docker Desktop (2026-09-24): single- and multi-arch builds (amd64, arm64, arm/v7; the ARM images run), privilege drop to default and custom `PUID`/`PGID`, `user: "1000:1000"` on a fresh volume, env vars and flags reaching `config.json`, the token surviving a restart, a real Acast subscription from inside the container, `docker stop` in about half a second. Image size about 55 MB.
- `.gitattributes` forces LF for `*.sh`, since a CRLF checkout on Windows would break the entrypoint inside the container.
- No `NET_ADMIN`, no `/dev/net/tun`, no `network_mode` — the WireGuard tunnels are userspace and in-process. If a change seems to need any of these, it's the wrong change.
- Image: `ghcr.io/aunefyren/solstein` (GHCR only).


## Working notes

- `docs/wip.md` (create when needed) tracks known bugs and deliberate gaps not yet fixed. Check it before assuming behaviour is intended, and remove entries once fixed.
- Git is managed by the maintainer; don't commit, push or branch.
