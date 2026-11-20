package minimp3

/*
#define MINIMP3_IMPLEMENTATION

#include "minimp3.h"
#include <stdlib.h>
#include <stdio.h>

int decode(mp3dec_t *dec, mp3dec_frame_info_t *info, unsigned char *data, int *length, unsigned char *decoded, int *decoded_length) {
    int samples;
    short pcm[MINIMP3_MAX_SAMPLES_PER_FRAME];
    samples = mp3dec_decode_frame(dec, data, *length, pcm, info);
    *decoded_length = samples * info->channels * 2;
    *length -= info->frame_bytes;
    unsigned char buffer[samples * info->channels * 2];
    memcpy(buffer, (unsigned char*)&(pcm), sizeof(short) * samples * info->channels);
    memcpy(decoded, buffer, sizeof(short) * samples * info->channels);
    return info->frame_bytes;
}
*/
import "C"
import (
	"context"
	"io"
	"sync"
	"time"
	"unsafe"
)

const maxSamplesPerFrame = 1152 * 2

// Decoder decode the mp3 stream by minimp3
type Decoder struct {
	locker        *sync.Mutex
	data          []byte
	decodedData   []byte
	decode        C.mp3dec_t
	info          C.mp3dec_frame_info_t
	context       context.Context
	contextCancel context.CancelFunc
	SampleRate    int
	Channels      int
	Kbps          int
	Layer         int

	// Gapless reports whether gapless output is enabled. When enabled the
	// encoder delay and end padding recorded in the Xing/LAME tag are
	// trimmed and only valid PCM samples are delivered. Streams without
	// usable gapless metadata are decoded unchanged.
	Gapless bool
	// ValidSamples is the total number of valid samples per channel after
	// gapless trimming. It is 0 when gapless output is disabled and -1
	// while the count is still unknown (e.g. before the stream metadata
	// has been parsed, or when the stream carries no frame count).
	ValidSamples int64

	gaplessParsed  bool // the first frame has been inspected for gapless metadata
	skipInfoFrame  bool // the Xing/Info metadata frame still has to be dropped
	delaySamples   int  // remaining leading samples per channel to drop
	padSamples     int  // trailing samples per channel to hold back and drop at EOF
	paddingDropped bool // the held back padding has been discarded
	deliveredBytes int64
	eof            bool // the input is exhausted and fully decoded

	originalEof bool // if the original reader is EOF, set this to true
}

// BufferSize Decoded data buffer size.
var BufferSize = 1024 * 10

// WaitForDataDuration wait for the data time duration.
var WaitForDataDuration = time.Millisecond * 10

// NewDecoder decode mp3 stream and get a Decoder for read the raw data to play.
func NewDecoder(reader io.Reader) (dec *Decoder, err error) {
	return newDecoder(reader, false)
}

// NewDecoderGapless is like NewDecoder but enables gapless output: the
// encoder delay and end padding stored in the Xing/LAME tag are removed so
// that only valid PCM samples are delivered. Files without usable gapless
// metadata are decoded exactly as with NewDecoder.
func NewDecoderGapless(reader io.Reader) (dec *Decoder, err error) {
	return newDecoder(reader, true)
}

