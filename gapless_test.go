package minimp3

import (
	"bytes"
	"io"
	"math"
	"os"
	"testing"
	"time"
)

// gaplessSourceSample generates the deterministic source signal that the
// testdata/gapless_*.mp3 fixtures were encoded from. Keep in sync with the
// fixture generator.
func gaplessSourceSample(rate, channel, i int) float64 {
	t := float64(i) / float64(rate)
	if channel == 0 {
		return 0.55*math.Sin(2*math.Pi*440*t) + 0.15*math.Sin(2*math.Pi*1320*t)
	}
	return 0.55*math.Sin(2*math.Pi*554.37*t) + 0.15*math.Sin(2*math.Pi*1660*t)
}

func gaplessSourcePCM(rate, channels, start, samples int) []int16 {
	pcm := make([]int16, 0, samples*channels)
	for i := start; i < start+samples; i++ {
		for ch := 0; ch < channels; ch++ {
			v := gaplessSourceSample(rate, ch, i) * 32767
			if v > 32767 {
				v = 32767
			}
			if v < -32768 {
				v = -32768
			}
			pcm = append(pcm, int16(v))
		}
	}
	return pcm
}

func bytesToSamples(data []byte) []int16 {
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(data[2*i]) | int16(data[2*i+1])<<8
	}
	return pcm
}

// bestLag finds the alignment in [-window, window] between decoded and
// source with the lowest mean absolute error on the given channel.
func bestLag(decoded, source []int16, channels, channel, window int) (lag int, meanErr float64) {
	meanErr = math.MaxFloat64
	n := 4096
	for l := -window; l <= window; l++ {
		var sum float64
		var count int
		for i := 0; i < n; i++ {
			s := i + l
			if s < 0 || s >= len(source)/channels || i >= len(decoded)/channels {
				continue
			}
			sum += math.Abs(float64(decoded[i*channels+channel]) - float64(source[s*channels+channel]))
			count++
		}
		if count > 0 && sum/float64(count) < meanErr {
			meanErr = sum / float64(count)
			lag = l
		}
	}
	return
}

func meanAbsErr(decoded, source []int16, channels, channel, start, n int) float64 {
	var sum float64
	var count int
	for i := start; i < start+n; i++ {
		if i < 0 || i >= len(decoded)/channels || i >= len(source)/channels {
			continue
		}
		sum += math.Abs(float64(decoded[i*channels+channel]) - float64(source[i*channels+channel]))
		count++
	}
	if count == 0 {
		return math.MaxFloat64
	}
	return sum / float64(count)
}

