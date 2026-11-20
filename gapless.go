package minimp3

// gaplessInfo holds the trimming information extracted from the Xing/Info
// header and the LAME (or compatible) tag of an MP3 stream. All sample
// counts are per channel.
type gaplessInfo struct {
	delay    int // encoder delay in samples, dropped from the start
	padding  int // end padding in samples, dropped from the end
	frames   int // total number of frames from the Xing header, 0 when unknown
	spf      int // samples per frame per channel (1152 for MPEG1, 576 for MPEG2/2.5)
	channels int // channel count taken from the first frame header
}

// maxSyncScan is how far parseXingGapless looks for the first frame sync
// after an optional ID3v2 tag before giving up.
const maxSyncScan = 64 * 1024

// parseXingGapless locates the first MPEG audio frame in data (skipping an
// optional ID3v2 tag) and reads the Xing/Info header together with the LAME
// tag extension that carries the encoder delay and end padding.
//
// The return values are:
//
//	info     the parsed trimming information (only valid when ok is true)
//	ok       true when usable gapless metadata was found
//	needMore true when data is too short to decide; call again with more data
//
// A (false, false) result means the stream has no usable gapless metadata
// (no Xing/Info header, no LAME tag, or contradictory values) and the caller
// should fall back to untrimmed decoding.
func parseXingGapless(data []byte) (info gaplessInfo, ok bool, needMore bool) {
	offset := 0
	if len(data) >= 3 && string(data[:3]) == "ID3" {
		if len(data) < 10 {
			return info, false, true
		}
		// ID3v2 header: "ID3", version (2), flags (1), synchsafe size (4).
		size := int(data[6]&0x7f)<<21 | int(data[7]&0x7f)<<14 |
			int(data[8]&0x7f)<<7 | int(data[9]&0x7f)
		offset = 10 + size
		if data[5]&0x10 != 0 { // footer present
			offset += 10
		}
		if len(data) < offset {
			return info, false, true
		}
	}

	// Find the first frame sync.
	sync := -1
	limit := len(data) - 4
	for i := offset; i <= limit && i < offset+maxSyncScan; i++ {
		if data[i] == 0xff && data[i+1]&0xe0 == 0xe0 {
			sync = i
			break
		}
	}
	if sync < 0 {
		if len(data) < offset+maxSyncScan {
			return info, false, true
		}
		return info, false, false
	}
	h := data[sync:]
	if len(h) < 4 {
		return info, false, true
	}

	version := (h[1] >> 3) & 0x3 // 0: MPEG2.5, 2: MPEG2, 3: MPEG1
	layer := (h[1] >> 1) & 0x3   // 1: Layer III
	if version == 1 || layer != 1 {
		// Reserved version, or not a Layer III stream: no Xing metadata.
		return info, false, false
	}
	protection := h[1] & 0x1
	channelMode := (h[3] >> 6) & 0x3

	info.channels = 2
	if channelMode == 3 {
		info.channels = 1
	}
	// Side information size and samples per frame depend on the MPEG version.
	var sideInfo int
	if version == 3 { // MPEG1
		info.spf = 1152
		sideInfo = 32
		if channelMode == 3 {
			sideInfo = 17
		}
	} else { // MPEG2/2.5
		info.spf = 576
		sideInfo = 17
		if channelMode == 3 {
			sideInfo = 9
		}
	}
	xingOff := 4 + sideInfo
	if protection == 0 { // CRC-16 follows the header
		xingOff += 2
	}
	if len(h) < xingOff+8 {
		return info, false, true
	}
	if tag := string(h[xingOff : xingOff+4]); tag != "Xing" && tag != "Info" {
		return info, false, false
	}
	flags := uint32(h[xingOff+4])<<24 | uint32(h[xingOff+5])<<16 |
		uint32(h[xingOff+6])<<8 | uint32(h[xingOff+7])
	p := xingOff + 8
	if flags&0x1 != 0 { // frame count
		if len(h) < p+4 {
			return info, false, true
		}
		info.frames = int(uint32(h[p])<<24 | uint32(h[p+1])<<16 |
			uint32(h[p+2])<<8 | uint32(h[p+3]))
		p += 4
	}
	if flags&0x2 != 0 { // byte count
		p += 4
	}
	if flags&0x4 != 0 { // TOC
		p += 100
	}
	if flags&0x8 != 0 { // quality
		p += 4
	}
	// The LAME tag follows the Xing fields; the encoder delay and end
	// padding are stored as two 12 bit values at offset 21 of the tag.
	if len(h) < p+24 {
		return info, false, true
	}
	// The encoder string of a LAME compatible tag is printable ASCII;
	// reject anything else to avoid reading garbage as delay/padding.
	for _, c := range h[p : p+9] {
		if c < 0x20 || c > 0x7e {
			return info, false, false
		}
	}
	info.delay = int(h[p+21])<<4 | int(h[p+22])>>4
	info.padding = int(h[p+22]&0x0f)<<8 | int(h[p+23])

	// Contradictory metadata (delay and padding consume the whole stream
	// or more) cannot be trusted; disable trimming for such files.
	if info.frames > 0 && info.delay+info.padding >= info.frames*info.spf {
		return gaplessInfo{}, false, false
	}
	return info, true, false
}

// validSamples returns the number of valid samples per channel described by
// the metadata, or -1 when the frame count is unknown.
func (info gaplessInfo) validSamples() int64 {
	if info.frames <= 0 {
		return -1
	}
	valid := info.frames*info.spf - info.delay - info.padding
	if valid < 0 {
		valid = 0
	}
	return int64(valid)
}

// decoderDelay is the inherent delay of an MP3 decoder in samples. The
// encoder delay recorded in the LAME tag does not include it, so it has to
// be accounted for when trimming the decoded stream.
const decoderDelay = 529

// trims returns the leading and trailing samples per channel to remove from
// the decoded stream (excluding the Xing/Info metadata frame, which is
// dropped separately). The total always equals delay+padding so that the
// valid length matches frames*spf-delay-padding.
func (info gaplessInfo) trims() (leading, trailing int) {
	trailing = info.padding - decoderDelay
	if trailing < 0 {
		trailing = 0
	}
	leading = info.delay + info.padding - trailing
	return
}
