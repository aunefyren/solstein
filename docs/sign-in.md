# Sign-in

Who may use the web UI ([`web-ui.md`](web-ui.md)): users with a password and, if they choose, an authenticator app (TOTP). The `auth` package is the core of it; the pages are in `server/ui_auth.go`, the console commands in `console/`. The feed API and the feed and episode URLs are unchanged: they keep the subscribe token and signed URLs ([`security.md`](security.md)).

## Users, on the console only

Users are added and reset on the console, never through the web, so a stolen session can't create another account or undo a reset:

```
solstein user list
solstein user add <name>
solstein user reset-password <name>
solstein user reset-mfa <name>
solstein user delete <name>
```

In Docker: `docker exec <container> /app/solstein user add <name>`. It uses the same database as the app (`-configdir` / `SOLSTEIN_CONFIG_DIR`, as the app) while the app keeps running.

- **`add` and `reset-password` print a one-time password**, valid once and for 24 hours. At sign-in the user must then choose their own. Nothing is typed into the shell, so no password lands in its history, and whoever runs the console never learns the user's password.
- **`reset-password` and `reset-mfa` sign the user out everywhere.** `reset-mfa` is for a lost phone: the user signs in with their password alone until they set up an authenticator again. `reset-password` leaves the authenticator as it is.
- **Run as root** (as `docker exec` does), the command first switches to the owner of the config directory, as the entrypoint does for the app, and sets the entrypoint's `umask 027`: a database file it creates belongs to the app's user and isn't world-readable. Verified on a Docker volume (2026-09-28): run as root, the database it created was `1000:1000`, `0640`.
- Usernames are 1 to 64 lower-case letters, digits, dots, dashes or underscores; sign-in ignores case.

## Signing in

1. **Username and password** (`/ui/login`). Wrong either way, the answer is the same ("Wrong username or password."), and an unknown name costs the same time as a wrong password: it is checked against a dummy hash.
2. **Choosing a password** (`/ui/login/password`), after a one-time password: 10 to 256 characters, not the one-time one.
3. **A TOTP code** (`/ui/login/totp`), for a user with an authenticator.

Between steps a **sign-in token** (10 minutes, cookie `solstein_sign_in`, path `/ui/login`) carries the steps left as its scopes; each step replaces it, so a step can't be repeated or skipped. The last step issues the **session**. Afterwards the browser goes to `next`, only ever a path under `/ui/` (anything else, including `//host` and `/ui/login` itself, goes to the feed list).

**Failed attempts are limited:** 5 per username and 20 per client address within 15 minutes, counting wrong passwords and wrong codes; then even the right password is refused until the oldest failure is 15 minutes old ("Too many failed attempts. Try again after 12:10.", `429`). A success clears the username's count. In memory, so a restart forgets them: acceptable against online guessing, which is what it's for.

Logged: sign-in, sign-out, a failed sign-in and a refusal for too many, with the client address, and the username quoted as typed.

## Passwords and TOTP

- **Passwords:** argon2id (`golang.org/x/crypto/argon2`, already a dependency) with OWASP's minimum parameters (19 MiB, 2 passes, 1 thread), a 16-byte salt, in PHC string form (`$argon2id$v=19$m=19456,t=2,p=1$…`), compared in constant time. A hash records its parameters, so raising them later still verifies old hashes.
- **TOTP:** RFC 6238 with what every authenticator app supports: HMAC-SHA1, 6 digits, 30-second steps, a 160-bit secret; one step either side is accepted, for a phone's clock. **A code works once:** the step of the last accepted code is stored, and only later steps are accepted (tested with RFC 6238's own test vectors).
- **Optional per user** (decided 2026-09-28): a user sets it up under **Account**. Setting up starts with a `POST` that makes a pending secret; the page shows it as a QR code, as text in groups of four, and as an `otpauth://` link for a phone; a code from the app turns it on. Removing it needs the password.
- **The QR code** is drawn on the server as inline SVG from `rsc.io/qr` (BSD, pure Go, one package; the only new dependency), so no script or image fetch is needed and the content security policy stays as it is. Checked by decoding a screenshot of the live page with `zbarimg`.
- **The TOTP secret is stored as it is** in the database, since the server needs it to check codes. Encrypting it with a key from `config.json` was considered and not chosen, for the reason `security.md` gives for secrets: the key would sit on the same volume.

## Sessions and tokens

Every credential Solstein issues is a row in one **token** table (`models.Token`), built along OAuth 2.0 lines so later kinds need no new model:

- **Opaque bearer tokens** (RFC 6750): 256 random bits, URL-safe base64, given to the client once. Only their SHA-256 is stored, so the database doesn't hold usable tokens (a plain hash is enough: they're random, not guessable like passwords).
- **Kind** says how a token is used: `session` (the UI's cookie), `sign-in` (a sign-in in progress). **Scopes** (RFC 6749, section 3.3) say what it allows: `ui` for a session; the steps left for a sign-in. **Expiry** and **last use** on each; deleting the row revokes it (RFC 7009's effect, without its endpoint).
- **Sessions:** cookie `solstein_session`, path `/ui`, `HttpOnly`, `SameSite=Lax`, `Secure` when the browser reached Solstein over HTTPS (directly, or through a `trusted_proxies` proxy saying so). A session lasts **7 days from its last use, and at most 30 days** from sign-in; its use is recorded at most every 5 minutes. Expired tokens are cleared out at each sign-in.
- **Changing the password** (Account) ends every other session and hands this browser a new one. **Signing out** deletes the session.

What is deliberately not implemented: OAuth 2.0's authorization and token endpoints, grants, client registration and refresh tokens. The web UI signs people in directly; nothing outside Solstein asks for tokens yet. What's planned on top of this is in [`wip.md`](wip.md) (Sign-in).

## Security notes

- The sign-in pages and every UI page carry the UI's headers and content security policy, and refuse cross-origin posts ([`web-ui.md`](web-ui.md)); with a session cookie that is the CSRF protection, together with `SameSite=Lax`.
- A new token at every sign-in, so a session can't be fixed in advance.
- No page shows a password, secret after set-up, token or hash; the sign-in page never says whether a username exists.

## Verified live (2026-09-28, `docker-test`)

A user added with `docker exec … user add`, then with curl: a page signed out → `303` to `/ui/login?next=…`; a wrong password `401`; the one-time password → `/ui/login/password`, with the sign-in cookie `HttpOnly` on `/ui/login`; a chosen password → back to `next`, signed in; TOTP set up from the QR page's secret; signed out; signing in again asked for a code, and the code already used was refused (`401`); the sixth wrong password from one address `429` with the time it lifts. The pages rendered at 1280 px and 390 px, light and dark; the QR code, decoded from the screenshot, was the expected `otpauth://totp/Solstein:<user>?…`.
