# mp3/spectrum

Measures an MP3 file's loudness over time from its compressed spectrum, for region diff's comparison by audio ([`docs/region-diff.md`](../../docs/region-diff.md)).

## Origin and licence

The frame decoding under `internal/` is taken from [go-mp3](https://github.com/hajimehoshi/go-mp3) v0.3.4 by Hajime Hoshi and contributors (Christopher Cooper, Sergei Dudka), under the Apache License 2.0; the licence is in [`LICENSE`](LICENSE) in this directory. Apache-2.0 code may be included in a GPL-3.0 project such as Solstein. Each copied file keeps its copyright header.

It is copied rather than used as a module because go-mp3's public API only decodes to PCM, and 71% of that time is PCM synthesis, which measuring loudness doesn't need: a full decode took 44 s for an 86-minute episode, this takes about 7 s.

Changes from go-mp3 (also noted at the top of `internal/frame/frame.go`, the one file changed in substance):
- Kept: `bits`, `consts`, `frameheader`, `sideinfo`, `huffman`, `maindata`, and from `frame` the frame reading and requantisation.
- Removed: `imdct`, the decoder and source (`decode.go`, `source.go`), and in `frame` everything after requantisation: reordering, stereo processing, antialiasing, hybrid and subband synthesis. Also the tests, examples and the `oto` dependency.
- Added in `frame`: `Energies` (each granule's spectral energy) and `Granules`, and `pow2Quarter`, a table of quarter powers in place of `math.Pow` in requantisation (every exponent there is a multiple of ¼; `math.Pow` took half the remaining time).
- `sideinfo.Read` and `maindata.Read` take the structure to fill (cleared first), and `frame.Read` passes the previous frame's: only its bit reservoir is still needed. Allocating them per frame (11 KB each) churned 5.9 GB through the garbage collector for one pair of 80-minute episodes; reused, the whole comparison allocates about 1 GB, most of it the bit reservoir's per-frame buffers.
- Import paths changed to this directory; `consts.UnexpectedEOF` literals keyed (for `go vet`).

`spectrum.go` and its tests are Solstein's own.

## Updating

There is little to update: go-mp3's last release was in 2022. To take a fix, compare the upstream file with the copy and carry the change over by hand, keeping the notes above current.
