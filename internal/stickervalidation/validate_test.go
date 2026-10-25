package stickervalidation_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/stickervalidation"
)

func TestValidateAcceptsEachFormatForEveryTarget(t *testing.T) {
	targets := []stickervalidation.Target{
		stickervalidation.TargetSticker,
		stickervalidation.TargetMaskSticker,
		stickervalidation.TargetCustomEmoji,
		stickervalidation.TargetSetThumbnail,
	}
	formats := []stickervalidation.Format{stickervalidation.FormatWEBP, stickervalidation.FormatTGS, stickervalidation.FormatWEBM}

	for _, target := range targets {
		for _, format := range formats {
			t.Run(string(target)+"/"+string(format), func(t *testing.T) {
				width, height := targetDimensions(target)
				body := testValidFile(format, width, height)
				got, err := stickervalidation.Validate(body, target)
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				if got.Format != format || got.Width != width || got.Height != height {
					t.Fatalf("Validate() = %+v, want %s %dx%d", got, format, width, height)
				}
				if format == stickervalidation.FormatWEBP && got.Duration != 0 {
					t.Fatalf("WEBP duration = %s, want zero", got.Duration)
				}
				if format != stickervalidation.FormatWEBP && got.Duration != 2*time.Second {
					t.Fatalf("%s duration = %s, want 2s", format, got.Duration)
				}
			})
		}
	}
}

func TestTargetSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   []byte
		target stickervalidation.Target
		want   string
	}{
		{name: "custom emoji WEBP", body: testFileWithPrefix(129*1024, []byte("RIFF\x00\x00\x00\x00WEBP")), target: stickervalidation.TargetCustomEmoji, want: "STICKER_FILE_INVALID"},
		{name: "thumbnail WEBP", body: testFileWithPrefix(129*1024, []byte("RIFF\x00\x00\x00\x00WEBP")), target: stickervalidation.TargetSetThumbnail, want: "STICKER_FILE_SIZE"},
		{name: "custom emoji TGS", body: testFileWithPrefix(33*1024, []byte{0x1f, 0x8b}), target: stickervalidation.TargetCustomEmoji, want: "STICKER_TGS_NOTGS"},
		{name: "thumbnail TGS", body: testFileWithPrefix(33*1024, []byte{0x1f, 0x8b}), target: stickervalidation.TargetSetThumbnail, want: "STICKER_FILE_SIZE"},
		{name: "custom emoji WEBM", body: testFileWithPrefix(33*1024, []byte{0x1a, 0x45, 0xdf, 0xa3}), target: stickervalidation.TargetCustomEmoji, want: "STICKER_VIDEO_NOWEBM"},
		{name: "thumbnail WEBM", body: testFileWithPrefix(33*1024, []byte{0x1a, 0x45, 0xdf, 0xa3}), target: stickervalidation.TargetSetThumbnail, want: "STICKER_FILE_SIZE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := stickervalidation.Validate(tc.body, tc.target)
			assertReason(t, err, tc.want)
		})
	}
}

func TestValidateChecksWEBPHeadersBeforeDecoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "VP8", body: testWEBPChunk(testChunk("VP8 ", testVP8Header(513, 512)))},
		{name: "VP8L", body: testWEBPChunk(testChunk("VP8L", testVP8LHeader(513, 512)))},
		{name: "VP8X", body: testWEBPChunk(testChunk("VP8X", testVP8X(513, 512)), testChunk("VP8L", testVP8LHeader(513, 512)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := stickervalidation.Validate(tc.body, stickervalidation.TargetSticker)
			assertReason(t, err, "STICKER_PNG_DIMENSIONS")
		})
	}
}

func TestValidateChecksExtendedWEBPDimensionsBeforeDecoding(t *testing.T) {
	body := testWEBPChunk(
		testChunk("VP8X", testVP8X(513, 512)),
		testChunk("VP8L", testVP8LHeader(513, 512)),
	)
	_, err := stickervalidation.Validate(body, stickervalidation.TargetSticker)
	assertReason(t, err, "STICKER_PNG_DIMENSIONS")
}

func TestValidateAcceptsExtendedWEBP(t *testing.T) {
	body := testWEBPChunk(
		testChunk("VP8X", testVP8X(512, 512)),
		testChunk("VP8L", testVP8L(512, 512)),
	)
	got, err := stickervalidation.Validate(body, stickervalidation.TargetSticker)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got.Format != stickervalidation.FormatWEBP || got.Width != 512 || got.Height != 512 {
		t.Fatalf("Validate() = %+v, want WEBP 512x512", got)
	}
}

