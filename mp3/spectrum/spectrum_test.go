package spectrum

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aunefyren/solstein/mp3"
)

// The region-diff fixtures are real Layer III files (MPEG-1 and MPEG-2):
// a synthetic 40-second show with a speech-like rhythm.
func parseFixture(t *testing.T, name string) mp3.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "modules", "regiondiff", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	file, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestMeasure(t *testing.T) {
	for _, c := range []struct {
		name     string
		granules int // per frame
	}{{"home-clean.mp3", 2}, {"other-plain.mp3", 1}, {"home-ad-vbr-stereo.mp3", 2}} {
		file := parseFixture(t, c.name)
		loudness, err := Measure(context.Background(), file)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(loudness.Energies) != c.granules*len(file.Frames) || len(loudness.FrameStarts) != len(file.Frames) || loudness.FrameStarts[1] != c.granules {
			t.Errorf("%s: %d energies, %d frame starts for %d frames", c.name, len(loudness.Energies), len(loudness.FrameStarts), len(file.Frames))
		}
		// Every granule's time adds up to the file's.
		if total := time.Duration(len(loudness.Energies)) * loudness.Granule; total.Round(10*time.Millisecond) != file.Duration().Round(10*time.Millisecond) {
			t.Errorf("%s: granules play %s, frames %s", c.name, total, file.Duration())
		}
		loud, quiet := 0, 0
		for _, energy := range loudness.Energies {
			if energy < 0 || math.IsNaN(energy) {
				t.Fatalf("%s: energy %v", c.name, energy)
			}
			if energy > 1e-3 {
				loud++
			} else {
				quiet++
			}
		}
		// The show's rhythm has both loud stretches and near-pauses.
		if loud == 0 || quiet == 0 {
			t.Errorf("%s: %d loud and %d quiet granules", c.name, loud, quiet)
		}
	}
}

func TestEnvelopeMatchesAcrossSampleRates(t *testing.T) {
	// The same show encoded at 32 and 24 kHz: on one time scale, their
	// loudness rises and falls together.
	first, err := Measure(context.Background(), parseFixture(t, "home-clean.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Measure(context.Background(), parseFixture(t, "other-plain.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	step := 100 * time.Millisecond
	a, b := first.Envelope(step), second.Envelope(step)
	n := min(len(a), len(b))
	if n < 390 || n > 401 {
		t.Fatalf("envelopes of %d and %d steps for 40 s", len(a), len(b))
	}
	if c := correlation(a[:n], b[:n]); c < 0.9 {
		t.Errorf("correlation %.3f, want the two encodings to agree", c)
	}
	if first.Envelope(0) != nil || (Loudness{}).Envelope(step) != nil {
		t.Error("empty envelope expected")
	}
}

func correlation(x, y []float64) float64 {
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx, my = mx/float64(len(x)), my/float64(len(y))
	var sxy, sx, sy float64
	for i := range x {
		sxy += (x[i] - mx) * (y[i] - my)
		sx += (x[i] - mx) * (x[i] - mx)
		sy += (y[i] - my) * (y[i] - my)
	}
	return sxy / math.Sqrt(sx*sy)
}

func TestMeasureStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Measure(ctx, parseFixture(t, "home-clean.mp3")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestMeasureRefusesWhatItCantRead(t *testing.T) {
	if _, err := Measure(context.Background(), mp3.File{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("no frames: %v", err)
	}
	// Frames that parse but hold no real audio data (zeros after the
	// header): mostly undecodable.
	var data []byte
	for range 40 {
		frame := make([]byte, 417)
		copy(frame, []byte{0xFF, 0xFB, 0x90, 0x00})
		for i := 36; i < len(frame); i++ {
			frame[i] = 0xFF
		}
		frame[4] = 0xFF // main_data_begin far beyond any reservoir
		data = append(data, frame...)
	}
	file, err := mp3.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Measure(context.Background(), file); err != nil && !errors.Is(err, ErrUnsupported) {
		t.Errorf("garbage: %v, want ErrUnsupported or a measurement", err)
	}
	// Layer II isn't Layer III.
	layer2 := make([]byte, 0)
	for range 5 {
		frame := make([]byte, 417)
		copy(frame, []byte{0xFF, 0xFD, 0x90, 0x00})
		layer2 = append(layer2, frame...)
	}
	if file, err := mp3.Parse(layer2); err == nil {
		if _, err := Measure(context.Background(), file); !errors.Is(err, ErrUnsupported) {
			t.Errorf("layer II: %v", err)
		}
	}
}

// FuzzMeasure feeds Measure whatever mp3.Parse accepts: downloads are
// untrusted, so it must never panic or report nonsense.
func FuzzMeasure(f *testing.F) {
	for _, name := range []string{"home-clean.mp3", "other-plain.mp3"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "modules", "regiondiff", "testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data[:min(len(data), 4000)])
	}
	f.Add([]byte{0xFF, 0xFB, 0x90, 0x00, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := mp3.Parse(data)
		if err != nil {
			return
		}
		loudness, err := Measure(context.Background(), file)
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("error %v isn't ErrUnsupported", err)
			}
			return
		}
		for _, energy := range loudness.Energies {
			if energy < 0 || math.IsNaN(energy) || math.IsInf(energy, 0) {
				t.Fatalf("energy %v", energy)
			}
		}
		loudness.Envelope(10 * time.Millisecond)
	})
}
