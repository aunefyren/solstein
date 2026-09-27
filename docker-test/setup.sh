#!/bin/sh
# Prepares ./data for the harness (gitignored, holds Solstein's config
# directory and Audiobookshelf's config/metadata/podcasts) and seeds
# Solstein's config.json from config.template.json on first run only, so
# a subsequent run never overwrites settings changed since. See README.md.
set -eu
cd "$(dirname "$0")"

mkdir -p data/solstein data/abs/config data/abs/metadata data/abs/podcasts

if [ ! -f data/solstein/config.json ]; then
    cp config.template.json data/solstein/config.json
    echo "Seeded data/solstein/config.json from config.template.json"
else
    echo "data/solstein/config.json already exists; leaving it as is"
fi

echo "Next: docker compose up -d --build"