func newDecoder(reader io.Reader, gapless bool) (dec *Decoder, err error) {
	dec = new(Decoder)
	dec.locker = new(sync.Mutex)
	dec.context, dec.contextCancel = context.WithCancel(context.Background())
	dec.decode = C.mp3dec_t{}
	C.mp3dec_init(&dec.decode)
	dec.info = C.mp3dec_frame_info_t{}
	dec.Gapless = gapless
	if gapless {
		dec.ValidSamples = -1
	}
	go func() {
		for {
			select {
			case <-dec.context.Done():
				return
			default:
			}
			dec.locker.Lock()
			pending := len(dec.data) > BufferSize
			dec.locker.Unlock()
			if pending {
				<-time.After(WaitForDataDuration)
				continue
			}
			var data = make([]byte, 512)
			n, readErr := reader.Read(data)
			dec.locker.Lock()
			dec.data = append(dec.data, data[:n]...)
			if readErr != nil {
				dec.originalEof = true
			}
			dec.locker.Unlock()
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-dec.context.Done():
				return
			default:
			}
			dec.locker.Lock()
			if len(dec.decodedData) > BufferSize {
				dec.locker.Unlock()
				<-time.After(WaitForDataDuration)
				continue
			}
			if len(dec.data) == 0 {
				if dec.originalEof {
					dec.eof = true
				}
				dec.locker.Unlock()
				<-time.After(WaitForDataDuration)
				continue
			}
			if dec.Gapless && !dec.gaplessParsed {
				// Inspect the first frame for a Xing/LAME tag before any
				// output is produced. While the data is incomplete the
				// parse is retried on the next round.
				if gi, ok, needMore := parseXingGapless(dec.data); ok {
					// The Xing/Info frame carries metadata only; its
					// decoded output is not part of the audio stream.
					dec.skipInfoFrame = true
					dec.delaySamples, dec.padSamples = gi.trims()
					if valid := gi.validSamples(); valid >= 0 {
						dec.ValidSamples = valid
					}
					dec.gaplessParsed = true
				} else if !needMore {
					dec.gaplessParsed = true
				}
			}
			var decoded = [maxSamplesPerFrame * 2]byte{}
			var decodedLength = C.int(0)
			var length = C.int(len(dec.data))
			frameSize := C.decode(&dec.decode, &dec.info,
				(*C.uchar)(unsafe.Pointer(&dec.data[0])),
				&length, (*C.uchar)(unsafe.Pointer(&decoded[0])),
				&decodedLength)
			if int(frameSize) == 0 {
				if dec.Gapless && dec.originalEof {
					// The remaining data is undecodable (e.g. a truncated
					// last frame); drop it so the stream can reach EOF.
					dec.data = dec.data[:0]
					dec.eof = true
				}
				dec.locker.Unlock()
				<-time.After(WaitForDataDuration)
				continue
			}
			dec.SampleRate = int(dec.info.hz)
			dec.Channels = int(dec.info.channels)
			dec.Kbps = int(dec.info.bitrate_kbps)
			dec.Layer = int(dec.info.layer)
			if dec.Gapless && !dec.gaplessParsed {
				// The first frame was consumed without usable metadata;
				// stop looking and deliver the stream unchanged.
				dec.gaplessParsed = true
			}
			chunk := decoded[:decodedLength]
			if dec.Gapless && dec.skipInfoFrame && len(chunk) > 0 {
				// Drop the output of the Xing/Info metadata frame.
				dec.skipInfoFrame = false
				chunk = chunk[:0]
			}
			if dec.Gapless && dec.delaySamples > 0 && len(chunk) > 0 {
				// Drop the encoder delay from the decoded output.
				bytesPerSample := dec.Channels * 2
				if drop := dec.delaySamples * bytesPerSample; drop >= len(chunk) {
					dec.delaySamples -= len(chunk) / bytesPerSample
					chunk = chunk[:0]
				} else {
					chunk = chunk[drop:]
					dec.delaySamples = 0
				}
			}
			dec.decodedData = append(dec.decodedData, chunk...)
			if int(frameSize) <= len(dec.data) {
				dec.data = dec.data[int(frameSize):]
			}
			if dec.originalEof && len(dec.data) == 0 {
				dec.eof = true
			}
			dec.locker.Unlock()
		}
	}()
	return
}

// Started check the record mp3 stream started ot not.
func (dec *Decoder) Started() (channel chan bool) {
	channel = make(chan bool)
	go func() {
		for {
			select {
			case <-dec.context.Done():
				channel <- false
				return
			default:
			}
			dec.locker.Lock()
			started := len(dec.decodedData) != 0
			dec.locker.Unlock()
			if started {
				channel <- true
				return
			}
			<-time.After(time.Millisecond * 100)
		}
	}()
	return
}

