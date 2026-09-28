# Web UI style guide

How the web UI ([`web-ui.md`](web-ui.md)) looks, reads and is built. **Every change to the frontend follows this document**, and a change to the look or the rules updates it in the same piece of work. The stylesheet (`server/web/static/style.css`) implements the tokens and components below; if the two disagree, one of them is wrong.

## Principles

- **Quiet and functional.** It's an admin page for a background service. It shows state and lets you change a few settings; it doesn't decorate. No illustrations, gradients, shadows or animation.
- **Server-rendered, works without JavaScript.** Pages are Go `html/template`, changes are plain HTML forms. A page must work fully with scripts off.
- **Honest about state.** Show what Solstein will actually do (the value *in use*), not only what was configured, and say where an inherited value comes from ("Default (on)").
- **Grows by adding pages, not by restyling.** A new page reuses the tokens and components here. A new component is added to this guide first.

## Look

The name is the Iceland spar sunstone: clear, pale, with a warm honey tint. The palette is neutral greys with one warm amber accent, used sparingly: the primary button, links, focus, the current nav item.

### Colour tokens

Defined once on `:root` in `style.css`, with a dark set under `prefers-color-scheme: dark`. **No colour value appears anywhere else** in the CSS; everything uses `var(--…)`. The one exception is the icon (`static/icon.svg`), an image that can't read CSS variables: it uses the two accent values, light and dark, as they are.

| Token | Light | Dark | Use |
|---|---|---|---|
| `--color-bg` | `#f6f5f2` | `#15171a` | Page background |
| `--color-surface` | `#ffffff` | `#1d2024` | Cards, table, header |
| `--color-border` | `#dcdad3` | `#30353b` | Borders, dividers |
| `--color-text` | `#1f2328` | `#e6e7e9` | Body text |
| `--color-muted` | `#5c6168` | `#a3a9b0` | Secondary text, labels, hints |
| `--color-accent` | `#8a4d0f` | `#f0b25a` | Links, primary button, focus, current nav |
| `--color-on-accent` | `#ffffff` | `#1b1406` | Text on the accent |
| `--color-ok` | `#1d6f3a` | `#6bd08f` | "On", success notices |
| `--color-warn` | `#8a5a00` | `#e8b04b` | Degraded, needs a look |
| `--color-error` | `#b42318` | `#ff8f80` | Failures, errors |

Every text/background pair meets WCAG AA (4.5:1 for body text). Check a new pair before adding it.

### Type

- System font stack (`system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`); `ui-monospace, "SF Mono", Consolas, monospace` for hostnames, IDs and paths. **No web fonts**: nothing is loaded from outside Solstein.
- Base 16px (`1rem`), line height 1.5. Scale: `--text-sm` 0.875rem (hints, table meta), `--text-md` 1rem, `--text-lg` 1.25rem (section headings), `--text-xl` 1.5rem (page title). Weights 400 and 600 only.
- Sentence case everywhere: headings, buttons, labels. Never all caps.

### Space, shape, layout

- Spacing scale on a 4px grid: `--space-1` 4px, `--space-2` 8px, `--space-3` 12px, `--space-4` 16px, `--space-5` 24px, `--space-6` 32px. Nothing off the scale.
- `--radius` 6px for surfaces, inputs and buttons; 1px `--color-border` borders; no shadows.
- Content column up to `--width-page` 72rem, centred, with a 16px side gutter; no horizontal page scroll at 320px wide. Tables become stacked cards below 44rem.

## Components

Class names are `block`, `block__element`, `block--modifier` (BEM-style), all lowercase with hyphens. Only these components exist; add a new one here before using it.

