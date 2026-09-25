# Solstein documentation

These documents describe Solstein **as it is built**. Anything planned, in progress, undecided or not yet understood lives in [`wip.md`](wip.md) instead, and moves into the right document here once it is done or decided.

| Document | Covers |
|---|---|
| [`architecture.md`](architecture.md) | What Solstein is, the core and its modules, how modules plug in, storage, background work |
| [`feeds.md`](feeds.md) | Subscribing, feed records and settings, the feed API, polling, feed rewriting, publish rules |
| [`episodes.md`](episodes.md) | Delivery modes, the episode pipeline, processors in the pipeline, serving audio, housekeeping |
| [`security.md`](security.md) | Tokens and signed URLs, network settings, outbound safeguards, keeping the home address out, secrets |
| [`exits.md`](exits.md) | The exits (VPN) module: providers, tunnels, Proton, exits and server selection |
| [`region-diff.md`](region-diff.md) | The region-diff module: how ads are found and cut, the processor, settings |
| [`openapi.yaml`](openapi.yaml) | The HTTP API as an OpenAPI 3.1 specification: every route, parameter, response and status code |
| [`clients.md`](clients.md) | Client behaviour Solstein relies on or works around, Audiobookshelf in detail |
| [`development.md`](development.md) | How to work on the code: layout, conventions, dependencies, testing, CI, Docker |
| [`wip.md`](wip.md) | Open issues, questions, ideas and planned work |

Rules for these documents:
- Describe current behaviour and the reasons for it. A decision that shaped the design is recorded where it applies, with its reason, not as a history of how it was reached.
- Live findings (tests against real hosts, VPNs and clients) belong with the behaviour they verified, dated.
- Nothing unfinished here: a planned feature, an open question or a known gap goes in `wip.md`.