func TestValidateRejectsRequiredMalformedFiles(t *testing.T) {
	const maxInflatedTGSBytes = 1 << 20
	oversizedTGS := testGzip(t, bytes.Repeat([]byte("a"), maxInflatedTGSBytes+1))
	if len(oversizedTGS) > 64*1024 {
		t.Fatalf("gzip bomb fixture is %d bytes, want at most %d", len(oversizedTGS), 64*1024)
	}

	truncatedWEBP := testWEBPChunk(testChunk("VP8L", testVP8LHeader(512, 512)))
	animatedWEBP := testAnimatedWEBP(512, 512)
	imageAssetTGS := testTGS(512, 512, `,"assets":[{"id":"img","w":1,"h":1,"p":"data:image/png;base64,AA=="}]`)
	withExpressionTGS := testTGS(512, 512, `,"layers":[{"x":"value"}]`)
	webmWithAudio := testWEBM(512, 512, 2, true)

	for _, tc := range []struct {
		name   string
		body   []byte
		target stickervalidation.Target
		want   string
	}{
		{name: "TGS inflation cap", body: oversizedTGS, target: stickervalidation.TargetSticker, want: "STICKER_TGS_NOTGS"},
		{name: "truncated WEBP body", body: truncatedWEBP, target: stickervalidation.TargetSticker, want: "STICKER_FILE_INVALID"},
		{name: "WEBM audio track", body: webmWithAudio, target: stickervalidation.TargetSticker, want: "STICKER_VIDEO_NOWEBM"},
		{name: "animated WEBP", body: animatedWEBP, target: stickervalidation.TargetSticker, want: "STICKER_FILE_INVALID"},
		{name: "TGS image asset", body: imageAssetTGS, target: stickervalidation.TargetSticker, want: "STICKER_TGS_NOTGS"},
		{name: "TGS expression", body: withExpressionTGS, target: stickervalidation.TargetSticker, want: "STICKER_TGS_NOTGS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := stickervalidation.Validate(tc.body, tc.target)
			assertReason(t, err, tc.want)
		})
	}
}

func TestValidateEnforcesTargetDimensions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   []byte
		target stickervalidation.Target
	}{
		{name: "sticker WEBP has no 512 side", body: testWEBP(511, 511), target: stickervalidation.TargetSticker},
		{name: "mask sticker WEBM exceeds side", body: testWEBM(513, 512, 2, false), target: stickervalidation.TargetMaskSticker},
		{name: "custom emoji TGS uses sticker canvas", body: testTGS(512, 512, ""), target: stickervalidation.TargetCustomEmoji},
		{name: "thumbnail WEBM is not 100 square", body: testWEBM(100, 99, 2, false), target: stickervalidation.TargetSetThumbnail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := stickervalidation.Validate(tc.body, tc.target)
			assertReason(t, err, "STICKER_PNG_DIMENSIONS")
		})
	}
}

func TestValidateEnforcesDurationAndFrameRate(t *testing.T) {
	tooLongWEBM := testWEBM(512, 512, 3.01, false)
	tooLongTGS := testTGSWithTiming(512, 512, 30, 0, 91, "")
	tooFastTGS := testTGSWithTiming(512, 512, 61, 0, 60, "")

	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{name: "WEBM duration", body: tooLongWEBM, want: "STICKER_VIDEO_DURATION"},
		{name: "TGS duration", body: tooLongTGS, want: "STICKER_TGS_NOTGS"},
		{name: "TGS frame rate", body: tooFastTGS, want: "STICKER_TGS_NOTGS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := stickervalidation.Validate(tc.body, stickervalidation.TargetSticker)
			assertReason(t, err, tc.want)
		})
	}
}

func TestValidateRejectsOversizedInputBeforeParsing(t *testing.T) {
	body := bytes.Repeat([]byte{0}, 512*1024+1)
	_, err := stickervalidation.Validate(body, stickervalidation.TargetSticker)
	assertReason(t, err, "STICKER_FILE_SIZE")
}

func TestValidateTGSInflationAllocationIsBounded(t *testing.T) {
	const maxInflatedTGSBytes = 1 << 20
	body := testGzip(t, bytes.Repeat([]byte("z"), 16*maxInflatedTGSBytes+1))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := stickervalidation.Validate(body, stickervalidation.TargetSticker)
	runtime.ReadMemStats(&after)
	assertReason(t, err, "STICKER_TGS_NOTGS")
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8*maxInflatedTGSBytes {
		t.Fatalf("TGS validation allocated %d bytes for a %d-byte inflate cap", allocated, maxInflatedTGSBytes)
	}
}

