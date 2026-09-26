package mp3

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// headerBytes builds a frame header. version: 3 = MPEG-1, 2 = MPEG-2,
// 0 = MPEG-2.5 (the raw field); layer: 1-3.
func headerBytes(version, layer, bitrateIndex, sampleRateIndex byte, padding, protected, mono bool) []byte {
	raw := []byte{0xFF, 0xE0 | version<<3 | (4-layer)<<1, bitrateIndex<<4 | sampleRateIndex<<2, 0}
	if !protected {
		raw[1] |= 1
	}
	if padding {
		raw[2] |= 2
	}
	if mono {
		raw[3] = 3 << 6
	}
	return raw
}

// frame builds a whole frame from a header, with main_data_begin set and
// the rest filled with fill.
func frame(t *testing.T, raw []byte, mainDataBegin int, fill byte) []byte {
	t.Helper()
	header, ok := ParseHeader(raw)
	if !ok {
		t.Fatalf("test header % x doesn't parse", raw)
	}
	data := bytes.Repeat([]byte{fill}, header.Size())
	copy(data, raw)
	start := 4
	if header.Protected {
		data[4], data[5] = 0, 0 // CRC
		start = 6
	}
	if header.Version == MPEG1 {
		data[start] = byte(mainDataBegin >> 1)
		data[start+1] = byte(mainDataBegin&1) << 7
	} else {
		data[start] = byte(mainDataBegin)
	}
	return data
}

// standard is MPEG-1 Layer III, 128 kbit/s, 44.1 kHz, as Acast serves.
func standard(padding bool) []byte {
	return headerBytes(3, 3, 9, 0, padding, false, false)
}

func frames(t *testing.T, count int) []byte {
	t.Helper()
	var data []byte
	for i := range count {
		data = append(data, frame(t, standard(i%2 == 1), i%200, byte(0x10+i%7))...)
	}
	return data
}

func id3v2Tag(payload int) []byte {
	tag := []byte{'I', 'D', '3', 4, 0, 0, byte(payload >> 21 & 0x7F), byte(payload >> 14 & 0x7F), byte(payload >> 7 & 0x7F), byte(payload & 0x7F)}
	return append(tag, bytes.Repeat([]byte{'x'}, payload)...)
}

func id3v1Tag() []byte {
	return append([]byte("TAG"), bytes.Repeat([]byte{' '}, 125)...)
}

