# Security

Solstein is ideally reachable only by its client (e.g. ABS and Solstein on the same Docker network, `http://solstein:8080/...`), but must be safe when exposed publicly. Unprotected, the prefix URL would make it an open proxy — through the VPN exits, on the operator's bandwidth — and a way to make it fetch internal addresses (server-side request forgery). ABS blocks private addresses by default; a private-network setup needs `SSRF_REQUEST_FILTER_WHITELIST=solstein` on the ABS side ([`clients.md`](clients.md)).

## Tokens and signed URLs

Clients generally can't send custom headers or cookies on feed and episode requests (confirmed for ABS), and enclosure downloads carry no credentials at all. So credentials go in the URL, as private podcast feeds (Patreon, Supercast) do. This works with any client. The checks are in `server/access.go`.

- **Subscribe token** (`auth_token`, generated on first run, at least 16 characters): required on `/api/rss/{token}/...` and the feed API. Without it, Solstein fetches nothing on anyone's behalf. Compared in constant time.
- **Signed URLs:** every URL Solstein writes out (feed self-links, enclosures) carries an HMAC-SHA256 signature over its canonical path (`signing` package, URL-safe base64 in `sig`), made with `url_signing_key` (generated and stored). The handler checks the request path equals the canonical path, so a differently spelled path can't reuse a signature. A leaked episode or feed URL opens only that one feed. Rotating the key invalidates every URL; clients would need re-subscribing.
- A client subscribed through the prefix URL stores the subscribe token in its feed URL. Acceptable for a trusted client like ABS; the explicit flow avoids it by handing out a signed feed URL. Redirecting the prefix URL to the signed URL wouldn't help with ABS, which stores the URL it was given.
- `disable_auth` turns off the token and signature checks (the client network check stays), for private-network-only setups; it logs a warning at start-up. With it on, the token segment of the prefix route is optional.
- **Logs never hold secrets:** the request log records the path only, never the query string (signatures, API tokens), with the prefix route's token shown as `***`; source URLs are logged without their query string, since private feeds often carry a token there.

## Network settings

Inbound:
- **`allowed_client_networks`:** CIDRs allowed to use Solstein at all (e.g. `172.16.0.0/12` for a Docker network). Empty allows any address. Checked in addition to the token. `/api/health` is exempt so container health checks keep working.
- **`trusted_proxies`:** reverse proxies whose `X-Forwarded-For` (and `-Proto`/`-Host`, for building links when `external_url` is unset) are believed. Empty trusts none.
- **`external_url`:** the base for every URL Solstein writes out.

Outbound, enforced in `outbound` so it covers every fetch through every exit (details in [`development.md`](development.md)):
- Only `http` and `https`, at most 10 redirects.
- **`allow_private_destinations`** (off by default): loopback, private, link-local, CGNAT, documentation, multicast and reserved ranges (including IPv4-mapped, NAT64 and 6to4 forms) are refused. Checked on each IP a connection actually dials, redirects included, so DNS tricks can't get round it. Applies inside VPN tunnels too.
- **`allowed_source_hosts`:** optional allowlist of hosts feeds can be subscribed from (a name includes its subdomains). It applies to feed URLs only, not every fetch: enclosures and redirects legitimately point at CDNs and ad servers.
- `HTTP_PROXY`/`HTTPS_PROXY` are ignored (a proxy would carry traffic out of another route); a fixed `Solstein/<version>` User-Agent; dial, TLS and response-header timeouts; HTTP/2 health-check pings.

## Keeping the home address out

An operator may want no connection at all to leave from their own address:
- **`default_exit`:** the exit for everything that names none — feeds without their own exit, and the Proton server-list refresh. Empty means `direct`.
- **`disable_direct`:** refuses the `direct` exit altogether; needs a `default_exit` naming a VPN exit. A feed or region-diff pair naming `direct` is then invalid (region diff's home side becomes a VPN exit in the home country, e.g. Proton `NO`).
- **Never a silent fallback:** when the default exit is down, requests fail rather than go out directly. A `default_exit` that doesn't exist or failed to load **stops start-up** — stricter than "a broken module stays off", because the operator has said where traffic must go, and leaking would be worse than not running.
- DNS for tunnelled requests goes through the tunnels. The VPN endpoints themselves are resolved and reached from the host, necessarily.
- Built in `outbound` (`Options.DefaultExit`, `DisableDirect`, `ErrDirectDisabled`) and `settings`. Tested end to end (a feed naming no exit subscribed through the tunnel, `direct` refused, each misconfiguration stopping start-up), and used in the region-diff live check ([`region-diff.md`](region-diff.md)).

## Secrets

- Any secret field in `config.json` may be a literal, `env:NAME` or `file:PATH` (e.g. a Docker secret); `settings.ResolveSecret` turns it into the value. `config.json` keeps the reference and it is what is written back; the resolved value is never persisted, logged or returned by any API.
- A reference that can't be resolved makes that provider unavailable with an error naming the variable or file, never the value; it doesn't stop start-up.
- Resolved WireGuard keys are held in `exits.Key`, which prints as `[redacted]` in every format and in JSON (tested).
- VPN secrets don't go through the flag/env settings table: that table writes values back to `config.json`, which would put them there in plain text.
- Secrets Solstein generates (the subscribe token, the signing key) stay in `config.json`, which is written `0600` atomically; the container's `umask 027` keeps the database and cache private too.
- Considered and not chosen: encrypting secrets inside `config.json` with a master key from the environment. On a typical host the master key sits in the compose file next to the data volume, so it is no stronger than putting the secret itself in the environment, and it adds a failure mode (a lost master key) and complicates write-back.
