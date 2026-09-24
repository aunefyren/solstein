# Embedded data

- `protonvpn.json.gz` — Proton VPN's server list, from
  [qdm12/gluetun-servers](https://github.com/qdm12/gluetun-servers) (`pkg/servers/protonvpn.json`),
  MIT-licensed; see `LICENSE-gluetun-servers`. Solstein ships this snapshot and refreshes it daily
  from the repository at runtime, keeping the last good copy in the config directory.
  Snapshot: gluetun-servers commit `0c7381f` (2026-08-06), schema version 4.

To update the snapshot, run from the repository root:

```
curl -fsSL https://raw.githubusercontent.com/qdm12/gluetun-servers/main/pkg/servers/protonvpn.json \
  | gzip -9n > modules/exits/data/protonvpn.json.gz
```

then run the tests (`go test ./modules/exits/`), which check that every country in the list maps to
an ISO code.