func TestParseHeader(t *testing.T) {
	cases := []struct {
		name        string
		raw         []byte
		version     Version
		layer       int
		bitrate     int
		sampleRate  int
		size        int
		samples     int
		sideInfo    int
		protected   bool
		mono        bool
		paddingSize int
	}{
		{"MPEG-1 Layer III 128k", standard(false), MPEG1, 3, 128, 44100, 417, 1152, 32, false, false, 418},
		{"MPEG-1 Layer III mono CRC", headerBytes(3, 3, 9, 0, false, true, true), MPEG1, 3, 128, 44100, 417, 1152, 17, true, true, 418},
		{"MPEG-2 Layer III 64k", headerBytes(2, 3, 8, 0, false, false, false), MPEG2, 3, 64, 22050, 208, 576, 17, false, false, 209},
		{"MPEG-2.5 Layer III 32k", headerBytes(0, 3, 4, 2, false, false, true), MPEG25, 3, 32, 8000, 288, 576, 9, false, true, 289},
		{"MPEG-1 Layer II 192k", headerBytes(3, 2, 10, 1, false, false, false), MPEG1, 2, 192, 48000, 576, 1152, 0, false, false, 577},
		{"MPEG-1 Layer I 384k", headerBytes(3, 1, 12, 0, false, false, false), MPEG1, 1, 384, 44100, 416, 384, 0, false, false, 420},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header, ok := ParseHeader(c.raw)
			if !ok {
				t.Fatal("didn't parse")
			}
			if header.Version != c.version || header.Layer != c.layer || header.Bitrate != c.bitrate || header.SampleRate != c.sampleRate ||
				header.Protected != c.protected || header.Mono != c.mono {
				t.Errorf("header = %+v", header)
			}
			if header.Size() != c.size || header.Samples() != c.samples {
				t.Errorf("size %d samples %d, want %d %d", header.Size(), header.Samples(), c.size, c.samples)
			}
			if c.layer == 3 && header.sideInfoSize() != c.sideInfo {
				t.Errorf("side info %d, want %d", header.sideInfoSize(), c.sideInfo)
			}
			header.Padding = true
			if header.Size() != c.paddingSize {
				t.Errorf("padded size %d, want %d", header.Size(), c.paddingSize)
			}
		})
	}

	if header, _ := ParseHeader(standard(false)); header.Duration() != 26122448*time.Nanosecond {
		t.Errorf("frame duration %s", header.Duration())
	}

	invalid := map[string][]byte{
		"short":            {0xFF, 0xFB},
		"no sync":          {0xFE, 0xFB, 0x90, 0x00},
		"reserved version": headerBytes(1, 3, 9, 0, false, false, false),
		"reserved layer":   {0xFF, 0xF9, 0x90, 0x00},
		"free bitrate":     headerBytes(3, 3, 0, 0, false, false, false),
		"bad bitrate":      headerBytes(3, 3, 15, 0, false, false, false),
		"bad sample rate":  headerBytes(3, 3, 9, 3, false, false, false),
	}
	for name, raw := range invalid {
		if _, ok := ParseHeader(raw); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseTagsAndFrames(t *testing.T) {
	tag := id3v2Tag(178)
	audio := frames(t, 10)
	data := append(append(append([]byte{}, tag...), audio...), id3v1Tag()...)

	file, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.ID3v2) != 188 || len(file.ID3v1) != 128 || file.Skipped != 0 || file.Info != nil {
		t.Errorf("ID3v2 %d, ID3v1 %d, skipped %d, info %v", len(file.ID3v2), len(file.ID3v1), file.Skipped, file.Info)
	}
	if len(file.Frames) != 10 || file.Frames[0].Offset != 188 || file.Frames[1].Offset != 188+417 || file.Frames[1].Size != 418 {
		t.Fatalf("frames = %+v", file.Frames[:2])
	}
	for i, got := range file.Frames {
		if got.MainDataBegin != i%200 {
			t.Errorf("frame %d main_data_begin = %d, want %d", i, got.MainDataBegin, i%200)
		}
	}
	if !bytes.Equal(file.FrameBytes(file.Frames[0]), audio[:417]) {
		t.Error("FrameBytes wrong")
	}
	if want := 10 * 26122448 * time.Nanosecond; file.Duration() != want {
		t.Errorf("duration %s, want %s", file.Duration(), want)
	}
}

func TestMainDataBeginVariants(t *testing.T) {
	cases := []struct {
		name  string
		raw   []byte
		value int
	}{
		{"MPEG-1, 9 bits", standard(false), 511},
		{"MPEG-1 with CRC", headerBytes(3, 3, 9, 0, false, true, false), 300},
		{"MPEG-2, 8 bits", headerBytes(2, 3, 8, 0, false, false, false), 255},
	}
	for _, c := range cases {
		data := append(frame(t, c.raw, c.value, 0x11), frame(t, c.raw, 0, 0x11)...)
		file, err := Parse(data)
		if err != nil || file.Frames[0].MainDataBegin != c.value || file.Frames[1].MainDataBegin != 0 {
			t.Errorf("%s: %+v, %v", c.name, file.Frames, err)
		}
	}
}

func TestParseResyncsOverJunk(t *testing.T) {
	junk := []byte{0x00, 0xFF, 0xFB, 0x90, 0x44, 0x13, 0x37, 0xFF, 0x00, 0x42}
	data := append(append(frames(t, 5), junk...), frames(t, 5)...)
	file, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Frames) != 10 || file.Skipped != len(junk) {
		t.Errorf("frames %d, skipped %d; want 10 and %d", len(file.Frames), file.Skipped, len(junk))
	}
}

func TestParseIgnoresFalseSync(t *testing.T) {
	// A valid-looking header at the start that isn't followed by another.
	lead := append(standard(false), bytes.Repeat([]byte{0x00}, 50)...)
	// Audio whose payload is full of 0xFF 0xFB, i.e. header look-alikes.
	var audio []byte
	for range 4 {
		f := frame(t, standard(false), 0, 0xFF)
		for i := 10; i+1 < len(f); i += 2 {
			f[i+1] = 0xFB
		}
		audio = append(audio, f...)
	}
	file, err := Parse(append(lead, audio...))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Frames) != 4 || file.Skipped != len(lead) || file.Frames[0].Offset != len(lead) {
		t.Errorf("frames %d, skipped %d, first at %d", len(file.Frames), file.Skipped, file.Frames[0].Offset)
	}
}

