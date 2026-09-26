// Package spectrum measures an MP3 file's loudness over time without
// decoding it to sound: it reads each frame's compressed spectrum (Huffman
// decoding and requantisation) and sums its energy per granule. That is
// what lining up two differently encoded copies of an episode needs, at a
// sixth of the cost of a full decode, which spends most of its time turning
// the spectrum back into samples.
//
// The frame decoding under internal/ is taken from github.com/hajimehoshi/
// go-mp3 (Apache License 2.0) and trimmed; see README.md in this directory.
// Like package mp3, it knows nothing of the rest of Solstein.
package spectrum

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"aunefyren/solstein/mp3"
	"aunefyren/solstein/mp3/spectrum/internal/frame"
)

// ErrUnsupported means the file isn't one this package can measure: not
// MPEG-1 or MPEG-2 Layer III, or too damaged to read.
var ErrUnsupported = errors.New("can't measure this audio")

// maxFailedShare is how much of a file may fail to decode before the file
// is refused rather than measured with gaps.
const maxFailedShare = 0.05

// Loudness is a file's spectral energy per granule (576 samples), in order,
// for the frames of an mp3.File.
type Loudness struct {
	// Energies has one value per granule; frame i's granules start at
	// FrameStarts[i].
	Energies    []float64
	FrameStarts []int
	// Granule is how long one granule plays.
	Granule time.Duration
}

// Measure reads the spectral energy of every frame in file. A frame that
// can't be decoded (damaged, or its bit reservoir is missing) counts as
// silent; more than a few of them and the file is refused. It never panics
// on bad data: this is downloaded, untrusted input. It stops with ctx's
// error when ctx is done.
func Measure(ctx context.Context, file mp3.File) (loudness Loudness, err error) {
	if len(file.Frames) == 0 {
		return Loudness{}, fmt.Errorf("%w: no audio frames", ErrUnsupported)
	}
	first := file.Frames[0].Header
	if first.Layer != 3 || first.Version == mp3.MPEG25 {
		return Loudness{}, fmt.Errorf("%w: %s layer %d", ErrUnsupported, first.Version, first.Layer)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			loudness, err = Loudness{}, fmt.Errorf("%w: decoding failed: %v", ErrUnsupported, recovered)
		}
	}()

	loudness.FrameStarts = make([]int, len(file.Frames))
	loudness.Granule = time.Duration(576) * time.Second / time.Duration(first.SampleRate)
	var previous *frame.Frame
	failed := 0
	for i, parsed := range file.Frames {
		if i%4096 == 0 && ctx.Err() != nil {
			return Loudness{}, ctx.Err()
		}
		loudness.FrameStarts[i] = len(loudness.Energies)
		granules := 2
		if parsed.Header.Version != mp3.MPEG1 {
			granules = 1
		}
		decoded, _, readErr := frame.Read(&frameSource{data: file.FrameBytes(parsed)}, 0, previous)
		if readErr != nil {
			failed++
			previous = nil
			loudness.Energies = append(loudness.Energies, make([]float64, granules)...)
			continue
		}
		previous = decoded
		loudness.Energies = append(loudness.Energies, decoded.Energies()...)
	}
	if share := float64(failed) / float64(len(file.Frames)); share > maxFailedShare {
		return Loudness{}, fmt.Errorf("%w: %.0f%% of the frames can't be decoded", ErrUnsupported, share*100)
	}
	return loudness, nil
}

// silenceFloor is added to energies before taking their log, so silence is
// a finite floor (about 90 dB below full scale, where a granule's energy is
// up to its 576 coefficients at 1.0) rather than minus infinity.
const silenceFloor = 1e-7

// Envelope resamples the loudness to one value per step from the start of
// the first frame: the log energy of the granules that start in that step,
// so files with different sample rates can be compared on one time scale.
func (loudness Loudness) Envelope(step time.Duration) []float64 {
	if len(loudness.Energies) == 0 || step <= 0 {
		return nil
	}
	total := time.Duration(len(loudness.Energies)) * loudness.Granule
	envelope := make([]float64, int(total/step))
	sums, counts := make([]float64, len(envelope)), make([]int, len(envelope))
	for g, energy := range loudness.Energies {
		index := int(time.Duration(g) * loudness.Granule / step)
		if index < len(envelope) {
			sums[index] += energy
			counts[index]++
		}
	}
	last := math.Log(silenceFloor)
	for i := range envelope {
		if counts[i] > 0 {
			last = math.Log(sums[i]/float64(counts[i]) + silenceFloor)
		}
		// A step shorter than a granule gets none of its own: it repeats
		// the one before.
		envelope[i] = last
	}
	return envelope
}

// frameSource hands one frame's bytes to the decoder.
type frameSource struct {
	data     []byte
	position int
}

func (source *frameSource) ReadFull(buffer []byte) (int, error) {
	n := copy(buffer, source.data[source.position:])
	source.position += n
	if n < len(buffer) {
		return n, io.EOF
	}
	return n, nil
}
