# Web UI

A small web interface for people, next to the API: server-rendered pages under `/ui`, built to the [style guide](style-guide.md). Three pages so far: the feeds with their simple settings, the exits, and the instance; more pages are added the same way (style guide, "Adding a page").

## Switching it on

```jsonc
"web_ui": {
  "enabled": false   // serve the web UI under /ui
}
```

Also `-webui` / `SOLSTEIN_WEB_UI`. **Off by default**, because it has no sign-in yet (below): switched on, anyone who can reach Solstein can change feed settings through it. Switched off, it costs nothing: no templates are parsed and none of its routes exist (`/ui/...` and `/` answer `404`). Start-up warns while it's on, saying how widely it can be reached ("…from any address, as allowed_client_networks is empty").

With it on, `/` redirects to `/ui/`, and `/ui/` to the feed list.

## Pages

**Feeds** (`/ui/feeds`): every feed, with its source's host, exit and delivery mode in use, and when it was last polled ("Failing" when the last poll failed; the log says why). For each feed, **Region diff** (only while the region-diff module runs) and **Prepare ahead**: a badge for what applies to the feed now, and a select for its own override: "Default (on)" / "Default (off)", naming what the global setting gives, "On" or "Off". Save posts the form and comes back to the list with "Saved the settings of '…'." A setting the feed can't have (for example region diff "on" while the module isn't running) comes back as an error notice, and nothing is saved.

A change goes through the same code as `PATCH /api/v1/feeds/{id}` (`handlers.updateFeed`): cached files made with the old settings are dropped, and switching prepare ahead on queues the backlog, exactly as through the API. It's logged: "Changed the settings of feed 'Debatten' through the web UI (region diff: off, prepare ahead: on)."

**Exits** (`/ui/exits`): where Solstein's traffic goes out, as of when the page was loaded (it doesn't refresh itself; it says the time). A table of every exit: its provider (or "This host's own connection" for `direct`), what uses it (default exit, region diff home, partner or fallback, how many feeds name it), where it goes out ("NO only" when strict, otherwise the list it works down), the server its next connection goes to, and its tunnel: "Open" with its users (or how long it's been idle), the last handshake and which key by number, or "Closed" (tunnels open on first use). Then one section per VPN provider: type, servers and where the list came from, how many keys, the tunnel limit, the open tunnels, and benched servers (marked "Check", with how much longer). Last, **Setup**: the default exit, the direct exit, and start-up's check that every exit in use at once can have its own tunnel, either "Enough" or the same warning as the log, marked "Check". Live data comes from `outbound.Manager.Status` (`outbound.StatusReporter`, implemented by the exits module, [`exits.md`](exits.md)), so the server shows tunnels without importing the module.

**Instance** (`/ui/instance`): this Solstein as it runs, read-only, in four sections. **Solstein:** version, running since, time zone, external URL, number of feeds. **Modules:** exits (VPN) and region diff, each "On" or "Off" with the module's own summary of how it came up (or why it's off); the VPN line links to the exits page instead of repeating it ("3 exits, default mine: see Exits."); the web UI itself is marked "Check" while it has no sign-in. **Defaults for feeds:** delivery mode, polling, prepare ahead, region diff and its failure policy (only while the module runs), the client wait, the cache, tracking redirects. **Access:** token and signed URLs, allowed client networks, trusted proxies, allowed source hosts, private destinations. Anything worth a look carries a "Check" badge: no external URL, any address allowed, auth or the private-address block off. Nothing on it can be changed there: it says to change `config.json` and restart.

What start-up works out beyond `config.json` (module state, the resolved default exit) comes in as `server.Instance`, filled in by `main.go`, so the server shows it without importing a module. The page never shows the subscribe token, the signing key, a VPN key or the `env:`/`file:` references naming one (tested).

What isn't there yet, on purpose, is in [`wip.md`](wip.md) (Web UI).

## How it's built

- `server/ui.go`: routes, handlers and the authenticator; templates and static files in `server/web/`, embedded in the binary with `go:embed`. Go's `html/template` escapes everything from a feed (a title can't inject markup; tested).
- No JavaScript, no framework, no build step, no new dependency. Each change is a plain form `POST` answered with `303 See Other` (post/redirect/get), so a reload never repeats it.
- Stylesheet and icon at `/ui/static/`, linked with `?v=<version>` and cached for a day.

## Security

- **Behind `allowed_client_networks`** like every other route. That's the only guard while there's no sign-in, which is why the UI is off by default and start-up warns.
- **Cross-origin posts are refused** (`403`, logged): Go's `http.CrossOriginProtection` checks `Sec-Fetch-Site`, falling back to `Origin` against `Host`. Without it, a page on another site could change settings through the browser of someone on an allowed network; with a session cookie later, it's the CSRF protection sign-in needs anyway.
- **Headers:** a content security policy allowing only the UI's own stylesheet and icon (`default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`: no scripts, no inline style, no framing), `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`; pages are `Cache-Control: no-store`.
- **Nothing secret on a page:** no subscribe token, signed feed URL or key; a feed's source shows only its host, since the path or query of a private feed's URL can carry its access token; a failed poll shows "Failing", not the error text, which can contain that URL (tested).
- **One place for sign-in:** every page is registered on the `/ui` group that runs `uiAuthenticator` (static files aside, which a sign-in page may need). Today that's `noSignIn`: everyone is anonymous, and the header says "No sign-in". A test swaps in an authenticator that refuses everyone and checks that every UI route answers with its `401`, so a page added later can't miss it. `uiUser` carries a `Subject` (OAuth 2.0/OpenID Connect `sub`) and a name for when sign-in exists.

## Verified live (2026-09-28, `docker-test`)

With `web_ui.enabled: true`: the start-up warning, `/` → `/ui/`, the feed list with both NRK shows and their defaults, a save through the form (`303`, the API showing `region_diff: off` and `prepare_ahead: on` afterwards, and the backlog queued for prepare ahead: "Queued 10 episodes of 'De 10 siste fra Debatten' to be prepared ahead in the background."), a cross-site post refused with `403`. Rendered in headless Chrome at 1280 px and 390 px wide (the table stacking into cards), light and dark.

The instance page, the same day: all five sections filled from the harness (two Proton keys, exits `abroad`, `mine`, `us`, region diff on with `us` as fallback, direct exit off), the web UI and any-address access marked "Check", no key or key reference anywhere in the page; rendered at 1280 px and 390 px.

The exits page, the same day, while a region-diff episode was being cleaned: `mine` open with 2 users (NO#23, key 2), the `us` fallback open (US-AZ#108, key 1), `abroad` closed after its key had moved to the US server — the key move the page's own Tunnels warning ("can hold 2 tunnels at once … but 3 of its exits can be in use at the same moment") predicts. No key or key reference anywhere in the page; rendered at 1280 px and 390 px.
