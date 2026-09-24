#!/bin/sh
set -eu

# New files (database, log, cache) are not world-readable: the database holds
# feed URLs, and private feeds often carry an access token in theirs.
umask 027

# A first argument that isn't a flag is a command to run instead of the app
# (e.g. `docker run -it <image> sh` for debugging).
if [ "$#" -gt 0 ] && [ "${1#-}" = "$1" ]; then
    exec "$@"
fi

# Solstein reads its SOLSTEIN_* environment variables itself, so any arguments
# are passed straight through as flags; flags take precedence over the
# environment.
set -- /app/solstein "$@"

# Started as root (the default): make the config directory belong to PUID/PGID
# and drop to that uid/gid. Only entries with the wrong owner are changed, so
# restarts with a large episode cache stay fast. A bind-mounted directory that
# Docker created on the host is root-owned, so without this Solstein couldn't
# write its config, log or cache.
# Started as non-root (e.g. `user: "1000:1000"` in compose): run as-is.
if [ "$(id -u)" = "0" ]; then
    PUID="${PUID:-1000}"
    PGID="${PGID:-1000}"
    case "$PUID$PGID" in
        "" | *[!0-9]*)
            echo "PUID and PGID must be numeric (got PUID='$PUID' PGID='$PGID')" >&2
            exit 1
            ;;
    esac
    configDir="${SOLSTEIN_CONFIG_DIR:-/config}"
    mkdir -p "$configDir"
    find "$configDir" \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown "$PUID:$PGID" {} +
    exec su-exec "$PUID:$PGID" "$@"
fi

exec "$@"
