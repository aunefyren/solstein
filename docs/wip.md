# Work in progress

Known issues, ideas and roadmap items that aren't scheduled in a build order yet. Decisions and the build orders themselves live in `docs/design.md`; move an item there once it is agreed and scheduled.

## Region diff

### Trim break markers (proposed)

**Issue (2026-09-25):** the cleaned "Corner Piece" episode has no ads left, but the short chime around each ad break is still in it.

**Cause:** the chime is its own spliced segment, and it is identical in both regions, so the diff keeps it as shared audio attached to the show segment next to it. Measured on the live Norway/Sweden pair (`config/live/`):
- It lasts 2.27 s (87 frames) and starts and ends on clean frames (`main_data_begin = 0`), as ads do.
- It appears four times: after the pre-roll (frames 3471–3558), and before the first mid-roll, the second mid-roll and the post-roll (frames 34070–34157, 71199–71286 and 99325–99412). Its audio data is byte-identical at all four.
- There is no chime after the two mid-rolls: the show resumes directly.
- Most likely a break bumper configured in Acast rather than part of the recording, but that can't be proven from the files alone.

**Proposal:** at the edge of a kept run, next to a removed break, drop a segment when all of these hold:
- it starts and ends on clean frames;
- it is shorter than about 5 s;
- the same audio (compared without headers and side information) appears at two or more break edges in the episode.

The repetition rule means a short one-off stretch of real show audio next to a break is never removed. The setting would be `trim_break_markers`, default on. The trade-off: a show that uses its own branded sting at breaks loses that sting too.

**Where:** `modules/regiondiff/diff.go`, after the kept runs are trimmed to segment boundaries and before the sanity checks. Test it with synthetic splices, and assert on the live pair that exactly four 87-frame pieces are dropped.