func FuzzValidateNeverPanics(f *testing.F) {
	f.Add([]byte(nil), string(stickervalidation.TargetSticker))
	f.Add([]byte("not a sticker"), string(stickervalidation.TargetMaskSticker))
	f.Add(testWEBP(512, 512), string(stickervalidation.TargetSticker))
	f.Add(testTGS(100, 100, ""), string(stickervalidation.TargetCustomEmoji))
	f.Add(testWEBM(100, 100, 2, false), string(stickervalidation.TargetSetThumbnail))
	f.Fuzz(func(t *testing.T, body []byte, targetName string) {
		_, err := stickervalidation.Validate(body, stickervalidation.Target(targetName))
		if err != nil {
			var validationErr *stickervalidation.ValidationError
			if !errors.As(err, &validationErr) || validationErr == nil {
				t.Fatalf("unexpected error type %T", err)
			}
		}
	})
}

func assertReason(t *testing.T, err error, want string) {
	t.Helper()
	var rejected *stickervalidation.ValidationError
	if !errors.As(err, &rejected) {
		t.Fatalf("error = %v, want *stickervalidation.ValidationError with reason %q", err, want)
	}
	if rejected.Reason != want {
		t.Fatalf("rejection reason = %q, want %q", rejected.Reason, want)
	}
}

func targetDimensions(target stickervalidation.Target) (int, int) {
	if target == stickervalidation.TargetCustomEmoji || target == stickervalidation.TargetSetThumbnail {
		return 100, 100
	}
	return 512, 320
}

func testValidFile(format stickervalidation.Format, width, height int) []byte {
	switch format {
	case stickervalidation.FormatWEBP:
		return testWEBP(width, height)
	case stickervalidation.FormatTGS:
		return testTGS(width, height, "")
	case stickervalidation.FormatWEBM:
		return testWEBM(width, height, 2, false)
	default:
		panic("unknown test format")
	}
}

func testTGS(width, height int, extra string) []byte {
	return testTGSWithTiming(width, height, 30, 0, 60, extra)
}

func testTGSWithTiming(width, height int, frameRate, inPoint, outPoint float64, extra string) []byte {
	json := []byte(`{"tgs":1,"w":` + itoa(width) + `,"h":` + itoa(height) + `,"fr":` + ftoa(frameRate) + `,"ip":` + ftoa(inPoint) + `,"op":` + ftoa(outPoint) + extra + `}`)
	return testGzipBytes(json)
}

func testGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	return testGzipBytes(data)
}

func testGzipBytes(data []byte) []byte {
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return out.Bytes()
}

func testWEBP(width, height int) []byte {
	return testWEBPChunk(testChunk("VP8L", testVP8L(width, height)))
}

func testFileWithPrefix(size int, prefix []byte) []byte {
	data := make([]byte, size)
	copy(data, prefix)
	return data
}

func testVP8Header(width, height int) []byte {
	if width < 1 || width > 1<<14-1 || height < 1 || height > 1<<14-1 {
		panic("invalid test VP8 dimensions")
	}
	data := make([]byte, 10)
	copy(data[3:6], []byte{0x9d, 0x01, 0x2a})
	binary.LittleEndian.PutUint16(data[6:8], uint16(width))
	binary.LittleEndian.PutUint16(data[8:10], uint16(height))
	return data
}

func testVP8X(width, height int) []byte {
	if width < 1 || width > 1<<24 || height < 1 || height > 1<<24 {
		panic("invalid test VP8X dimensions")
	}
	data := make([]byte, 10)
	writeUint24(data[4:7], uint32(width-1))
	writeUint24(data[7:10], uint32(height-1))
	return data
}

func testWEBPChunk(chunks ...[]byte) []byte {
	var body bytes.Buffer
	body.WriteString("WEBP")
	for _, chunk := range chunks {
		body.Write(chunk)
	}
	var out bytes.Buffer
	out.WriteString("RIFF")
	if err := binary.Write(&out, binary.LittleEndian, testUint32(body.Len())); err != nil {
		panic(err)
	}
	out.Write(body.Bytes())
	return out.Bytes()
}

func testChunk(fourCC string, payload []byte) []byte {
	var out bytes.Buffer
	out.WriteString(fourCC)
	if err := binary.Write(&out, binary.LittleEndian, testUint32(len(payload))); err != nil {
		panic(err)
	}
	out.Write(payload)
	if len(payload)%2 != 0 {
		out.WriteByte(0)
	}
	return out.Bytes()
}

func testVP8L(width, height int) []byte {
	if width < 1 || width > 1<<14 || height < 1 || height > 1<<14 {
		panic("invalid test VP8L dimensions")
	}
	var bits testBitWriter
	bits.write(0x2f, 8)
	bits.write(uint32(width-1), 14)
	bits.write(uint32(height-1), 14)
	bits.write(0, 1) // Alpha hint.
	bits.write(0, 3) // Version.
	bits.write(0, 1) // No transforms.
	bits.write(0, 1) // No color cache.
	bits.write(0, 1) // One Huffman group.
	for _, symbol := range []uint32{0, 0, 0, 255, 0} {
		bits.write(1, 1) // Simple Huffman tree.
		bits.write(0, 1) // One symbol.
		if symbol > 1 {
			bits.write(1, 1) // Eight-bit symbol value.
			bits.write(symbol, 8)
		} else {
			bits.write(0, 1) // One-bit symbol value.
			bits.write(symbol, 1)
		}
	}
	return bits.bytes()
}