// silenceRun returns the longest run of near-digital-silence samples on the
// given channel within [start, end).
func silenceRun(pcm []int16, channels, channel, start, end int) (longest int) {
	var run int
	for i := start; i < end && i < len(pcm)/channels; i++ {
		v := pcm[i*channels+channel]
		if v < 4 && v > -4 {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseXingGaplessFixtures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		ok       bool
		channels int
		spf      int
	}{
		{"vbr stereo", "gapless_lame.mp3", true, 2, 1152},
		{"vbr mono", "gapless_mono.mp3", true, 1, 1152},
		{"vbr mpeg2", "gapless_22k.mp3", true, 2, 576},
		{"cbr stereo", "gapless_cbr.mp3", true, 2, 1152},
		{"no xing tag", "gapless_notag.mp3", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, ok, needMore := parseXingGapless(readFixture(t, tc.file))
			if needMore {
				t.Fatal("complete file reported needMore")
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if info.channels != tc.channels || info.spf != tc.spf {
				t.Errorf("channels=%d spf=%d, want %d/%d", info.channels, info.spf, tc.channels, tc.spf)
			}
			if info.frames <= 0 || info.delay <= 0 || info.padding <= 0 {
				t.Errorf("unexpected values: %+v", info)
			}
			if valid := info.validSamples(); valid <= 0 {
				t.Errorf("validSamples = %d", valid)
			}
			t.Logf("%s: %+v valid=%d", tc.name, info, info.validSamples())
		})
	}
}

// buildXingFrame assembles a synthetic first frame carrying a Xing header
// and a LAME tag with the given delay/padding.
func buildXingFrame(frames, delay, padding int, encoder string) []byte {
	frame := []byte{0xff, 0xfb, 0x90, 0x00} // MPEG1 Layer III, no CRC
	frame = append(frame, make([]byte, 32)...) // side information
	frame = append(frame, 'X', 'i', 'n', 'g')
	frame = append(frame, 0, 0, 0, 1) // flags: frames present
	frame = append(frame, byte(frames>>24), byte(frames>>16), byte(frames>>8), byte(frames))
	tag := make([]byte, 24)
	copy(tag, encoder)
	tag[21] = byte(delay >> 4)
	tag[22] = byte(delay&0xf)<<4 | byte(padding>>8)
	tag[23] = byte(padding)
	frame = append(frame, tag...)
	return frame
}

func TestParseXingGaplessEdge(t *testing.T) {
	t.Run("empty input needs more data", func(t *testing.T) {
		if _, _, needMore := parseXingGapless(nil); !needMore {
			t.Error("nil input should report needMore")
		}
	})
	t.Run("truncated ID3 needs more data", func(t *testing.T) {
		data := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 1, 0} // 128 byte tag, absent
		if _, _, needMore := parseXingGapless(data); !needMore {
			t.Error("truncated ID3 should report needMore")
		}
	})
	t.Run("corrupt data without sync", func(t *testing.T) {
		data := bytes.Repeat([]byte{0x11, 0x22, 0x33}, 48000) // > maxSyncScan
		if _, ok, needMore := parseXingGapless(data); ok || needMore {
			t.Errorf("corrupt data: ok=%v needMore=%v, want false/false", ok, needMore)
		}
	})
	t.Run("synthetic tag", func(t *testing.T) {
		info, ok, needMore := parseXingGapless(buildXingFrame(10, 576, 1000, "LAME3.100"))
		if !ok || needMore {
			t.Fatalf("ok=%v needMore=%v", ok, needMore)
		}
		if info.delay != 576 || info.padding != 1000 || info.frames != 10 || info.spf != 1152 || info.channels != 2 {
			t.Errorf("unexpected parse: %+v", info)
		}
		if valid := info.validSamples(); valid != 10*1152-576-1000 {
			t.Errorf("validSamples = %d", valid)
		}
	})
	t.Run("tag behind ID3v2", func(t *testing.T) {
		frame := buildXingFrame(10, 576, 1000, "LAME3.100")
		id3 := []byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 5} // 5 byte tag + footer
		data := append(append(id3, make([]byte, 15)...), frame...)
		info, ok, needMore := parseXingGapless(data)
		if !ok || needMore {
			t.Fatalf("ok=%v needMore=%v", ok, needMore)
		}
		if info.delay != 576 || info.padding != 1000 {
			t.Errorf("unexpected parse: %+v", info)
		}
	})
	t.Run("contradictory frames and delay", func(t *testing.T) {
		// delay+padding consumes the whole stream: reject the metadata.
		if _, ok, _ := parseXingGapless(buildXingFrame(1, 576, 1000, "LAME3.100")); ok {
			t.Error("contradictory delay/padding should be rejected")
		}
	})
	t.Run("non printable encoder string", func(t *testing.T) {
		if _, ok, _ := parseXingGapless(buildXingFrame(10, 576, 1000, "\x00\x01\x02")); ok {
			t.Error("non printable encoder string should be rejected")
		}
	})
	t.Run("reserved version", func(t *testing.T) {
		frame := buildXingFrame(10, 576, 1000, "LAME3.100")
		frame[1] = 0xfa // reserved MPEG version bits
		if _, ok, _ := parseXingGapless(frame); ok {
			t.Error("reserved version should be rejected")
		}
	})
}

