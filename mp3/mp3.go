// Package mp3 reads the structure of MPEG audio files: tags, info frames and
// every audio frame's header and position. It decodes no audio. It knows
// nothing about the rest of Solstein; region diff uses it to find and cut the
// frames podcast hosts splice ads in with.
package mp3

import (
	"bytes"
	"errors"
	"fmt"
	"time"
)

// ErrNotMP3 means no run of valid MPEG audio frames was found.
var ErrNotMP3 = errors.New("not MPEG audio")

// Version is the MPEG version.
type Version int

const (
	MPEG1  Version = 1
	MPEG2  Version = 2
	MPEG25 Version = 25 // MPEG 2.5, an unofficial extension for low rates
)

func (version Version) String() string {
	switch version {
	case MPEG1:
		return "MPEG-1"
	case MPEG2:
		return "MPEG-2"
	case MPEG25:
		return "MPEG-2.5"
	}
	return "unknown"
}

// Header is a decoded 4-byte frame header.
type Header struct {
	Version    Version
	Layer      int // 1, 2 or 3
	Bitrate    int // kbit/s
	SampleRate int // Hz
	Padding    bool
	Protected  bool // followed by a 16-bit CRC
	Mono       bool // channel mode 3
}

var (
	bitratesMPEG1 = [4][15]int{
		1: {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		2: {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		3: {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	}
	bitratesMPEG2 = [4][15]int{
		1: {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		2: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		3: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	}
	sampleRates = map[Version][3]int{
		MPEG1:  {44100, 48000, 32000},
		MPEG2:  {22050, 24000, 16000},
		MPEG25: {11025, 12000, 8000},
	}
)

// ParseHeader decodes a frame header. It refuses reserved values and the
// "free" bitrate, whose frame size can't be known from the header.
func ParseHeader(raw []byte) (Header, bool) {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1]&0xE0 != 0xE0 {
		return Header{}, false
	}
	var header Header
	switch (raw[1] >> 3) & 3 {
	case 0:
		header.Version = MPEG25
	case 2:
		header.Version = MPEG2
	case 3:
		header.Version = MPEG1
	default:
		return Header{}, false // reserved
	}
	header.Layer = 4 - int((raw[1]>>1)&3)
	if header.Layer == 4 {
		return Header{}, false // reserved
	}
	header.Protected = raw[1]&1 == 0

	bitrateIndex := raw[2] >> 4
	sampleRateIndex := (raw[2] >> 2) & 3
	if bitrateIndex == 0 || bitrateIndex == 15 || sampleRateIndex == 3 {
		return Header{}, false
	}
	if header.Version == MPEG1 {
		header.Bitrate = bitratesMPEG1[header.Layer][bitrateIndex]
	} else {
		header.Bitrate = bitratesMPEG2[header.Layer][bitrateIndex]
	}
	header.SampleRate = sampleRates[header.Version][sampleRateIndex]
	header.Padding = (raw[2]>>1)&1 == 1
	header.Mono = raw[3]>>6 == 3
	return header, true
}

// Size is the whole frame's length in bytes, header included.
func (header Header) Size() int {
	padding := 0
	if header.Padding {
		padding = 1
	}
	bitsPerSecond := header.Bitrate * 1000
	switch {
	case header.Layer == 1:
		return (12*bitsPerSecond/header.SampleRate + padding) * 4
	case header.Layer == 3 && header.Version != MPEG1:
		return 72*bitsPerSecond/header.SampleRate + padding
	default:
		return 144*bitsPerSecond/header.SampleRate + padding
	}
}

// Samples is how many audio samples per channel the frame holds.
func (header Header) Samples() int {
	switch {
	case header.Layer == 1:
		return 384
	case header.Layer == 3 && header.Version != MPEG1:
		return 576
	default:
		return 1152
	}
}

// Duration is how long the frame plays.
func (header Header) Duration() time.Duration {
	return time.Duration(header.Samples()) * time.Second / time.Duration(header.SampleRate)
}

// sideInfoSize is the Layer III side information's length, which follows the
// header (and CRC).
func (header Header) sideInfoSize() int {
	switch {
	case header.Version == MPEG1 && header.Mono:
		return 17
	case header.Version == MPEG1:
		return 32
	case header.Mono:
		return 9
	default:
		return 17
	}
}

// sameStream reports whether two headers can belong to one stream: a file
// doesn't change version, layer or sample rate between frames. Bitrate may
// change (variable bitrate), and so may channel mode.
func (header Header) sameStream(other Header) bool {
	return header.Version == other.Version && header.Layer == other.Layer && header.SampleRate == other.SampleRate
}

// Frame is one audio frame in a file.
type Frame struct {
	Offset int
	Size   int
	Header Header
	// MainDataBegin is how many bytes of this frame's audio data lie in
	// earlier frames (Layer III's bit reservoir). Zero means the frame
	// decodes on its own; a stream spliced at such a frame has no glitch.
	MainDataBegin int
}

// File is an MPEG audio file's structure. Data is the whole file; the other
// fields point into it.
type File struct {
	Data []byte
	// ID3v2 is the tag at the start, empty if there is none. Several
	// consecutive tags are kept together.
	ID3v2 []byte
	// Info is the Xing, Info or VBRI frame, if the file has one. It is not
	// in Frames: it describes the whole file and plays as silence.
	Info *Frame
	// Frames are the audio frames, in order.
	Frames []Frame
	// ID3v1 is the 128-byte tag at the end, empty if there is none.
	ID3v1 []byte
	// Skipped counts bytes that belong to no frame: junk, or tags other
	// than ID3v1/ID3v2.
	Skipped int
	// Truncated is the length of a cut-off last frame, not in Frames.
	// Acast's files end with one.
	Truncated int
}

// MainDataSize is how many bytes of Layer III audio data the frame itself
// carries, after its header, CRC and side information. Frames after it can
// borrow them through the bit reservoir.
func (frame Frame) MainDataSize() int {
	return max(frame.Size-frame.Header.mainDataOffset(), 0)
}

// mainDataOffset is where a Layer III frame's own audio data starts.
func (header Header) mainDataOffset() int {
	offset := 4 + header.sideInfoSize()
	if header.Protected {
		offset += 2
	}
	return offset
}

// SilentFrame returns a copy of a Layer III frame that plays as silence but
// keeps every byte after its side information. The side information is
// zeroed, so the frame claims no audio data of its own and borrows none;
// its bytes stay in the stream for the bit reservoir. Put in place of the
// removed frames just before a cut, it keeps the bytes the next kept frame
// borrows where that frame looks for them, so the cut decodes without a
// glitch. The CRC, if the frame has one, is recomputed.
func (file File) SilentFrame(frame Frame) []byte {
	silent := append([]byte(nil), file.FrameBytes(frame)...)
	header := frame.Header
	sideInfo := 4
	if header.Protected {
		sideInfo += 2
	}
	end := min(sideInfo+header.sideInfoSize(), len(silent))
	clear(silent[sideInfo:end])
	if header.Protected && len(silent) >= end {
		crc := crc16(append(append([]byte(nil), silent[2:4]...), silent[sideInfo:end]...))
		silent[4], silent[5] = byte(crc>>8), byte(crc)
	}
	return silent
}

// crc16 is MPEG audio's frame CRC: polynomial 0x8005, initial value
// 0xFFFF, over the header's last two bytes and the side information.
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, value := range data {
		for bit := 7; bit >= 0; bit-- {
			in := uint16(value>>uint(bit)) & 1
			top := crc >> 15
			crc <<= 1
			if top^in == 1 {
				crc ^= 0x8005
			}
		}
	}
	return crc
}

// FrameBytes returns a frame's bytes.
func (file File) FrameBytes(frame Frame) []byte {
	return file.Data[frame.Offset : frame.Offset+frame.Size]
}

// Duration is the audio frames' total playing time.
func (file File) Duration() time.Duration {
	var total time.Duration
	for _, frame := range file.Frames {
		total += frame.Header.Duration()
	}
	return total
}

// Parse reads a whole MPEG audio file.
func Parse(data []byte) (File, error) {
	file := File{Data: data}
	position := id3v2Length(data)
	file.ID3v2 = data[:position]
	end := len(data)
	if end-position >= 128 && bytes.Equal(data[end-128:end-125], []byte("TAG")) {
		file.ID3v1 = data[end-128:]
		end -= 128
	}

	var stream *Header
	// synced is false at the start and after skipping bytes: a header found
	// then is believed only if another valid one follows it (or the data
	// ends), so a byte pattern inside junk or audio data isn't taken for a
	// frame. Within a run of frames, each follows the last exactly.
	synced := false
	for position+4 <= end {
		header, ok := ParseHeader(data[position:])
		if ok && stream != nil && !header.sameStream(*stream) {
			ok = false
		}
		if !ok {
			position++
			file.Skipped++
			synced = false
			continue
		}
		size := header.Size()
		if position+size > end {
			if synced {
				// A cut-off last frame: note it and stop.
				file.Truncated = end - position
				position = end
				break
			}
			position++
			file.Skipped++
			continue
		}
		if !synced && !confirmed(data[:end], position+size, header) {
			position++
			file.Skipped++
			continue
		}
		synced = true

		frame := Frame{Offset: position, Size: size, Header: header}
		if header.Layer == 3 {
			frame.MainDataBegin = mainDataBegin(data[position:position+size], header)
		}
		position += size
		if stream == nil {
			stream = &header
			if isInfoFrame(file.FrameBytes(frame), header) {
				file.Info = &frame
				continue
			}
		}
		file.Frames = append(file.Frames, frame)
	}
	file.Skipped += max(0, end-position)

	if len(file.Frames) == 0 {
		return File{}, fmt.Errorf("%w: no audio frames found", ErrNotMP3)
	}
	return file, nil
}

// confirmed reports whether a candidate frame at the given end is followed
// by a matching header, or ends the data.
func confirmed(data []byte, next int, header Header) bool {
	if next == len(data) {
		return true
	}
	following, ok := ParseHeader(data[next:])
	return ok && following.sameStream(header) && next+following.Size() <= len(data)
}

// id3v2Length is the length of the ID3v2 tags at the start of data.
func id3v2Length(data []byte) int {
	position := 0
	for len(data)-position >= 10 && bytes.Equal(data[position:position+3], []byte("ID3")) {
		raw := data[position+6 : position+10]
		if raw[0]|raw[1]|raw[2]|raw[3] >= 0x80 {
			break // not syncsafe: not a real tag
		}
		size := 10 + (int(raw[0])<<21 | int(raw[1])<<14 | int(raw[2])<<7 | int(raw[3]))
		if data[position+5]&0x10 != 0 {
			size += 10 // footer
		}
		if position+size > len(data) {
			break
		}
		position += size
	}
	return position
}

// mainDataBegin reads Layer III's back pointer from the side information.
func mainDataBegin(frame []byte, header Header) int {
	start := 4
	if header.Protected {
		start += 2
	}
	if len(frame) < start+2 {
		return 0
	}
	if header.Version == MPEG1 {
		return int(frame[start])<<1 | int(frame[start+1]>>7) // 9 bits
	}
	return int(frame[start]) // 8 bits
}

// isInfoFrame recognises the Xing/Info header (after the side information)
// and the VBRI header (32 bytes after the header) that encoders put in the
// first frame.
func isInfoFrame(frame []byte, header Header) bool {
	if header.Layer != 3 {
		return false
	}
	xing := 4 + header.sideInfoSize()
	if header.Protected {
		xing += 2
	}
	if len(frame) >= xing+4 {
		if tag := string(frame[xing : xing+4]); tag == "Xing" || tag == "Info" {
			return true
		}
	}
	return len(frame) >= 36+4 && string(frame[36:40]) == "VBRI"
}
