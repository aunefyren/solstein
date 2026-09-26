// Copyright 2017 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modified for Solstein (2026): trimmed to reading frames and requantising
// their spectrum, reusing the previous frame's side information and main
// data structures rather than allocating them for every frame. The PCM
// synthesis (reorder, stereo processing,
// antialiasing, IMDCT, subband synthesis) is removed, since Solstein only
// needs each granule's spectral energy (Energies, added), and 2^x in
// requantisation is computed from a table of quarter powers instead of
// math.Pow (pow2Quarter, added). See mp3/spectrum/README.md.

package frame

import (
	"fmt"
	"io"
	"math"

	"aunefyren/solstein/mp3/spectrum/internal/bits"
	"aunefyren/solstein/mp3/spectrum/internal/consts"
	"aunefyren/solstein/mp3/spectrum/internal/frameheader"
	"aunefyren/solstein/mp3/spectrum/internal/maindata"
	"aunefyren/solstein/mp3/spectrum/internal/sideinfo"
)

var (
	powtab34 = make([]float64, 8207)
	pretab   = []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 3, 3, 3, 2, 0}
)

func init() {
	for i := range powtab34 {
		powtab34[i] = math.Pow(float64(i), 4.0/3.0)
	}
}

type Frame struct {
	header   frameheader.FrameHeader
	sideInfo *sideinfo.SideInfo
	mainData *maindata.MainData

	mainDataBits *bits.Bits
}

type FullReader interface {
	ReadFull([]byte) (int, error)
}

func readCRC(source FullReader) error {
	buf := make([]byte, 2)
	if n, err := source.ReadFull(buf); n < 2 {
		if err == io.EOF {
			return &consts.UnexpectedEOF{At: "readCRC"}
		}
		return fmt.Errorf("mp3: error at readCRC: %v", err)
	}
	return nil
}

func Read(source FullReader, position int64, prev *Frame) (frame *Frame, startPosition int64, err error) {
	h, pos, err := frameheader.Read(source, position)
	if err != nil {
		return nil, 0, err
	}

	if h.ProtectionBit() == 0 {
		if err := readCRC(source); err != nil {
			return nil, 0, err
		}
	}

	if h.ID() == consts.Version2_5 {
		return nil, 0, fmt.Errorf("mp3: MPEG version 2.5 is not supported")
	}
	if h.Layer() != consts.Layer3 {
		return nil, 0, fmt.Errorf("mp3: only layer3 (want %d; got %d) is supported", consts.Layer3, h.Layer())
	}

	// The previous frame's structures are reused: only its main data bits
	// (the bit reservoir) are still needed, and they are kept apart.
	var si *sideinfo.SideInfo
	var md *maindata.MainData
	if prev != nil {
		si, md = prev.sideInfo, prev.mainData
	} else {
		si, md = &sideinfo.SideInfo{}, &maindata.MainData{}
	}
	si, err = sideinfo.Read(source, h, si)
	if err != nil {
		return nil, 0, err
	}

	// If there's not enough main data in the bit reservoir,
	// signal to calling function so that decoding isn't done!
	// Get main data (scalefactors and Huffman coded frequency data)
	var prevM *bits.Bits
	if prev != nil {
		prevM = prev.mainDataBits
	}
	md, mdb, err := maindata.Read(source, prevM, h, si, md)
	if err != nil {
		return nil, 0, err
	}
	nf := &Frame{
		header:       h,
		sideInfo:     si,
		mainData:     md,
		mainDataBits: mdb,
	}
	return nf, pos, nil
}

func (f *Frame) SamplingFrequency() (int, error) {
	return f.header.SamplingFrequencyValue()
}

// Granules is how many granules the frame holds: 2 for MPEG-1, 1 for
// MPEG-2.
func (f *Frame) Granules() int {
	return f.header.Granules()
}

// Energies returns the spectral energy of each granule, both channels
// together. Stereo processing is skipped: mid/side coding keeps the total
// energy, and intensity stereo only moves some of it between channels.
func (f *Frame) Energies() []float64 {
	nch := f.header.NumberOfChannels()
	out := make([]float64, f.header.Granules())
	for gr := range out {
		sum := 0.0
		for ch := 0; ch < nch; ch++ {
			f.requantize(gr, ch)
			for _, v := range f.mainData.Is[gr][ch][:f.sideInfo.Count1[gr][ch]] {
				sum += float64(v) * float64(v)
			}
		}
		out[gr] = sum
	}
	return out
}

func (f *Frame) requantizeProcessLong(gr, ch, is_pos, sfb int) {
	sf_mult := 0.5
	if f.sideInfo.ScalefacScale[gr][ch] != 0 {
		sf_mult = 1.0
	}
	pf_x_pt := float64(f.sideInfo.Preflag[gr][ch]) * pretab[sfb]
	idx := -(sf_mult * (float64(f.mainData.ScalefacL[gr][ch][sfb]) + pf_x_pt)) +
		0.25*(float64(f.sideInfo.GlobalGain[gr][ch])-210)
	tmp1 := pow2Quarter(idx)
	tmp2 := 0.0
	if f.mainData.Is[gr][ch][is_pos] < 0.0 {
		tmp2 = -powtab34[int(-f.mainData.Is[gr][ch][is_pos])]
	} else {
		tmp2 = powtab34[int(f.mainData.Is[gr][ch][is_pos])]
	}
	f.mainData.Is[gr][ch][is_pos] = float32(tmp1 * tmp2)
}