func TestDecodeFullGaplessLAME(t *testing.T) {
	file := readFixture(t, "gapless_lame.mp3")

	_, plainPCM, err := DecodeFull(file)
	if err != nil {
		t.Fatal(err)
	}
	dec, pcm, err := DecodeFullGapless(file)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Channels != 2 || dec.SampleRate != 44100 {
		t.Fatalf("channels=%d rate=%d", dec.Channels, dec.SampleRate)
	}

	const wantSamples = 22050
	if dec.ValidSamples != wantSamples {
		t.Errorf("ValidSamples = %d, want %d", dec.ValidSamples, wantSamples)
	}
	if got := len(pcm) / (2 * 2); got != wantSamples {
		t.Fatalf("gapless samples = %d, want %d", got, wantSamples)
	}
	plainSamples := len(plainPCM) / (2 * 2)
	if plainSamples <= wantSamples {
		t.Fatalf("plain decode should include delay+padding, got %d samples", plainSamples)
	}
	t.Logf("plain=%d gapless=%d trimmed=%d", plainSamples, wantSamples, plainSamples-wantSamples)

	source := gaplessSourcePCM(44100, 2, 0, wantSamples)
	decoded := bytesToSamples(pcm)
	for ch := 0; ch < 2; ch++ {
		lag, meanErr := bestLag(decoded, source, 2, ch, 8)
		if lag != 0 {
			t.Errorf("channel %d: best lag = %d, want 0 (mean err %.1f)", ch, lag, meanErr)
		}
		if meanErr > 400 {
			t.Errorf("channel %d: mean abs error %.1f too large", ch, meanErr)
		}
	}
	// First and last samples must line up with the source, not with silence.
	if err := meanAbsErr(decoded, source, 2, 0, 0, 64); err > 400 {
		t.Errorf("first 64 samples mean err %.1f", err)
	}
	if err := meanAbsErr(decoded, source, 2, 1, wantSamples-64, 64); err > 400 {
		t.Errorf("last 64 samples mean err %.1f", err)
	}
}