// Read read the raw stream
func (dec *Decoder) Read(data []byte) (n int, err error) {
	for {
		select {
		case <-dec.context.Done(): // if the decoder is stopped, then here should return EOF
			err = io.EOF
			return
		default:
		}
		dec.locker.Lock()
		if len(dec.decodedData) > 0 {
			deliverable := len(dec.decodedData)
			if dec.Gapless && dec.padSamples > 0 && !dec.paddingDropped {
				// Hold back the end padding until the stream is
				// exhausted, then drop it instead of delivering it.
				hold := dec.padSamples * dec.Channels * 2
				if dec.eof {
					keep := len(dec.decodedData) - hold
					if keep < 0 {
						keep = 0
					}
					dec.decodedData = dec.decodedData[:keep]
					dec.paddingDropped = true
					deliverable = keep
				} else if deliverable -= hold; deliverable < 0 {
					deliverable = 0
				}
			}
			if deliverable > 0 {
				n = copy(data, dec.decodedData[:deliverable])
				dec.decodedData = dec.decodedData[n:]
				dec.deliveredBytes += int64(n)
				dec.locker.Unlock()
				return
			}
		}
		if dec.eof {
			if dec.Gapless {
				channels := dec.Channels
				if channels == 0 {
					channels = 1
				}
				if actual := dec.deliveredBytes / int64(channels*2); dec.ValidSamples < 0 || actual < dec.ValidSamples {
					dec.ValidSamples = actual
				}
			}
			dec.locker.Unlock()
			err = io.EOF
			return
		}
		dec.locker.Unlock()
		<-time.After(WaitForDataDuration)
	}
}

// Close stop the decode mp3 stream cycle.
func (dec *Decoder) Close() {
	if dec.contextCancel != nil {
		dec.contextCancel()
	}
}

// DecodeFull put all of the mp3 data to decode.
func DecodeFull(mp3 []byte) (dec *Decoder, decodedData []byte, err error) {
	return decodeFull(mp3, false)
}

// DecodeFullGapless is like DecodeFull but enables gapless output: the
// encoder delay and end padding stored in the Xing/LAME tag are removed so
// that only valid PCM samples are returned, and dec.ValidSamples reports
// their count per channel. Files without usable gapless metadata are
// decoded exactly as with DecodeFull.
func DecodeFullGapless(mp3 []byte) (dec *Decoder, decodedData []byte, err error) {
	return decodeFull(mp3, true)
}

func decodeFull(mp3 []byte, gapless bool) (dec *Decoder, decodedData []byte, err error) {
	dec = new(Decoder)
	dec.decode = C.mp3dec_t{}
	C.mp3dec_init(&dec.decode)
	info := C.mp3dec_frame_info_t{}
	var gi gaplessInfo
	var gaplessOK bool
	if gapless {
		dec.Gapless = true
		dec.ValidSamples = -1
		// The whole stream is available, so a needMore result means the
		// metadata is truncated or corrupt and is simply ignored.
		gi, gaplessOK, _ = parseXingGapless(mp3)
	}
	var length = C.int(len(mp3))
	skipInfoFrame := gaplessOK
	for {
		var decoded = [maxSamplesPerFrame * 2]byte{}
		var decodedLength = C.int(0)
		frameSize := C.decode(&dec.decode,
			&info, (*C.uchar)(unsafe.Pointer(&mp3[0])),
			&length, (*C.uchar)(unsafe.Pointer(&decoded[0])),
			&decodedLength)
		if int(frameSize) == 0 {
			break
		}
		chunk := decoded[:decodedLength]
		if skipInfoFrame && len(chunk) > 0 {
			// The Xing/Info frame carries metadata only; its decoded
			// output is not part of the audio stream.
			skipInfoFrame = false
			chunk = chunk[:0]
		}
		decodedData = append(decodedData, chunk...)
		if int(frameSize) < len(mp3) {
			mp3 = mp3[int(frameSize):]
		}
		dec.SampleRate = int(info.hz)
		dec.Channels = int(info.channels)
		dec.Kbps = int(info.bitrate_kbps)
		dec.Layer = int(info.layer)
	}
	if gapless {
		channels := dec.Channels
		if channels == 0 {
			channels = 1
		}
		bytesPerSample := channels * 2
		total := len(decodedData) / bytesPerSample
		if gaplessOK {
			leading, trailing := gi.trims()
			valid := total - leading - trailing
			// A truncated stream yields fewer samples than the metadata
			// promises; extra frames beyond the recorded count (e.g.
			// appended junk) are trimmed as well.
			if expected := gi.validSamples(); expected >= 0 && int64(valid) > expected {
				valid = int(expected)
			}
			if valid < 0 {
				valid = 0
			}
			start := leading * bytesPerSample
			if start > len(decodedData) {
				start = len(decodedData)
			}
			end := start + valid*bytesPerSample
			if end > len(decodedData) {
				end = len(decodedData)
			}
			decodedData = decodedData[start:end]
			dec.ValidSamples = int64(valid)
		} else {
			dec.ValidSamples = int64(total)
		}
	}
	return
}