func TestParseInfoFrame(t *testing.T) {
	info := frame(t, standard(false), 0, 0)
	copy(info[4+32:], "Info")
	file, err := Parse(append(info, frames(t, 3)...))
	if err != nil {
		t.Fatal(err)
	}
	if file.Info == nil || file.Info.Offset != 0 || len(file.Frames) != 3 || file.Frames[0].Offset != 417 {
		t.Errorf("info %v, frames %d", file.Info, len(file.Frames))
	}

	vbri := frame(t, standard(false), 0, 0)
	copy(vbri[36:], "VBRI")
	if file, _ := Parse(append(vbri, frames(t, 2)...)); file.Info == nil {
		t.Error("VBRI frame not recognised")
	}

	// "Xing" in a later frame is just audio.
	later := frame(t, standard(false), 0, 0)
	copy(later[36:], "Xing")
	if file, _ := Parse(append(frames(t, 2), later...)); file.Info != nil || len(file.Frames) != 3 {
		t.Error("an info marker after the first frame was treated as an info frame")
	}
}

func TestParseTruncatedAndChangedStreams(t *testing.T) {
	audio := frames(t, 4)
	file, err := Parse(audio[:len(audio)-100])
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Frames) != 3 || file.Truncated != 418-100 || file.Skipped != 0 {
		t.Errorf("truncated: frames %d, truncated %d, skipped %d", len(file.Frames), file.Truncated, file.Skipped)
	}

	// Frames in another sample rate after the stream began belong to no
	// stream of this file: skipped.
	other := append(frame(t, headerBytes(3, 3, 9, 1, false, false, false), 0, 0x22), frame(t, headerBytes(3, 3, 9, 1, false, false, false), 0, 0x22)...)
	file, err = Parse(append(frames(t, 3), other...))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Frames) != 3 || file.Skipped != len(other) {
		t.Errorf("format change: frames %d, skipped %d", len(file.Frames), file.Skipped)
	}
}

func TestParseTags(t *testing.T) {
	// Two tags in a row, the second with a footer.
	second := id3v2Tag(20)
	second[5] |= 0x10
	second = append(second, bytes.Repeat([]byte{'f'}, 10)...)
	data := append(append(id3v2Tag(30), second...), frames(t, 2)...)
	if file, err := Parse(data); err != nil || len(file.ID3v2) != 40+40 {
		t.Errorf("ID3v2 = %d bytes, %v; want 80", len(file.ID3v2), err)
	}

	// "ID3" that isn't a tag (not syncsafe) is skipped as junk.
	fake := []byte{'I', 'D', '3', 4, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}
	if file, err := Parse(append(fake, frames(t, 2)...)); err != nil || len(file.ID3v2) != 0 || file.Skipped != 10 {
		t.Errorf("fake tag: ID3v2 %d, skipped %d, %v", len(file.ID3v2), file.Skipped, err)
	}
}

func TestParseNotMP3(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":    nil,
		"html":     []byte("<html><body>Not found</body></html>"),
		"tag only": id3v2Tag(100),
		"zeros":    make([]byte, 5000),
	} {
		if _, err := Parse(data); !errors.Is(err, ErrNotMP3) {
			t.Errorf("%s: err = %v, want ErrNotMP3", name, err)
		}
	}
}