func TestDecodeFullGaplessChannelLayouts(t *testing.T) {
	for _, tc := range []struct {
		file     string
		rate     int
		channels int
		samples  int
	}{
		{"gapless_mono.mp3", 44100, 1, 22050},
		{"gapless_22k.mp3", 22050, 2, 11025},
	} {
		t.Run(tc.file, func(t *testing.T) {
			dec, pcm, err := DecodeFullGapless(readFixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if dec.Channels != tc.channels || dec.SampleRate != tc.rate {
				t.Fatalf("channels=%d rate=%d", dec.Channels, dec.SampleRate)
			}
			if dec.ValidSamples != int64(tc.samples) {
				t.Errorf("ValidSamples = %d, want %d", dec.ValidSamples, tc.samples)
			}
			if got := len(pcm) / (tc.channels * 2); got != tc.samples {
				t.Fatalf("samples = %d, want %d", got, tc.samples)
			}
			source := gaplessSourcePCM(tc.rate, tc.channels, 0, tc.samples)
			decoded := bytesToSamples(pcm)
			lag, meanErr := bestLag(decoded, source, tc.channels, 0, 8)
			if lag != 0 || meanErr > 400 {
				t.Errorf("lag=%d meanErr=%.1f", lag, meanErr)
			}
		})
	}
}

func TestDecodeFullGaplessNoTag(t *testing.T) {
	file := readFixture(t, "gapless_notag.mp3")
	_, plainPCM, err := DecodeFull(file)
	if err != nil {
		t.Fatal(err)
	}
	dec, pcm, err := DecodeFullGapless(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plainPCM, pcm) {
		t.Error("file without metadata must decode identically in gapless mode")
	}
	if want := int64(len(pcm) / (dec.Channels * 2)); dec.ValidSamples != want {
		t.Errorf("ValidSamples = %d, want %d", dec.ValidSamples, want)
	}
}

func TestDecoderGaplessStreaming(t *testing.T) {
	streamDecode := func(t *testing.T, file []byte) (*Decoder, []byte) {
		t.Helper()
		dec, err := NewDecoderGapless(bytes.NewReader(file))
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		<-dec.Started()

		var out []byte
		buf := make([]byte, 1024)
		deadline := time.Now().Add(30 * time.Second)
		for {
			n, err := dec.Read(buf)
			out = append(out, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("streaming decode did not terminate")
			}
		}
		return dec, out
	}

	t.Run("lame tag", func(t *testing.T) {
		file := readFixture(t, "gapless_lame.mp3")
		_, wantPCM, err := DecodeFullGapless(file)
		if err != nil {
			t.Fatal(err)
		}
		dec, out := streamDecode(t, file)
		if !bytes.Equal(out, wantPCM) {
			t.Errorf("streaming output differs from DecodeFullGapless: %d vs %d bytes", len(out), len(wantPCM))
		}
		if dec.ValidSamples != 22050 {
			t.Errorf("ValidSamples = %d, want 22050", dec.ValidSamples)
		}
	})

	t.Run("no tag passes through", func(t *testing.T) {
		file := readFixture(t, "gapless_notag.mp3")
		_, plainPCM, err := DecodeFull(file)
		if err != nil {
			t.Fatal(err)
		}
		dec, out := streamDecode(t, file)
		if !bytes.Equal(out, plainPCM) {
			t.Errorf("streaming without metadata must match plain decode: %d vs %d bytes", len(out), len(plainPCM))
		}
		if want := int64(len(plainPCM) / 4); dec.ValidSamples != want {
			t.Errorf("ValidSamples = %d, want %d", dec.ValidSamples, want)
		}
	})
}

func TestGaplessConcatenation(t *testing.T) {
	var joined []byte
	for _, name := range []string{"gapless_half1.mp3", "gapless_half2.mp3"} {
		dec, pcm, err := DecodeFullGapless(readFixture(t, name))
		if err != nil {
			t.Fatal(err)
		}
		if dec.ValidSamples != 11025 {
			t.Fatalf("%s: ValidSamples = %d, want 11025", name, dec.ValidSamples)
		}
		joined = append(joined, pcm...)
	}
	const total = 22050
	if got := len(joined) / 4; got != total {
		t.Fatalf("joined samples = %d, want %d", got, total)
	}
	source := gaplessSourcePCM(44100, 2, 0, total)
	decoded := bytesToSamples(joined)
	// The seam between the two files sits at sample 11025; it must not
	// contain a gap of digital silence and must track the source signal.
	const seam = 11025
	for ch := 0; ch < 2; ch++ {
		if run := silenceRun(decoded, 2, ch, seam-256, seam+256); run >= 64 {
			t.Errorf("channel %d: silence run of %d samples at the seam", ch, run)
		}
		if err := meanAbsErr(decoded, source, 2, ch, seam-256, 512); err > 1000 {
			t.Errorf("channel %d: seam mean err %.1f", ch, err)
		}
		if err := meanAbsErr(decoded, source, 2, ch, 0, total); err > 500 {
			t.Errorf("channel %d: overall mean err %.1f", ch, err)
		}
	}
}

func TestDecodeFullGaplessTruncated(t *testing.T) {
	file := readFixture(t, "gapless_lame.mp3")
	_, fullPCM, err := DecodeFullGapless(file)
	if err != nil {
		t.Fatal(err)
	}
	truncated := file[:len(file)-100]

	dec, pcm, err := DecodeFullGapless(truncated)
	if err != nil {
		t.Fatal(err)
	}
	if dec.ValidSamples < 0 || dec.ValidSamples > 22050 {
		t.Errorf("ValidSamples = %d, want within [0, 22050]", dec.ValidSamples)
	}
	if got := len(pcm) / 4; got != int(dec.ValidSamples) {
		t.Errorf("delivered %d samples, ValidSamples = %d", got, dec.ValidSamples)
	}
	// The kept prefix must be identical to the full decode.
	if !bytes.Equal(pcm, fullPCM[:len(pcm)]) {
		t.Error("truncated decode is not a prefix of the full decode")
	}

	// Streaming a truncated file must still terminate.
	stream, err := NewDecoderGapless(bytes.NewReader(truncated))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	buf := make([]byte, 1024)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := stream.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("streaming truncated file did not terminate")
		}
	}
}

