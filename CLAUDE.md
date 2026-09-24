# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Solstein — a self-hosted Go podcast RSS proxy that sits between podcast hosts (Acast first) and an Audiobookshelf instance. The core fetches and polls source feeds, rewrites them and serves them to Audiobookshelf. Everything else is an optional module that can be switched off:

- **Exits (VPN):** embedded userspace WireGuard tunnels that let Solstein fetch through other regions. If no VPN is configured, the module is off and all traffic goes out directly.
- **Region diff:** strips dynamically inserted ads (DAI) by downloading each episode through two exits and removing the audio that differs. It depends on the exits module.

The name is the Viking sunstone (Iceland spar), which shows everything twice through double refraction — a nod to the region-diff module. Licence: GPL-3.0.

**Status: core proxy built; exits module designed; region diff in planning.** The core (feeds, episodes, serving, auth, housekeeping) is complete — see **Core build order** in `docs/design.md`. The exits (VPN) module's design and decisions are agreed and it is built in the order under **Exits build order**. The region-diff module is still in planning: do not write code for it until the maintainer says its design is done.

Never open, print or copy secret files (such as the maintainer's Proton key env file); refer to them only by path (`docker --env-file`) or through `env:` / `file:` references. The working design lives in `docs/design.md` — read it at the start of every session and keep it current as decisions are made; record open questions there rather than silently picking an answer.

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
- WireGuard runs in-process via `golang.zx2c4.com/wireguard` + `tun/netstack` — no gluetun sidecar, no `NET_ADMIN`, no `network_mode` coupling. Each exit gets its own `http.Transport` using the netstack `DialContext`; exits run concurrently; a `direct` exit (no tunnel) also exists.
- Episodes are processed when feeds are polled, never on request, so Audiobookshelf never waits on diffing.
- Cleaned episodes are served from a cache with correct `Content-Length` and HTTP range support; the rewritten feed carries the updated enclosure `length` and duration.
- Any WireGuard VPN is supported (generic provider from wg-quick `.conf` files); Proton is a convenience provider fed by the MIT-licensed `github.com/qdm12/gluetun-servers` data (keep attribution). VPN configuration lives in `config.json`, not flags/env.