func (f *Frame) requantizeProcessShort(gr, ch, is_pos, sfb, win int) {
	sf_mult := 0.5
	if f.sideInfo.ScalefacScale[gr][ch] != 0 {
		sf_mult = 1.0
	}
	idx := -(sf_mult * float64(f.mainData.ScalefacS[gr][ch][sfb][win])) +
		0.25*(float64(f.sideInfo.GlobalGain[gr][ch])-210.0-
			8.0*float64(f.sideInfo.SubblockGain[gr][ch][win]))
	tmp1 := pow2Quarter(idx)
	tmp2 := 0.0
	if f.mainData.Is[gr][ch][is_pos] < 0 {
		tmp2 = -powtab34[int(-f.mainData.Is[gr][ch][is_pos])]
	} else {
		tmp2 = powtab34[int(f.mainData.Is[gr][ch][is_pos])]
	}
	f.mainData.Is[gr][ch][is_pos] = float32(tmp1 * tmp2)
}

func getSfBandIndicesArray(header *frameheader.FrameHeader) ([]int, []int) {
	sfreq := header.SamplingFrequency() // Setup sampling frequency index
	lsf := header.LowSamplingFrequency()
	sfBandIndicesShort := consts.SfBandIndices[lsf][sfreq][consts.SfBandIndicesShort]
	sfBandIndicesLong := consts.SfBandIndices[lsf][sfreq][consts.SfBandIndicesLong]
	return sfBandIndicesLong, sfBandIndicesShort
}

func (f *Frame) requantize(gr int, ch int) {
	sfBandIndicesLong, sfBandIndicesShort := getSfBandIndicesArray(&f.header)
	// Determine type of block to process
	if f.sideInfo.WinSwitchFlag[gr][ch] == 1 && f.sideInfo.BlockType[gr][ch] == 2 { // Short blocks
		// Check if the first two subbands
		// (=2*18 samples = 8 long or 3 short sfb's) uses long blocks
		if f.sideInfo.MixedBlockFlag[gr][ch] != 0 { // 2 longbl. sb  first
			// First process the 2 long block subbands at the start
			sfb := 0
			next_sfb := sfBandIndicesLong[sfb+1]
			for i := 0; i < 36; i++ {
				if i == next_sfb {
					sfb++
					next_sfb = sfBandIndicesLong[sfb+1]
				}
				f.requantizeProcessLong(gr, ch, i, sfb)
			}
			// And next the remaining,non-zero,bands which uses short blocks
			sfb = 3
			next_sfb = sfBandIndicesShort[sfb+1] * 3
			win_len := sfBandIndicesShort[sfb+1] -
				sfBandIndicesShort[sfb]

			for i := 36; i < int(f.sideInfo.Count1[gr][ch]); /* i++ done below! */ {
				// Check if we're into the next scalefac band
				if i == next_sfb {
					sfb++
					next_sfb = sfBandIndicesShort[sfb+1] * 3
					win_len = sfBandIndicesShort[sfb+1] -
						sfBandIndicesShort[sfb]
				}
				for win := 0; win < 3; win++ {
					for j := 0; j < win_len; j++ {
						f.requantizeProcessShort(gr, ch, i, sfb, win)
						i++
					}
				}

			}
		} else { // Only short blocks
			sfb := 0
			next_sfb := sfBandIndicesShort[sfb+1] * 3
			win_len := sfBandIndicesShort[sfb+1] -
				sfBandIndicesShort[sfb]
			for i := 0; i < int(f.sideInfo.Count1[gr][ch]); /* i++ done below! */ {
				// Check if we're into the next scalefac band
				if i == next_sfb {
					sfb++
					next_sfb = sfBandIndicesShort[sfb+1] * 3
					win_len = sfBandIndicesShort[sfb+1] -
						sfBandIndicesShort[sfb]
				}
				for win := 0; win < 3; win++ {
					for j := 0; j < win_len; j++ {
						f.requantizeProcessShort(gr, ch, i, sfb, win)
						i++
					}
				}
			}
		}
	} else { // Only long blocks
		sfb := 0
		next_sfb := sfBandIndicesLong[sfb+1]
		for i := 0; i < int(f.sideInfo.Count1[gr][ch]); i++ {
			if i == next_sfb {
				sfb++
				next_sfb = sfBandIndicesLong[sfb+1]
			}
			f.requantizeProcessLong(gr, ch, i, sfb)
		}
	}
}

// quarters are 2^0, 2^0.25, 2^0.5 and 2^0.75.
var quarters = [4]float64{1, 1.189207115002721, 1.4142135623730951, 1.681792830507429}

// pow2Quarter is 2^x for x a multiple of 0.25, which every requantisation
// exponent is; math.Pow took half the decoding time.
func pow2Quarter(x float64) float64 {
	k := int(math.Round(x * 4))
	return math.Ldexp(quarters[k&3], k>>2)
}