// xingDelayOffset returns the offset of the encoder delay/padding field
// inside the LAME tag of the first frame.
func xingDelayOffset(t *testing.T, data []byte) int {
	t.Helper()
	idx := bytes.Index(data, []byte("Xing"))
	if idx < 0 {
		idx = bytes.Index(data, []byte("Info"))
	}
	if idx < 0 {
		t.Fatal("no Xing/Info tag")
	}
	flags := uint32(data[idx+4])<<24 | uint32(data[idx+5])<<16 | uint32(data[idx+6])<<8 | uint32(data[idx+7])
	p := idx + 8
	if flags&0x1 != 0 {
		p += 4
	}
	if flags&0x2 != 0 {
		p += 4
	}
	if flags&0x4 != 0 {
		p += 100
	}
	if flags&0x8 != 0 {
		p += 4
	}
	return p + 21
}

func TestDecodeFullGaplessCorrupt(t *testing.T) {
	t.Run("garbage input", func(t *testing.T) {
		dec, pcm, err := DecodeFullGapless(bytes.Repeat([]byte{0xde, 0xad}, 2048))
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm) != 0 || dec.ValidSamples != 0 {
			t.Errorf("garbage: %d bytes, ValidSamples=%d", len(pcm), dec.ValidSamples)
		}
	})
	t.Run("contradictory metadata disables trimming", func(t *testing.T) {
		file := readFixture(t, "gapless_lame.mp3")
		_, plainPCM, err := DecodeFull(file)
		if err != nil {
			t.Fatal(err)
		}
		// Shrink the Xing frame count so delay+padding exceed the
		// recorded stream length: the metadata must be ignored.
		idx := bytes.Index(file, []byte("Xing"))
		if idx < 0 {
			t.Fatal("no Xing tag in fixture")
		}
		patched := bytes.Clone(file)
		patched[idx+8] = 0 // frames = 1
		patched[idx+9] = 0
		patched[idx+10] = 0
		patched[idx+11] = 1
		dec, pcm, err := DecodeFullGapless(patched)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pcm, plainPCM) {
			t.Error("contradictory metadata should fall back to untrimmed output")
		}
		if want := int64(len(plainPCM) / (dec.Channels * 2)); dec.ValidSamples != want {
			t.Errorf("ValidSamples = %d, want %d", dec.ValidSamples, want)
		}
	})
	t.Run("stream shorter than encoder delay", func(t *testing.T) {
		file := readFixture(t, "gapless_lame.mp3")
		// Keep the first frame (with the Xing header) plus one more
		// frame, and inflate the encoder delay beyond what these frames
		// can produce.
		idx := bytes.Index(file, []byte("Xing"))
		if idx < 0 {
			t.Fatal("no Xing tag in fixture")
		}
		frameStart := idx - 36 // MPEG1 stereo side information
		short := bytes.Clone(file[:frameStart+2*1024])
		delayOff := xingDelayOffset(t, short)
		short[delayOff] = 0xff   // delay = 0xfff = 4095 samples
		short[delayOff+1] |= 0xf0
		dec, pcm, err := DecodeFullGapless(short)
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm) != 0 || dec.ValidSamples != 0 {
			t.Errorf("short stream: %d bytes, ValidSamples=%d, want none", len(pcm), dec.ValidSamples)
		}
	})
}
