# Region diff module

`modules/regiondiff` removes dynamically inserted ads by downloading each episode through two exits in different ad markets and keeping only the audio both share. Nothing is decoded or re-encoded: the show stays byte for byte as published. It is an episode processor ([`architecture.md`](architecture.md)); the frame reader it uses is the `mp3` package.

## Why it works: how Acast inserts ads

Measured on one episode of a show with Norwegian dynamic ads ("Corner Piece"), downloaded twice from Norway (direct) and twice through a Swedish Proton exit (2026-09-25):
- Same region, same moment: byte-identical. Different regions: different (42,848,705 vs 43,044,093 bytes; the feed states 38,273,358).
- Both files are CBR 128 kbit/s, 44.1 kHz MPEG-1 Layer III: one 188-byte ID3v2 tag and nothing but frames, ending in a cut-off frame (200 of 417 bytes in the Norwegian download, 399 of 418 in the Swedish one). No Xing/LAME header, no junk.
- **Acast splices whole MP3 frames without re-encoding.** 92,128 frames (40.1 min, against the feed's 39:52) are byte-identical in both, in the show segments; the ad breaks — a pre-roll, mid-rolls at about 15 and 31 minutes, a post-roll — are runs of frames that differ.
- **Every splice point starts on a clean frame** (`main_data_begin = 0`: it borrows nothing from earlier frames through MP3's bit reservoir). Acast encodes each segment on its own, so cutting there leaves no glitch. Only 34 and 29 frames in the whole files start clean: the segment starts.
- Short runs of identical frames also occur inside ad breaks (silence encodes identically, e.g. runs of 3 and 31 frames in the post-rolls). They don't start on a clean frame and are short, which tells them apart from show audio.
- Ads vary over time, not just by region: downloads hours later through a Proton Norway exit carried different ads from the direct Norwegian ones, but byte-identical show audio.

## The MP3 reader (`mp3`)

- Reads ID3v2 (several tags, footer), ID3v1, Xing/Info/VBRI info frames (kept apart from the audio: they describe the uncut file), and MPEG-1/2/2.5 Layer I–III frame headers including CRC and Layer III's `main_data_begin`.
- A header is only believed when another valid one follows it, at the start and after any skipped bytes; the stream's version, layer and sample rate must stay the same. Junk bytes (`Skipped`) and a cut-off last frame (`Truncated`) are counted, never fatal.
- No Solstein dependencies. Fuzzed (21 million inputs: no panic, every byte accounted for as tag, frame, skipped or truncated).

## The diff (`Diff`)

`Diff(home, other, options)` removes from `home` everything it doesn't share with `other`:
1. Parse both; a file that isn't MPEG audio (e.g. AAC in `.m4a`), or two files in different formats, is `ErrUnsupported`.
2. **Hash every frame** and **align:** anchors are groups of 16 frames (~0.4 s) that occur once in each file; the largest in-order set of anchors is kept (longest increasing subsequence), and each is extended into the longest run of equal frames. With about 100,000 frames per episode this avoids a quadratic diff. Runs are verified byte for byte, so a hash collision can't let other audio in.
3. **Trim each run to real segment boundaries:** it must start on a clean frame (or the file's first frame) and end where the next frame starts clean (or the file ends); otherwise it is cut back to just before the last clean frame inside it. This handles breaks that end in identical silence (the run would start early) and that open with an identical jingle (it would end late).
4. **Keep runs of at least `min_shared_seconds`** (default 2 s, about 77 frames). Everything else is ad.
5. **Sanity checks:** at least one run kept, at most `max_removed_share` (default 30%) of the home file removed, and the result within 5% of the feed's `itunes:duration` when it states one; otherwise `ErrImplausible`.
6. **Write:** the home download's ID3v2 tag, the kept frames copied one by one, its ID3v1 tag. No info frame (it would describe the uncut file) and no cut-off last frame.

`ErrIdentical` means the audio is the same (same bytes, or nothing removed from either side).

On the live pair the diff takes under a second and keeps the three show segments (13:22, 15:10, 11:34), removing the four breaks (1:31, 1:00, 0:40, 1:21): 40:06 against the feed's 39:52. Diffing in either direction gives byte-identical show audio, and ffmpeg decodes the result with no errors.

Shared ads stay in: audio that is the same in both regions is kept as show audio, including an ad running in both markets, and a short bumper spliced in at each break (see [`wip.md`](wip.md)). Host-read ads baked into the recording can't be found by any diff.

## The processor

`regiondiff.Processor`, for each episode of a feed it handles:
1. **Download through both exits at the same moment**, with the same User-Agent, so the only difference is the region. If one download fails, the other is cancelled.
2. **Diff.** The home exit's download is the one kept.
3. **Identical audio** means no dynamic ads, or the same campaign in both markets; two files can't tell which. Each of `fallback_exits` is then tried in turn against the home download (skipping any that is part of the feed's own pair). If every region agrees, the episode is kept as it is and noted "no dynamic ads found" — with a hint when the file is more than 5% longer than stated, which suggests ads that are the same everywhere.
4. **Errors:** not MP3 or mismatched formats fail for good (`ErrPermanent`); an implausible result, a failed download or a failed fallback is retried with the pipeline's back-off, since the next downloads may carry other ads.
5. The result goes to the pipeline with its duration and a note, e.g. "removed 4m15s of ads in 4 breaks, comparing norway with sweden" ([`episodes.md`](episodes.md)).

Every episode of a processed feed is served cleaned: new episodes are processed before they are published, backlog episodes (and processed files past their retention) are processed on first request with a bounded wait, and the newest `backlog` episodes are processed as soon as a feed is added ([`episodes.md`](episodes.md), [`feeds.md`](feeds.md)). The unprocessed version is served only when processing fails for good and the policy is `publish`.

## Settings

```jsonc
"region_diff": {
  "enabled": false,                  // default for feeds that don't set region_diff
  "exits": ["norway", "sweden"],     // the pair, home region first
  "fallback_exits": ["germany"],     // tried when the pair's downloads are identical
  "min_shared_seconds": 2,
  "max_removed_share": 0.3,
  "on_failure": "publish",           // publish (with ads) | hide
  "backlog": 0                       // newest existing episodes processed when a feed is added
}
```

- In `config.json` only, like the VPN block. `settings.RegionDiff` checks the numbers (`min_shared_seconds` 0.5–60, `max_removed_share` above 0 and at most 1, `backlog` not negative) and `on_failure`; a bad value stops start-up like any bad setting.
- **The pair:** `direct` works as the home side but shows the host the home address and needs only one tunnel; a VPN exit in the home country (e.g. Proton `NO`) gives the same home-region ads without that, over two tunnels, which one Proton key handles. Under `disable_direct`, it has to be a VPN exit.
- **Per feed:** `region_diff` (`on`, `off`, or empty for `enabled`), `region_diff_exits` and `region_diff_on_failure` ([`feeds.md`](feeds.md)). With `enabled: false` and `exits` set, region diff runs only for feeds that switch it on.
- **Start-up** (`regiondiff.Setup`, given the exits that exist): without `exits`, region diff is off and silent. It stays off with a warning when `enabled` has no `exits`, when there aren't exactly two, when one is named twice, or when one doesn't exist (with a specific message for `direct` under `disable_direct`). Unusable fallback exits are dropped with a warning. Region diff never stops start-up. The log states the pair, fallbacks, default and failure policy.

## Verified live with Audiobookshelf (2026-09-25)

Solstein and ABS (v2.36) in Docker; Proton exits `norway` and `sweden` (two keys), `default_exit: norway`, `disable_direct: true`, region diff `["norway", "sweden"]` on for every feed, so nothing left from the host's address but the tunnels. The same "Corner Piece" show:
- All three episodes were backlog, downloaded through ABS and cleaned on request: 40:06 in 7.4 s (after the dropped connection below), 32:31 in 5.0 s with both tunnels opened cold, and the 1:16 trailer in 0.6 s, identical from both regions and kept as it is. No request came near the 20 s limit.
- The cleaned 40-minute episode was **byte-identical to the one diffed from the earlier direct/Sweden downloads**, tag included (38,491,718 bytes), although the Norwegian download came through Proton hours later with different ads (4:15 removed instead of 4:32).
- The feed then served the cleaned length and duration and no source audio URL. ffmpeg decoded all three ABS copies with no errors; ABS stores the audio unchanged (its ffmpeg copy writes its own ID3 tag and adds one Info frame).
- The first request's Norway download failed at once with a bare `EOF`, as the Swedish tunnel opened; the pipeline now retries a request that gets no response ([`episodes.md`](episodes.md)). ABS itself retried the download once with another User-Agent ([`clients.md`](clients.md)).
