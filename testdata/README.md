# Gapless test fixtures

The `gapless_*.mp3` files are short (0.5 s) MP3 encodes of a deterministic
two-tone signal (see `gaplessSourceSample` in `gapless_test.go`), produced
with LAME 3.101:

``` sh
lame -V4 --noreplaygain -r -s 44.1 --signed --little-endian src44_stereo.pcm gapless_lame.mp3
lame -V4 --noreplaygain -t -r -s 44.1 --signed --little-endian src44_stereo.pcm gapless_notag.mp3
lame -V4 --noreplaygain -m m -r -s 44.1 --signed --little-endian src44_mono.pcm gapless_mono.mp3
lame -V4 --noreplaygain -r -s 22.05 --signed --little-endian src22_stereo.pcm gapless_22k.mp3
lame -V4 --noreplaygain -r -s 44.1 --signed --little-endian src44_half1.pcm gapless_half1.mp3
lame -V4 --noreplaygain -r -s 44.1 --signed --little-endian src44_half2.pcm gapless_half2.mp3
lame --cbr -b 128 --noreplaygain -r -s 44.1 --signed --little-endian src44_stereo.pcm gapless_cbr.mp3
```

- `gapless_lame.mp3`: 44.1 kHz stereo VBR with a Xing/LAME tag.
- `gapless_notag.mp3`: same source, VBR tag disabled (`-t`).
- `gapless_mono.mp3`: mono variant.
- `gapless_22k.mp3`: 22.05 kHz stereo (MPEG2, 576 samples per frame).
- `gapless_half1.mp3` / `gapless_half2.mp3`: first and second half of the
  stereo source, encoded separately, for the concatenation test.
- `gapless_cbr.mp3`: CBR encode carrying an `Info` header.