// TestParseLiveDownloads reads the real Acast downloads from the live
// region test, when they are there (config/live is local only).
func TestParseLiveDownloads(t *testing.T) {
	for _, name := range []string{"direct-1.mp3", "sweden-1.mp3"} {
		path := filepath.Join("..", "config", "live", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("%s not present; run the live region test to fetch it", path)
		}
		file, err := Parse(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		clean := 0
		for _, frame := range file.Frames {
			if frame.Header.Version != MPEG1 || frame.Header.Layer != 3 || frame.Header.Bitrate != 128 || frame.Header.SampleRate != 44100 {
				t.Fatalf("%s: unexpected frame %+v", name, frame.Header)
			}
			if frame.MainDataBegin == 0 {
				clean++
			}
		}
		t.Logf("%s: %d frames, %s, ID3v2 %d bytes, info frame %v, skipped %d, cut-off last frame %d bytes, %d frames start clean", name, len(file.Frames),
			file.Duration().Round(time.Second), len(file.ID3v2), file.Info != nil, file.Skipped, file.Truncated, clean)
		// Acast's files: one tag, then nothing but frames, ending with a
		// cut-off one.
		if file.Skipped != 0 || len(file.ID3v2) != 188 || file.Truncated == 0 || file.Truncated >= file.Frames[0].Size+1 {
			t.Errorf("%s: skipped %d, ID3v2 %d, truncated %d", name, file.Skipped, len(file.ID3v2), file.Truncated)
		}
	}
}

// FuzzParse feeds Parse arbitrary bytes: downloaded files are untrusted, so
// it must never panic, and whatever it returns must be consistent.
func FuzzParse(f *testing.F) {
	t := &testing.T{}
	f.Add(frames(t, 3))
	f.Add(append(id3v2Tag(20), frames(t, 2)...))
	f.Add([]byte("ID3\x04\x00\x10\x00\x00\x00\x05xxxxx\xff\xfb\x90\x00"))
	f.Add([]byte{0xFF, 0xFB, 0x90, 0x44, 0xFF, 0xFB})
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(data)
		if err != nil {
			return
		}
		covered := len(file.ID3v2) + len(file.ID3v1) + file.Skipped + file.Truncated
		previousEnd := len(file.ID3v2)
		if file.Info != nil {
			covered += file.Info.Size
		}
		for _, frame := range file.Frames {
			if frame.Offset < previousEnd || frame.Offset+frame.Size > len(data) || frame.Size <= 0 {
				t.Fatalf("frame %+v overlaps or leaves the data (previous end %d, length %d)", frame, previousEnd, len(data))
			}
			previousEnd = frame.Offset + frame.Size
			covered += frame.Size
		}
		if covered != len(data) {
			t.Fatalf("tags, frames, skipped and truncated bytes cover %d of %d bytes", covered, len(data))
		}
	})
}

func TestCRC16(t *testing.T) {
	// The standard check value for this CRC (CRC-16/CMS).
	if got := crc16([]byte("123456789")); got != 0xAEE7 {
		t.Errorf("crc16 = %#04x, want 0xaee7", got)
	}
}

func TestSilentFrame(t *testing.T) {
	for _, protected := range []bool{false, true} {
		raw := headerBytes(3, 3, 9, 0, false, protected, false)
		data := append(frame(t, raw, 300, 0xAB), frame(t, raw, 0, 0xCD)...)
		file, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		original := file.Frames[0]
		silent := file.SilentFrame(original)
		offset := original.Header.mainDataOffset()
		if len(silent) != original.Size || !bytes.Equal(silent[:4], data[:4]) || !bytes.Equal(silent[offset:], data[offset:original.Size]) {
			t.Fatalf("protected=%v: header or audio bytes changed", protected)
		}
		start := 4
		if protected {
			start = 6
			if crc := crc16(append(append([]byte(nil), silent[2:4]...), silent[6:offset]...)); silent[4] != byte(crc>>8) || silent[5] != byte(crc) {
				t.Errorf("CRC not recomputed")
			}
		}
		if !bytes.Equal(silent[start:offset], make([]byte, offset-start)) {
			t.Errorf("protected=%v: side information not zeroed", protected)
		}
		if parsed, err := Parse(append(silent, data[original.Size:]...)); err != nil || parsed.Frames[0].MainDataBegin != 0 {
			t.Errorf("protected=%v: silent frame %+v, %v", protected, parsed.Frames, err)
		}
		if want := original.Size - offset; original.MainDataSize() != want || want <= 0 {
			t.Errorf("main data size %d, want %d", original.MainDataSize(), want)
		}
		if !bytes.Equal(data[:original.Size], file.FrameBytes(original)) {
			t.Error("the original frame was changed")
		}
	}
}

func TestVersionString(t *testing.T) {
	cases := map[Version]string{MPEG1: "MPEG-1", MPEG2: "MPEG-2", MPEG25: "MPEG-2.5", Version(3): "unknown"}
	for version, want := range cases {
		if got := version.String(); got != want {
			t.Errorf("Version(%d).String() = %q, want %q", version, got, want)
		}
	}
}
