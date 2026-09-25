# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Solstein — a self-hosted Go podcast RSS proxy that sits between podcast hosts (Acast first) and an Audiobookshelf instance. The core fetches and polls source feeds, rewrites them and serves them to Audiobookshelf. Everything else is an optional module that can be switched off:

- **Exits (VPN):** embedded userspace WireGuard tunnels that let Solstein fetch through other regions. If no VPN is configured, the module is off and all traffic goes out directly.
- **Region diff:** strips dynamically inserted ads (DAI) by downloading each episode through two exits and removing the audio that differs. It depends on the exits module.

The name is the Viking sunstone (Iceland spar), which shows everything twice through double refraction — a nod to the region-diff module. Licence: GPL-3.0.

**Status: core proxy, exits (VPN) and region diff are built and checked live** against Acast and Audiobookshelf.

## Documentation

`docs/` describes Solstein as built, one document per service or module; [`docs/README.md`](docs/README.md) is the index. `docs/wip.md` holds everything not finished.

- **At the start of every session, read `docs/wip.md`**, and the documents for the areas you will touch.
- **Add to `docs/wip.md` as you go:** issues, gaps, open questions, ideas and trade-offs, as soon as they are discovered or discussed. Record open questions there rather than silently picking an answer.
- **When a WIP item is resolved** (built, decided, answered or dropped), remove it from `wip.md` and record the outcome in the document for that area. `wip.md` never holds finished items, and the other documents never hold unfinished ones.
- Keep the documents current with the code: a change in behaviour updates the document that describes it in the same piece of work.

Never open, print or copy secret files (such as the maintainer's Proton key env file); refer to them only by path (`docker --env-file`) or through `env:` / `file:` references.

Never run `git` commands that change state (commit, push, branch, reset, etc.) — git is managed by the maintainer.

## Conventions

**Read `docs/development.md` before writing any code.** It covers layout, layering, naming (camelCase, acronyms as one unit), error handling, dependencies (Gin, logrus, WireGuard netstack), config, logging, build, testing, CI and Docker. Every change should follow it.

Must be clean before handing work over (CI enforces all of them, plus `go mod tidy`):
```
gofmt -l .      # must print nothing
go build ./...
go vet ./...
go test -race ./...
```

Run locally with `go run .` (serves on :8080, config directory `./config`); `go run . -h` lists every flag and its `SOLSTEIN_*` env var.

## Architectural constraints already decided

- The RSS proxy is the core; exits and region diff are modules. The core must build, run and be useful with every module disabled, and must not import module packages directly — modules plug in through interfaces the core defines. A disabled module costs nothing at runtime (no tunnels opened, no server list fetched).
- WireGuard runs in-process via `golang.zx2c4.com/wireguard` + `tun/netstack` — no gluetun sidecar, no `NET_ADMIN`, no `network_mode` coupling. Modules supply dialers and the core (`outbound`) builds every HTTP client on them, so the outbound safeguards apply to every exit; exits run concurrently; a `direct` exit (no tunnel) also exists.
- New episodes are processed when feeds are polled, and published only once processed, so Audiobookshelf never waits on diffing. The one exception (agreed): an episode without its processed file (backlog, or expired from the cache) is processed on its first request, with a bounded wait (20 s, then `503` + `Retry-After`).
- Cleaned episodes are served from a cache with correct `Content-Length` and HTTP range support; the rewritten feed carries the updated enclosure `length` and duration.
- Any WireGuard VPN is supported (generic provider from wg-quick `.conf` files); Proton is a convenience provider fed by the MIT-licensed `github.com/qdm12/gluetun-servers` data (keep attribution). VPN configuration lives in `config.json`, not flags/env.