func testVP8LHeader(width, height int) []byte {
	data := testVP8L(width, height)
	return data[:5]
}

type testBitWriter struct {
	data []byte
	bits uint8
}

func (w *testBitWriter) write(value uint32, count uint8) {
	for i := range count {
		if w.bits == 0 {
			w.data = append(w.data, 0)
		}
		w.data[len(w.data)-1] |= byte(value>>i&1) << w.bits
		w.bits = (w.bits + 1) % 8
	}
}

func (w *testBitWriter) bytes() []byte { return w.data }

func testAnimatedWEBP(width, height int) []byte {
	if width < 1 || width > 1<<24 || height < 1 || height > 1<<24 {
		panic("invalid test VP8X dimensions")
	}
	vp8x := make([]byte, 10)
	vp8x[0] = 1 << 1
	writeUint24(vp8x[4:7], uint32(width-1))
	writeUint24(vp8x[7:10], uint32(height-1))
	return testWEBPChunk(testChunk("VP8X", vp8x), testChunk("ANIM", make([]byte, 6)), testChunk("VP8L", testVP8L(width, height)))
}

func writeUint24(dst []byte, value uint32) {
	dst[0] = byte(value & 0xff)
	dst[1] = byte((value >> 8) & 0xff)
	dst[2] = byte((value >> 16) & 0xff)
}

func testWEBM(width, height int, duration float64, withAudio bool) []byte {
	if width < 1 || height < 1 {
		panic("invalid test WEBM dimensions")
	}
	header := testEBMLElement(0x1a45dfa3, testEBMLElement(0x4282, []byte("webm")))
	var durationBytes [8]byte
	binary.BigEndian.PutUint64(durationBytes[:], math.Float64bits(duration*1000))
	info := append(testEBMLElement(0x2ad7b1, []byte{0x0f, 0x42, 0x40}), testEBMLElement(0x4489, durationBytes[:])...)
	video := testEBMLElement(0xae, append(
		append(
			append(testEBMLElement(0xd7, []byte{1}), testEBMLElement(0x83, []byte{1})...),
			testEBMLElement(0x86, []byte("V_VP9"))...),
		testEBMLElement(0xe0, append(testEBMLElement(0xb0, testEBMLUint(uint64(width))), testEBMLElement(0xba, testEBMLUint(uint64(height)))...))...,
	))
	tracks := video
	if withAudio {
		audio := testEBMLElement(0xae, append(
			append(testEBMLElement(0xd7, []byte{2}), testEBMLElement(0x83, []byte{2})...),
			testEBMLElement(0x86, []byte("A_OPUS"))...,
		))
		tracks = append(tracks, audio...)
	}
	segment := append(testEBMLElement(0x1549a966, info), testEBMLElement(0x1654ae6b, tracks)...)
	cluster := append(testEBMLElement(0xe7, []byte{0}), testEBMLElement(0xa3, []byte{0x81, 0, 0, 0x80, 0x80})...)
	segment = append(segment, testEBMLElement(0x1f43b675, cluster)...)
	return append(header, testEBMLElement(0x18538067, segment)...)
}

func testEBMLUint(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	i := 0
	for i < len(encoded)-1 && encoded[i] == 0 {
		i++
	}
	return encoded[i:]
}

func testEBMLElement(id uint32, payload []byte) []byte {
	idLen := 4
	for idLen > 1 && id < 1<<((idLen-1)*8) {
		idLen--
	}
	var out bytes.Buffer
	for shift := (idLen - 1) * 8; shift >= 0; shift -= 8 {
		out.WriteByte(byte((id >> shift) & 0xff))
	}
	size := testUint32(len(payload))
	if size >= 1<<14 {
		panic("test EBML payload is too large")
	}
	if size < 127 {
		out.WriteByte(testByte(0x80 | size))
	} else {
		out.WriteByte(testByte(0x40 | size>>8))
		out.WriteByte(testByte(size & 0xff))
	}
	out.Write(payload)
	return out.Bytes()
}

func testUint32(value int) uint32 {
	if value < 0 {
		panic("negative test value")
	}
	unsigned := uint64(value)
	if unsigned > uint64(^uint32(0)) {
		panic("test value does not fit uint32")
	}
	return uint32(unsigned)
}

func testByte(value uint32) byte {
	if value > 0xff {
		panic("test value does not fit byte")
	}
	return byte(value)
}

func itoa(value int) string {
	return strconv.FormatInt(int64(value), 10)
}

func ftoa(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}
