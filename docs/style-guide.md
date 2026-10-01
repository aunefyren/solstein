# Web UI style guide

How the web UI ([`web-ui.md`](web-ui.md)) looks, reads and is built. **Every change to the frontend follows this document**, and a change to the look or the rules updates it in the same piece of work. The stylesheet (`server/web/static/style.css`) implements the tokens and components below; if the two disagree, one of them is wrong.

## Principles

- **Quiet and functional.** It's an admin page for a background service. It shows state and lets you change a few settings; it doesn't decorate. No illustrations, gradients, shadows or animation.
- **Server-rendered, works without JavaScript.** Pages are Go `html/template`, changes are plain HTML forms. A page must work fully with scripts off; the one script there is (live updates, below) only makes a working page smoother.
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
| `--color-qr-dark` | `#000000` | `#000000` | A QR code's modules: the same in both modes, so a phone can scan it |
| `--color-qr-light` | `#ffffff` | `#ffffff` | A QR code's background and quiet zone |

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

- **Site header** (`site-header`): the product name, the nav (`site-nav`, the current page marked `aria-current="page"`), and on the right the user (`site-user`): the signed-in name, linking to Account, and a Sign out button (`button--secondary`, a form posting to `/ui/logout`). Signed out, the header has the name only: no nav, no user.
- **Page heading** (`page-heading`): one `h1` per page, with an optional one-line description in `--color-muted` under it.
- **Notice** (`notice`, `notice--ok`, `notice--error`): the outcome of the last action, at the top of the content, with `role="status"` (ok) or `role="alert"` (error). Says what happened, naming the object: "Saved the settings of 'Debatten'."
- **Data table** (`data-table`, with `data-table__title` for a row's name, `data-table__form` for a row's settings form, `data-table__empty` for the "nothing yet" row): one row per thing (feed, exit); stacks into cards on narrow screens, each cell carrying its column name (`data-label`). The row's name comes first; secondary facts about it go in `meta` lines under it. **A table whose rows change live has fixed column widths** (`data-table--fixed` and a modifier giving its columns, e.g. `data-table--episodes`), so an update never moves the columns sideways.
- **Editable table** (`data-table--edit`, with `data-table--fixed` and a modifier giving its columns, e.g. `data-table--rules`): a data table that is one form, for editing a list whose order matters (a feed's rules). Each row's cells hold its fields, styled like the setting select; each field has its own `<label>`, `visually-hidden` and naming the row ("Rule 2: at most"), since the column head names the column for the eye and the stacked card shows it again through `data-label`. The row's name (`data-table__title`, "Rule 2") is shown only when the table stacks, heading the card; side by side, the row's position says it and the fields' labels name it. A cell whose field already says what it is in visible words (a checkbox labelled "Remove") carries no `data-label`, so the card doesn't say it twice. Adding an entry is an empty row **set apart in its own row group** (`data-table__group`: a `<tbody>` headed by a full-width `<th scope="rowgroup">`, "Add a rule", on the page background), ignored when left empty. Order is a **position select** per row, not move buttons, so the form keeps a single submit button and Enter in a field saves: an existing row's select gives its place (1, 2 …), the new row's says where it goes in words ("Before rule 1" … "At the end", the default). Removing is a **checkbox** per row, taking effect on save. The whole list is saved at once by the **form footer** under the table; a refused save shows the page again with the rows as sent, so nothing typed is lost.
- **Checkbox** (`checkbox`): a native checkbox with its label beside it, in muted small text; the box takes the accent colour.
- **Form footer** (`form-footer`): the save button of a form that doesn't fit `form-stack` (an editable table), with an optional meta line beside it.
- **Status badge** (`badge`, `badge--on`, `badge--off`, `badge--warn`, `badge--error`): a short word in a bordered pill, e.g. "On", "Off", "Check" (works, but worth a look), "Failing". **The word always carries the meaning**; the colour only reinforces it.
- **Setting select** (`setting`): a labelled `<select>` for a per-feed override with three options: "Default (on)" or "Default (off)" (what the global setting gives), "On", "Off". The label is visible, not only a placeholder.
- **Form** (`form-stack`): fields one under the other, each a `field`: a visible `<label>` above its `<input>`, with an optional hint (`field__hint`, muted) under it. Inputs get the right `type`, `autocomplete` (`username`, `current-password`, `new-password`, `one-time-code`) and, for codes, `inputmode="numeric"`, so password managers and phones do the right thing. The primary button last.
- **Sign-in card** (`auth-card`): the sign-in steps, alone in a narrow centred column (24rem), one `h1` saying the step ("Sign in", "Choose your password", "Enter your code").
- **QR code** (`qr`): inline SVG made on the server, always dark on white (`--color-qr-*`) with its quiet zone, 12rem wide; always with the secret as text beside it, for typing in instead.
- **Button** (`button`, `button--primary`, `button--secondary`): `button--primary` for the one main action of a form (Save), `button--secondary` otherwise. Buttons say the action as a verb ("Save", not "OK" or "Submit").
- **Meta line** (`meta`): small muted text for secondary facts (hostname, last poll).
- **View links** (`view-links`): plain links that change what a list shows ("All 86 · Only problems 4 · Show all"), the current one marked `aria-current="true"`; a query parameter, so they work without JavaScript and can be bookmarked.
- **Live region** (`data-live` on the part of a page that shows work in progress: a feed's episodes, the exits' tunnels; `live-status` for its status line): while the server marks it `active` (episodes queued or being prepared, tunnels open), `live.js` fetches the same page every 5 s and swaps in the new region, leaving everything outside it (a settings form being edited, the scroll position) alone. It never swaps while a control inside the region (a button, a link) has focus, keeps focus on the same row across a swap, and doesn't poll while the tab is hidden. **Actions inside the region** (forms marked `data-live-submit`) post in the background and swap in the region from the answer, so the page doesn't reload and the scroll position stays; their outcome ("Queued 'RIP kong Harald'.") shows in the status line. Every such form also works without the script, and then comes back to the same view (`show`) and row (`#episode-…`). The status line says what's happening in words ("Updating every 5 s while 2 episodes are queued or working.") with a Pause/Resume button, remembered for the tab; its text is announced politely to screen readers only when the region's summary changes, not on every update. Once nothing is in progress it says "Up to date." and stops. Without JavaScript, the page refreshes itself through `<noscript>` instead, with a Stop link (`?live=off`).
- **Actions** (`actions`): a row of buttons acting on a whole section ("Retry all failed", "Prepare all not cached"), `button--secondary`, each a form of its own; a button appears only when its action has something to do, and says how much ("Retry 3 failed").
- **Section** (`section`): a titled group of content on a page, with an `h2` at `--text-lg`. A page with several groups uses one per group, in the order a reader needs them.
- **Fact list** (`facts`): read-only settings or properties as a `<dl>`, the label (`<dt>`, muted) beside its value (`<dd>`), stacking below 44rem. A value can start with a status badge ("On", "Off", "Check"), still followed by words that say what it means. Values that are names from `config.json`, hostnames or paths are `mono`. A value that is simply absent says so in words ("Not set", "Any address"), never an empty cell or a dash. A value that has a page of its own links to it (`<a>`), rather than repeating it.

## Writing

- **English, British spelling**, as in the rest of Solstein ("normalise", "licence").
- **Plain words, full sentences for messages**, naming the object, as the log does: "Saved the settings of 'Debatten'.", "Couldn't save the settings of 'Debatten': region diff can only be on or off."
- Settings use the name from `config.json`, spelled out for people: `region_diff` is "Region diff", `prepare_ahead` is "Prepare ahead". A hint says what it does in one line.
- Times as `28 Sep 2026 09:39`, in Solstein's time zone. "Never" for a time that hasn't happened yet. How long ago, or for how long, in the largest whole unit that fits: "40 s", "3 min", "2 h", "5 days" ("40 s ago", "idle 3 min").
- A page showing live state (tunnels, jobs) says when it was taken ("As of 10:52:07"). It updates itself only through a **live region** (below), only while something is in progress, and always visibly, with a way to pause.
- No exclamation marks, no "Oops", no emoji.
- **Never show a secret or an internal error.** No subscribe token, signing key, signed URL or VPN key; no raw error text (it can hold a feed's private URL): say what failed and point to the log, as the API does.

## Rules for building it

- **Where things live:** templates in `server/web/templates/` (`layout.html` plus one file per page, which defines `title` and `content`), static files in `server/web/static/`, all embedded in the binary. The Go side is `server/ui.go`.
- **One stylesheet, no inline style.** No `style=` attributes, `<style>` or inline `<script>` blocks: the content security policy (`style-src 'self'`, `script-src 'self'`) refuses them.
- **JavaScript only as progressive enhancement**, in files under `static/` (today one: `live.js`), loaded with `defer`; no inline script, no framework, no build step, no third-party code. Every page works without it. It builds DOM with `createElement`/`textContent`, never by inserting strings as HTML, except swapping in a region parsed from Solstein's own page. A new script, or new behaviour in one, goes into this guide first.
- **No external resources:** no CDN, no web fonts, no remote images or analytics. Solstein must work offline and never tell a third party who opens its UI.
- **No framework and no build step:** plain HTML templates and one hand-written CSS file.
- **GET never changes anything.** A change is a `POST` form, answered with `303 See Other` to a page (post/redirect/get), so reloading never repeats it. Cross-origin POSTs are refused.
- **Every page goes through the UI's authenticator** (`uiAuthenticator`, `web-ui.md`): the routes are registered on the `/ui` group that runs it, never directly on the router.
- **Accessible by default:** every input has a `<label>`; a column or control whose meaning is obvious to the eye but not in words gets text in `visually-hidden` (read out, not shown), never nothing; everything works by keyboard with a visible focus ring (`--color-accent`, 2px outline); headings in order; tables have `<th scope>`; status never by colour alone.

## Adding a page

1. A template in `server/web/templates/`, from the layout, with one `h1`.
2. A handler in `server/ui.go`, on the `/ui` group; any change is a `POST` answered with `303`.
3. Only the components above; a new one goes into this guide first.
4. A test in `server/ui_test.go`; the route in `docs/openapi.yaml` and its coverage test.
5. The page in [`web-ui.md`](web-ui.md).
