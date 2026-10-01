#!/bin/sh
# Sets up a freshly started harness (docker compose up -d --build after
# setup.sh) so it can be used at once, and writes every credential it makes
# to data/credentials.md (gitignored, like the rest of data/):
#   - Audiobookshelf: the root user (its setup wizard, done through its API)
#     and a Podcast library on /podcasts.
#   - Solstein: a web UI user with a chosen password (the console's one-time
#     password is used once, here, to set it).
# Passwords are random. Run it once per fresh data/; see README.md.
set -eu
cd "$(dirname "$0")"

SOLSTEIN=http://localhost:8080
ABS=http://localhost:13378
ABS_USER=root
UI_USER=tester
CREDENTIALS=data/credentials.md

if [ -f "$CREDENTIALS" ]; then
    echo "$CREDENTIALS already exists: this data/ has been set up. Start clean with rm -rf data (see README.md)." >&2
    exit 1
fi
command -v python3 >/dev/null || { echo "bootstrap.sh needs python3 (for reading JSON)." >&2; exit 1; }

json() { python3 -c "import json,sys; data=json.load(sys.stdin); print($1)"; }
password() { openssl rand -base64 18 | tr -d '/+=' | cut -c1-20; }

wait_for() {
    printf 'Waiting for %s' "$1"
    for _ in $(seq 1 60); do
        if curl -sf -o /dev/null "$1"; then echo; return 0; fi
        printf .; sleep 2
    done
    echo; echo "$1 didn't come up; see docker compose logs." >&2; exit 1
}
wait_for "$SOLSTEIN/api/health"
wait_for "$ABS/status"

# Audiobookshelf: the root user, then a Podcast library.
if [ "$(curl -s "$ABS/status" | json 'data["isInit"]')" = "True" ]; then
    echo "Audiobookshelf is already set up; start clean with rm -rf data." >&2
    exit 1
fi
abs_password=$(password)
curl -sf -o /dev/null -H "Content-Type: application/json" \
    -d "{\"newRoot\": {\"username\": \"$ABS_USER\", \"password\": \"$abs_password\"}}" "$ABS/init"
abs_token=$(curl -sf -H "Content-Type: application/json" -H "x-return-tokens: true" \
    -d "{\"username\": \"$ABS_USER\", \"password\": \"$abs_password\"}" "$ABS/login" |
    json 'data["user"].get("accessToken") or data["user"]["token"]')
curl -sf -o /dev/null -H "Authorization: Bearer $abs_token" -H "Content-Type: application/json" \
    -d '{"name": "Podcasts", "folders": [{"fullPath": "/podcasts"}], "icon": "podcast", "mediaType": "podcast", "provider": "itunes"}' \
    "$ABS/api/libraries"
echo "Audiobookshelf: user '$ABS_USER' and library 'Podcasts' created."

# Solstein: a web UI user. The console prints a one-time password; signing
# in with it leads to choosing the real one.
one_time=$(docker exec st-solstein /app/solstein user add "$UI_USER" | sed -n 's/^One-time password: //p')
ui_password=$(password)
jar=$(mktemp)
trap 'rm -f "$jar"' EXIT
curl -sf -o /dev/null -c "$jar" -b "$jar" -H "Origin: $SOLSTEIN" \
    --data-urlencode "username=$UI_USER" --data-urlencode "password=$one_time" "$SOLSTEIN/ui/login"
curl -sf -o /dev/null -c "$jar" -b "$jar" -H "Origin: $SOLSTEIN" \
    --data-urlencode "password=$ui_password" --data-urlencode "repeat=$ui_password" "$SOLSTEIN/ui/login/password"
echo "Solstein: web UI user '$UI_USER' created."

auth_token=$(json 'data["auth_token"]' < data/solstein/config.json)

umask 077
cat > "$CREDENTIALS" <<EOF
# Harness credentials

Made by bootstrap.sh on $(date '+%Y-%m-%d %H:%M'). Local test logins only; this file is gitignored with the rest of data/.

| What | Where | User | Password |
|---|---|---|---|
| Solstein web UI | $SOLSTEIN/ui | \`$UI_USER\` | \`$ui_password\` |
| Audiobookshelf | $ABS | \`$ABS_USER\` | \`$abs_password\` |

Solstein's API token (\`auth_token\` in data/solstein/config.json), for \`Authorization: Bearer\`:

\`\`\`
$auth_token
\`\`\`

In Audiobookshelf, add a feed with Add Podcast in the **Podcasts** library, pasting the \`feed_url\` the feed API returns (it starts with http://solstein:8080, which Audiobookshelf reaches on the harness network).
EOF
echo "Credentials written to $CREDENTIALS."