- **Site header** (`site-header`): the product name, the nav (`site-nav`, the current page marked `aria-current="page"`), and on the right the user status (`site-user`): "No sign-in" today, the signed-in name later.
- **Page heading** (`page-heading`): one `h1` per page, with an optional one-line description in `--color-muted` under it.
- **Notice** (`notice`, `notice--ok`, `notice--error`): the outcome of the last action, at the top of the content, with `role="status"` (ok) or `role="alert"` (error). Says what happened, naming the object: "Saved the settings of 'Debatten'."
- **Data table** (`data-table`, with `data-table__title` for a row's name, `data-table__form` for a row's settings form, `data-table__empty` for the "nothing yet" row): one row per thing (feed, exit); stacks into cards on narrow screens, each cell carrying its column name (`data-label`). The row's name comes first; secondary facts about it go in `meta` lines under it.
- **Status badge** (`badge`, `badge--on`, `badge--off`, `badge--warn`, `badge--error`): a short word in a bordered pill, e.g. "On", "Off", "Check" (works, but worth a look), "Failing". **The word always carries the meaning**; the colour only reinforces it.
- **Setting select** (`setting`): a labelled `<select>` for a per-feed override with three options: "Default (on)" or "Default (off)" (what the global setting gives), "On", "Off". The label is visible, not only a placeholder.
- **Button** (`button`, `button--primary`, `button--secondary`): `button--primary` for the one main action of a form (Save), `button--secondary` otherwise. Buttons say the action as a verb ("Save", not "OK" or "Submit").
- **Meta line** (`meta`): small muted text for secondary facts (hostname, last poll).
- **Section** (`section`): a titled group of content on a page, with an `h2` at `--text-lg`. A page with several groups uses one per group, in the order a reader needs them.
- **Fact list** (`facts`): read-only settings or properties as a `<dl>`, the label (`<dt>`, muted) beside its value (`<dd>`), stacking below 44rem. A value can start with a status badge ("On", "Off", "Check"), still followed by words that say what it means. Values that are names from `config.json`, hostnames or paths are `mono`. A value that is simply absent says so in words ("Not set", "Any address"), never an empty cell or a dash. A value that has a page of its own links to it (`<a>`), rather than repeating it.

## Writing

- **English, British spelling**, as in the rest of Solstein ("normalise", "licence").
- **Plain words, full sentences for messages**, naming the object, as the log does: "Saved the settings of 'Debatten'.", "Couldn't save the settings of 'Debatten': region diff can only be on or off."
- Settings use the name from `config.json`, spelled out for people: `region_diff` is "Region diff", `prepare_ahead` is "Prepare ahead". A hint says what it does in one line.
- Times as `28 Sep 2026 09:39`, in Solstein's time zone. "Never" for a time that hasn't happened yet. How long ago, or for how long, in the largest whole unit that fits: "40 s", "3 min", "2 h", "5 days" ("40 s ago", "idle 3 min").
- A page showing live state (tunnels, jobs) says when it was taken ("As of 10:52:07; reload for the latest"); pages don't refresh themselves.
- No exclamation marks, no "Oops", no emoji.
- **Never show a secret or an internal error.** No subscribe token, signing key, signed URL or VPN key; no raw error text (it can hold a feed's private URL): say what failed and point to the log, as the API does.

## Rules for building it

- **Where things live:** templates in `server/web/templates/` (`layout.html` plus one file per page, which defines `title` and `content`), static files in `server/web/static/`, all embedded in the binary. The Go side is `server/ui.go`.
- **One stylesheet, no inline style.** No `style=` attributes, `<style>` or `<script>` blocks: the content security policy (`style-src 'self'`, no scripts) refuses them.
- **No JavaScript** for now. If a page ever needs it: its own file under `static/`, progressive enhancement only, and the page still works without it. This guide says so first.
- **No external resources:** no CDN, no web fonts, no remote images or analytics. Solstein must work offline and never tell a third party who opens its UI.
- **No framework and no build step:** plain HTML templates and one hand-written CSS file.
- **GET never changes anything.** A change is a `POST` form, answered with `303 See Other` to a page (post/redirect/get), so reloading never repeats it. Cross-origin POSTs are refused.
- **Every page goes through the UI's authenticator** (`uiAuthenticator`, `web-ui.md`): the routes are registered on the `/ui` group that runs it, never directly on the router.
- **Accessible by default:** every input has a `<label>`; everything works by keyboard with a visible focus ring (`--color-accent`, 2px outline); headings in order; tables have `<th scope>`; status never by colour alone.

## Adding a page

1. A template in `server/web/templates/`, from the layout, with one `h1`.
2. A handler in `server/ui.go`, on the `/ui` group; any change is a `POST` answered with `303`.
3. Only the components above; a new one goes into this guide first.
4. A test in `server/ui_test.go`; the route in `docs/openapi.yaml` and its coverage test.
5. The page in [`web-ui.md`](web-ui.md).
