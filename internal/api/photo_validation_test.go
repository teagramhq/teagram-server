//nolint:testpackage // Tests must exercise the private validator before its integration ticket.
package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- fixtures use Telegram's required MD5 checksum.
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

const testPhotoMaxBytes = int64(10 * 1024 * 1024)

func TestPhotoSizeTypeThresholds(t *testing.T) {
	for _, tc := range []struct {
		longSide int
		want     string
	}{
		{longSide: 800, want: "x"},
		{longSide: 801, want: "y"},
		{longSide: 1280, want: "y"},
		{longSide: 1281, want: "w"},
	} {
		t.Run(tc.want+"/"+strconv.Itoa(tc.longSide), func(t *testing.T) {
			if got := photoSizeType(tc.longSide, 1); got != tc.want {
				t.Fatalf("photoSizeType(%d, 1) = %q, want %q", tc.longSide, got, tc.want)
			}
		})
	}
}

func TestPhotoValidateJPEGBaselineAndContentIdentification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		components int
	}{
		{name: "jpeg named png", components: 1},
		{name: "jpeg named jpg", components: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := testSequentialJPEG(640, 480, 0xc0, tc.components)
			got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), testPhotoMD5(body))
			assertPhotoValid(t, got, err, 640, 480)
		})
	}
}

func TestPhotoValidateJPEGProgressive(t *testing.T) {
	body := testProgressiveJPEG(640, 480, 3, 1)
	got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), testPhotoMD5(body))
	assertPhotoValid(t, got, err, 640, 480)
}

func TestPhotoValidateJPEGProgressiveEOBRUNSymbols(t *testing.T) {
	body := testProgressiveJPEGWithEOBRUNSymbols()
	got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
	assertPhotoValid(t, got, err, 1, 1)

	sequential := append([]byte(nil), body...)
	frame := bytes.Index(sequential, []byte{0xff, 0xc2})
	if frame < 0 {
		t.Fatal("progressive frame marker not found")
	}
	sequential[frame+1] = 0xc0
	_, err = validateJPEG(bytes.NewReader(sequential), int64(len(sequential)), "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")
}

func TestPhotoValidateJPEGExtendedFramesAccept16BitQuantization(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame byte
		ss    byte
		se    byte
	}{
		{name: "extended sequential", frame: 0xc1, ss: 0, se: 63},
		{name: "progressive", frame: 0xc2, ss: 0, se: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			components := testPhotoComponents(1)
			body := testJPEG(tc.frame, 8, 640, 480, components, [][]byte{
				testSegment(0xdb, testDQT16(0)),
				testSegment(0xc4, testDHT()),
			}, [][]byte{testSOS(components, tc.ss, tc.se, 0, 0)}, true)
			got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
			assertPhotoValid(t, got, err, 640, 480)
		})
	}
}

func TestPhotoValidateJPEGProgressionAndDCOnly(t *testing.T) {
	for _, body := range [][]byte{testProgressiveRefinementJPEG(), testProgressiveDCOnlyJPEG()} {
		got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
		assertPhotoValid(t, got, err, 640, 480)
	}
}

func TestPhotoValidateJPEGDimensionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		width  uint16
		height uint16
	}{
		{name: "minimum", width: 1, height: 1},
		{name: "dimension sum limit", width: 9000, height: 1000},
		{name: "area limit", width: 4096, height: 4096},
		{name: "ratio limit", width: 20, height: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := testSequentialJPEG(tc.width, tc.height, 0xc0, 1)
			got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
			assertPhotoValid(t, got, err, int(tc.width), int(tc.height))
		})
	}
}

func TestPhotoValidateJPEGDimensionVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		width  uint16
		height uint16
	}{
		{name: "zero width", width: 0, height: 1},
		{name: "zero height", width: 1, height: 0},
		{name: "width ceiling", width: 10001, height: 1},
		{name: "height ceiling", width: 1, height: 10001},
		{name: "dimension sum", width: 9001, height: 1000},
		{name: "aspect ratio", width: 21, height: 1},
		{name: "pixel area", width: 4097, height: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := testSequentialJPEG(tc.width, tc.height, 0xc0, 1)
			_, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
			assertPhotoVerdict(t, err, "PHOTO_INVALID_DIMENSIONS")
		})
	}
}

func TestPhotoValidateJPEGAllocationIndependentOfDimensionsAndBodyLength(t *testing.T) {
	small := testSequentialJPEG(1, 1, 0xc0, 1)
	large := testJPEGWithEncodedSize(9523, 477, int(maxPhotoJPEGBytes))
	allocations := func(body []byte) float64 {
		return testing.AllocsPerRun(1, func() {
			_, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
			if err != nil {
				t.Fatalf("validateJPEG failed: %v", err)
			}
		})
	}

	smallAllocs := allocations(small)
	largeAllocs := allocations(large)
	if largeAllocs != smallAllocs {
		t.Fatalf("allocation count changed with dimensions/body size: small=%v large=%v", smallAllocs, largeAllocs)
	}
}

func TestPhotoValidateJPEGComponentAndSamplingRules(t *testing.T) {
	for _, componentCount := range []int{2, 4} {
		body := testSequentialJPEG(640, 480, 0xc0, componentCount)
		_, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
		assertPhotoVerdict(t, err, "MEDIA_INVALID")
	}

	base := testSequentialJPEG(640, 480, 0xc0, 1)
	sof := bytes.Index(base, []byte{0xff, 0xc0})
	for _, tc := range []struct {
		name     string
		sampling byte
		valid    bool
	}{
		{name: "maximum sampling factor", sampling: 0x44, valid: true},
		{name: "zero horizontal factor", sampling: 0x01},
		{name: "zero vertical factor", sampling: 0x10},
		{name: "factor above four", sampling: 0x51},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := append([]byte(nil), base...)
			body[sof+11] = tc.sampling
			got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
			if tc.valid {
				assertPhotoValid(t, got, err, 640, 480)
				return
			}
			assertPhotoVerdict(t, err, "MEDIA_INVALID")
		})
	}
}

func TestPhotoValidateJPEGContentAndStructureFailures(t *testing.T) {
	base := testSequentialJPEG(640, 480, 0xc0, 1)
	sof := testSOFSegment(0xc0, 8, 640, 480, testPhotoComponents(1))
	unsupportedSOF := testSOFSegment(0xc3, 8, 640, 480, testPhotoComponents(1))
	badLength := []byte{0xff, 0xd8, 0xff, 0xe0, 0xff, 0xff}
	badDHT := testJPEG(0xc0, 8, 640, 480, testPhotoComponents(1), [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHTTable(0, 0, []byte{3}, []byte{0, 1, 2})),
		testSegment(0xc4, testDHTTable(1, 0, []byte{1}, []byte{0})),
	}, [][]byte{testSOS(testPhotoComponents(1), 0, 63, 0, 0)}, true)
	undefinedTable := append([]byte(nil), base...)
	if i := bytes.Index(undefinedTable, []byte{0xff, 0xda}); i >= 0 {
		undefinedTable[i+6] = 0x10
	}
	unsupportedPrecision := append([]byte(nil), base...)
	if i := bytes.Index(unsupportedPrecision, []byte{0xff, 0xc0}); i >= 0 {
		unsupportedPrecision[i+4] = 12
	}
	unsupportedFrame := append([]byte(nil), base...)
	if i := bytes.Index(unsupportedFrame, []byte{0xff, 0xc0}); i >= 0 {
		unsupportedFrame[i+1] = 0xc3
	}
	secondSOF := insertBeforeMarker(base, 0xda, sof)
	missingEOI := base[:len(base)-2]
	trailing := append(append([]byte(nil), base...), 0)
	badDQT := testJPEG(0xc0, 8, 640, 480, testPhotoComponents(1), [][]byte{
		testSegment(0xdb, testDQTWithZero(0)),
		testSegment(0xc4, testDHT()),
	}, [][]byte{testSOS(testPhotoComponents(1), 0, 63, 0, 0)}, true)
	baseline16BitDQT := testJPEG(0xc0, 8, 640, 480, testPhotoComponents(1), [][]byte{
		testSegment(0xdb, testDQT16(0)),
		testSegment(0xc4, testDHT()),
	}, [][]byte{testSOS(testPhotoComponents(1), 0, 63, 0, 0)}, true)
	zero16BitDQT := testJPEG(0xc2, 8, 640, 480, testPhotoComponents(1), [][]byte{
		testSegment(0xdb, testDQT16WithZero(0)),
		testSegment(0xc4, testDHT()),
	}, [][]byte{testSOS(testPhotoComponents(1), 0, 0, 0, 0)}, true)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "png content named jpg", body: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}},
		{name: "gif content named jpg", body: []byte("GIF89a")},
		{name: "text content named jpg", body: []byte("not an image")},
		{name: "truncated segment", body: badLength},
		{name: "unsupported SOF", body: unsupportedFrame},
		{name: "unsupported SOF family", body: unsupportedSOF},
		{name: "unsupported precision", body: unsupportedPrecision},
		{name: "second SOF", body: secondSOF},
		{name: "undefined Huffman table", body: undefinedTable},
		{name: "oversubscribed Huffman table", body: badDHT},
		{name: "invalid quantization table", body: badDQT},
		{name: "baseline 16-bit quantization table", body: baseline16BitDQT},
		{name: "zero 16-bit quantization value", body: zero16BitDQT},
		{name: "two components", body: testSequentialJPEG(640, 480, 0xc0, 2)},
		{name: "four components", body: testSequentialJPEG(640, 480, 0xc0, 4)},
		{name: "missing EOI", body: missingEOI},
		{name: "trailing bytes", body: trailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateJPEG(bytes.NewReader(tc.body), int64(len(tc.body)), "")
			assertPhotoVerdict(t, err, "MEDIA_INVALID")
		})
	}
}

func TestPhotoValidateJPEGSkipsApplicationContents(t *testing.T) {
	body := testSequentialJPEG(640, 480, 0xc0, 1)
	app := testSegment(0xe1, []byte{0x45, 0x78, 0xff, 0xc0, 0, 8, 8, 0, 1, 0, 1, 1, 0xff, 0xda})
	body = insertBeforeMarker(body, 0xdb, app)
	got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), testPhotoMD5(body))
	assertPhotoValid(t, got, err, 640, 480)
}

func TestPhotoValidateJPEGAcceptsCutScanWithAppendedEOI(t *testing.T) {
	body := testSequentialJPEG(640, 480, 0xc0, 1)
	cut := append([]byte(nil), body[:len(body)-3]...)
	cut = append(cut, 0xff, 0xd9)
	got, err := validateJPEG(bytes.NewReader(cut), int64(len(cut)), testPhotoMD5(cut))
	assertPhotoValid(t, got, err, 640, 480)
}

func TestPhotoValidateJPEGRestartMarkers(t *testing.T) {
	valid := testRestartJPEG(true, 0xd0)
	got, err := validateJPEG(bytes.NewReader(valid), int64(len(valid)), "")
	assertPhotoValid(t, got, err, 640, 480)

	for _, body := range [][]byte{testRestartJPEG(false, 0xd0), testRestartJPEG(true, 0xd1)} {
		_, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
		assertPhotoVerdict(t, err, "MEDIA_INVALID")
	}
}

func TestPhotoValidateJPEGMD5(t *testing.T) {
	body := testSequentialJPEG(640, 480, 0xc0, 1)
	tests := []struct {
		name     string
		checksum string
		valid    bool
	}{
		{name: "matching", checksum: testPhotoMD5(body), valid: true},
		{name: "matching uppercase", checksum: strings.ToUpper(testPhotoMD5(body)), valid: true},
		{name: "optional", checksum: "", valid: true},
		{name: "mismatch", checksum: strings.Repeat("0", 32)},
		{name: "invalid hex", checksum: strings.Repeat("g", 32)},
		{name: "invalid length", checksum: "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), tc.checksum)
			if tc.valid {
				assertPhotoValid(t, got, err, 640, 480)
				return
			}
			assertPhotoVerdict(t, err, "MEDIA_INVALID")
		})
	}
}

func TestPhotoValidateJPEGStreamingReaders(t *testing.T) {
	body := testSequentialJPEG(640, 480, 0xc0, 1)
	got, err := validateJPEG(&photoShortReader{reader: bytes.NewReader(body), maxChunk: 3}, int64(len(body)), testPhotoMD5(body))
	assertPhotoValid(t, got, err, 640, 480)

	failure := errors.New("stream cancelled")
	_, err = validateJPEG(&photoFailureReader{err: failure}, 100, "")
	if !errors.Is(err, failure) {
		t.Fatalf("stream error = %v, want propagated %v", err, failure)
	}
	_, err = validateJPEG(&photoFailureReader{err: context.Canceled}, 100, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v, want context.Canceled", err)
	}

	zero := &photoZeroReader{}
	_, err = validateJPEG(zero, 100, "")
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("zero-progress stream error = %v, want %v", err, io.ErrNoProgress)
	}
	if zero.reads > 100 {
		t.Fatalf("zero-progress reads = %d, want at most 100", zero.reads)
	}
}

func TestPhotoValidateJPEGEncodedSizeBoundaries(t *testing.T) {
	base := testSequentialJPEG(1, 1, 0xc0, 1)
	padding := bytes.Repeat([]byte{0x7f}, int(testPhotoMaxBytes)-len(base))
	body := make([]byte, 0, int(testPhotoMaxBytes))
	body = append(body, base[:len(base)-2]...)
	body = append(body, padding...)
	body = append(body, 0xff, 0xd9)
	if int64(len(body)) != testPhotoMaxBytes {
		t.Fatalf("fixture size = %d, want %d", len(body), testPhotoMaxBytes)
	}
	got, err := validateJPEG(bytes.NewReader(body), int64(len(body)), "")
	assertPhotoValid(t, got, err, 1, 1)

	tooLarge := &photoCountingReader{}
	_, err = validateJPEG(tooLarge, testPhotoMaxBytes+1, "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")
	if tooLarge.reads != 0 {
		t.Fatalf("oversized body caused %d reads, want rejection before reading", tooLarge.reads)
	}
	_, err = validateJPEG(bytes.NewReader(nil), 0, "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")

	base = testSequentialJPEG(640, 480, 0xc0, 1)
	_, err = validateJPEG(bytes.NewReader(base), int64(len(base)-1), "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")
	_, err = validateJPEG(bytes.NewReader(base), int64(len(base)+1), "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")
	withUnreportedTrailing := append(append([]byte(nil), base...), 0)
	_, err = validateJPEG(bytes.NewReader(withUnreportedTrailing), int64(len(base)), "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")
}

func TestPhotoValidateJPEGMarkerTableAndScanLimits(t *testing.T) {
	base := testSequentialJPEG(640, 480, 0xc0, 1)
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{name: "64 application markers", body: insertBeforeMarkerMany(base, 0xdb, repeatedSegment(0xe1, 64)), want: ""},
		{name: "65 application markers", body: insertBeforeMarkerMany(base, 0xdb, repeatedSegment(0xe1, 65)), want: "MEDIA_INVALID"},
		{name: "16 quantization segments", body: insertBeforeMarkerMany(base, 0xc0, repeatedSegment(0xdb, 15, testDQT(0))), want: ""},
		{name: "17 quantization segments", body: insertBeforeMarkerMany(base, 0xc0, repeatedSegment(0xdb, 16, testDQT(0))), want: "MEDIA_INVALID"},
		{name: "32 Huffman segments", body: insertBeforeMarkerMany(base, 0xc0, repeatedSegment(0xc4, 31, testDHTTable(0, 0, []byte{1}, []byte{0}))), want: ""},
		{name: "33 Huffman segments", body: insertBeforeMarkerMany(base, 0xc0, repeatedSegment(0xc4, 32, testDHTTable(0, 0, []byte{1}, []byte{0}))), want: "MEDIA_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateJPEG(bytes.NewReader(tc.body), int64(len(tc.body)), "")
			if tc.want == "" {
				assertPhotoValid(t, got, err, 640, 480)
				return
			}
			assertPhotoVerdict(t, err, tc.want)
		})
	}

	validScans := testProgressiveJPEGWithScanCount(64)
	got, err := validateJPEG(bytes.NewReader(validScans), int64(len(validScans)), "")
	assertPhotoValid(t, got, err, 1, 1)
	tooManyScans := testProgressiveJPEGWithScanCount(65)
	_, err = validateJPEG(bytes.NewReader(tooManyScans), int64(len(tooManyScans)), "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")

	markerLimit := testJPEGWithDRIMarkers(2042)
	got, err = validateJPEG(bytes.NewReader(markerLimit), int64(len(markerLimit)), "")
	assertPhotoValid(t, got, err, 1, 1)
	tooManyMarkers := testJPEGWithDRIMarkers(2043)
	_, err = validateJPEG(bytes.NewReader(tooManyMarkers), int64(len(tooManyMarkers)), "")
	assertPhotoVerdict(t, err, "MEDIA_INVALID")

	manyRestartMarkers := testJPEGWithRestartMarkers(1, maxPhotoJPEGMarkers+1)
	got, err = validateJPEG(bytes.NewReader(manyRestartMarkers), int64(len(manyRestartMarkers)), "")
	assertPhotoValid(t, got, err, 1, 1)
}

func FuzzValidatePhotoJPEG(f *testing.F) {
	baseline := testSequentialJPEG(640, 480, 0xc0, 1)
	progressive := testProgressiveJPEG(640, 480, 3, 1)
	cut := append([]byte(nil), baseline[:len(baseline)-3]...)
	cut = append(cut, 0xff, 0xd9)
	for _, seed := range [][]byte{baseline, progressive, cut, []byte("not jpeg"), {0xff, 0xd8, 0xff}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		first, firstErr := validateJPEG(bytes.NewReader(data), int64(len(data)), "")
		second, secondErr := validateJPEG(bytes.NewReader(data), int64(len(data)), "")
		if (firstErr == nil) != (secondErr == nil) {
			t.Fatalf("nondeterministic verdicts: first=%v second=%v", firstErr, secondErr)
		}
		if firstErr != nil {
			var firstVerdict interface{ Verdict() string }
			var secondVerdict interface{ Verdict() string }
			if !errors.As(firstErr, &firstVerdict) || !errors.As(secondErr, &secondVerdict) {
				t.Fatalf("unexpected parser error: first=%v second=%v", firstErr, secondErr)
			}
			if firstVerdict.Verdict() != secondVerdict.Verdict() {
				t.Fatalf("nondeterministic verdicts: %s and %s", firstVerdict.Verdict(), secondVerdict.Verdict())
			}
			return
		}
		assertSamePhotoDimensions(t, first, second)
	})
}

func testSequentialJPEG(width, height uint16, sof byte, componentCount int) []byte {
	components := testPhotoComponents(componentCount)
	return testJPEG(sof, 8, width, height, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	}, [][]byte{testSOS(components, 0, 63, 0, 0)}, true)
}

func testProgressiveJPEG(width, height uint16, componentCount, scans int) []byte {
	components := testPhotoComponents(componentCount)
	prefix := testJPEGPrefix(0xc2, 8, width, height, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	})
	prefix = append(prefix, testSOS(components, 0, 0, 0, 0)...)
	prefix = append(prefix, 0x11)
	acScanCount := min(scans, 63)
	for i := range acScanCount {
		component := components[i%len(components)]
		coefficient := byte(i + 1)
		prefix = append(prefix, testSOS([]byte{component}, coefficient, coefficient, 0, 0)...)
		prefix = append(prefix, 0x11)
	}
	return append(prefix, 0xff, 0xd9)
}

func testProgressiveJPEGWithEOBRUNSymbols() []byte {
	components := testPhotoComponents(1)
	eobrunSymbols := []byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90, 0xa0, 0xb0, 0xc0, 0xd0, 0xe0}
	prefix := testJPEGPrefix(0xc2, 8, 1, 1, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHTTable(0, 0, []byte{1}, []byte{0})),
		testSegment(0xc4, testDHTTable(1, 0, []byte{0, 0, 0, 14}, eobrunSymbols)),
	})
	prefix = append(prefix, testSOS(components, 0, 0, 0, 0)...)
	prefix = append(prefix, 0x11)
	prefix = append(prefix, testSOS(components, 1, 63, 0, 0)...)
	prefix = append(prefix, 0x11)
	return append(prefix, 0xff, 0xd9)
}

func testProgressiveJPEGWithScanCount(count int) []byte {
	components := testPhotoComponents(1)
	prefix := testJPEGPrefix(0xc2, 8, 1, 1, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	})
	for i := range count {
		coefficient := byte(0)
		if i > 0 {
			coefficient = byte(i)
		}
		prefix = append(prefix, testSOS(components, coefficient, coefficient, 0, 0)...)
		prefix = append(prefix, 0x11)
	}
	return append(prefix, 0xff, 0xd9)
}

func testProgressiveRefinementJPEG() []byte {
	components := testPhotoComponents(1)
	prefix := testJPEGPrefix(0xc2, 8, 640, 480, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	})
	prefix = append(prefix, testSOS(components, 0, 0, 0, 1)...)
	prefix = append(prefix, 0x11)
	prefix = append(prefix, testSOS(components, 0, 0, 1, 0)...)
	prefix = append(prefix, 0x11)
	prefix = append(prefix, testSOS(components, 1, 63, 0, 1)...)
	prefix = append(prefix, 0x11)
	prefix = append(prefix, testSOS(components, 1, 63, 1, 0)...)
	prefix = append(prefix, 0x11)
	return append(prefix, 0xff, 0xd9)
}

func testProgressiveDCOnlyJPEG() []byte {
	components := testPhotoComponents(1)
	prefix := testJPEGPrefix(0xc2, 8, 640, 480, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHTTable(0, 0, []byte{1}, []byte{0})),
	})
	prefix = append(prefix, testSOS(components, 0, 0, 0, 0)...)
	prefix = append(prefix, 0x11)
	return append(prefix, 0xff, 0xd9)
}

func testRestartJPEG(withInterval bool, restartMarker byte) []byte {
	components := testPhotoComponents(1)
	prefix := testJPEGPrefix(0xc0, 8, 640, 480, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	})
	if withInterval {
		prefix = append(prefix, testSegment(0xdd, []byte{0, 1})...)
	}
	prefix = append(prefix, testSOS(components, 0, 63, 0, 0)...)
	prefix = append(prefix, 0x11, 0xff, restartMarker, 0x22, 0xff, 0xd9)
	return prefix
}

func testJPEGWithDRIMarkers(count int) []byte {
	components := testPhotoComponents(1)
	prefix := testJPEGPrefix(0xc0, 8, 1, 1, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	})
	for range count {
		prefix = append(prefix, testSegment(0xdd, []byte{0, 0})...)
	}
	prefix = append(prefix, testSOS(components, 0, 63, 0, 0)...)
	prefix = append(prefix, 0x11, 0xff, 0xd9)
	return prefix
}

func testJPEGWithRestartMarkers(driCount, restartCount int) []byte {
	components := testPhotoComponents(1)
	prefix := testJPEGPrefix(0xc0, 8, 1, 1, components, [][]byte{
		testSegment(0xdb, testDQT(0)),
		testSegment(0xc4, testDHT()),
	})
	for i := range driCount {
		interval := []byte{0, 0}
		if i == driCount-1 {
			interval[1] = 1
		}
		prefix = append(prefix, testSegment(0xdd, interval)...)
	}
	prefix = append(prefix, testSOS(components, 0, 63, 0, 0)...)
	for i := range restartCount {
		prefix = append(prefix, 0x11, 0xff, byte(0xd0+i%8))
	}
	prefix = append(prefix, 0x11, 0xff, 0xd9)
	return prefix
}

func testJPEGWithEncodedSize(width, height uint16, size int) []byte {
	body := testSequentialJPEG(width, height, 0xc0, 1)
	if size < len(body) {
		panic("encoded fixture size is too small")
	}
	body = append(body[:len(body)-2], bytes.Repeat([]byte{0x11}, size-len(body))...)
	return append(body, 0xff, 0xd9)
}

func testJPEG(sof, precision byte, width, height uint16, components []byte, beforeSOF, scans [][]byte, entropy bool) []byte {
	prefix := testJPEGPrefix(sof, precision, width, height, components, beforeSOF)
	for _, scan := range scans {
		prefix = append(prefix, scan...)
		if entropy {
			prefix = append(prefix, 0x11, 0x22)
		}
	}
	return append(prefix, 0xff, 0xd9)
}

func testJPEGPrefix(sof, precision byte, width, height uint16, components []byte, beforeSOF [][]byte) []byte {
	body := []byte{0xff, 0xd8}
	for _, segment := range beforeSOF {
		body = append(body, segment...)
	}
	return append(body, testSOFSegment(sof, precision, width, height, components)...)
}

func testSOFSegment(marker, precision byte, width, height uint16, components []byte) []byte {
	payload := make([]byte, 0, 6+3*len(components))
	payload = append(payload, precision, byte((height>>8)&0xff), byte(height&0xff), byte((width>>8)&0xff), byte(width&0xff), byte(len(components)&0xff))
	for _, id := range components {
		payload = append(payload, id, 0x11, 0)
	}
	return testSegment(marker, payload)
}

func testPhotoComponents(count int) []byte {
	components := make([]byte, count)
	for i := range components {
		components[i] = byte(i + 1)
	}
	return components
}

func testSOS(components []byte, ss, se, ah, al byte) []byte {
	payload := make([]byte, 1, 1+2*len(components)+3)
	payload[0] = byte(len(components) & 0xff)
	for _, id := range components {
		payload = append(payload, id, 0)
	}
	payload = append(payload, ss, se, ah<<4|al)
	return testSegment(0xda, payload)
}

func testDQT(id byte) []byte {
	payload := make([]byte, 1, 65)
	payload[0] = id
	for range 64 {
		payload = append(payload, 1)
	}
	return payload
}

func testDQT16(id byte) []byte {
	payload := make([]byte, 1, 129)
	payload[0] = 0x10 | id
	for range 64 {
		payload = append(payload, 0, 1)
	}
	return payload
}

func testDQT16WithZero(id byte) []byte {
	payload := testDQT16(id)
	payload[len(payload)-1] = 0
	return payload
}

func testDQTWithZero(id byte) []byte {
	payload := testDQT(id)
	payload[len(payload)-1] = 0
	return payload
}

func testDHT() []byte {
	payload := testDHTTable(0, 0, []byte{1}, []byte{0})
	return append(payload, testDHTTable(1, 0, []byte{1}, []byte{0})...)
}

func testDHTTable(class, id byte, counts, symbols []byte) []byte {
	payload := make([]byte, 17, 17+len(symbols))
	payload[0] = class<<4 | id
	for i := range 16 {
		if i < len(counts) {
			payload[i+1] = counts[i]
		}
	}
	return append(payload, symbols...)
}

func testSegment(marker byte, payload []byte) []byte {
	length := len(payload) + 2
	return append([]byte{0xff, marker, byte((length >> 8) & 0xff), byte(length & 0xff)}, payload...)
}

func insertBeforeMarker(body []byte, marker byte, insert []byte) []byte {
	return insertBeforeMarkerMany(body, marker, [][]byte{insert})
}

func insertBeforeMarkerMany(body []byte, marker byte, inserts [][]byte) []byte {
	needle := []byte{0xff, marker}
	i := bytes.Index(body, needle)
	if i < 0 {
		panic("marker not found in test fixture")
	}
	result := append([]byte(nil), body[:i]...)
	for _, insert := range inserts {
		result = append(result, insert...)
	}
	return append(result, body[i:]...)
}

func repeatedSegment(marker byte, count int, payload ...[]byte) [][]byte {
	result := make([][]byte, 0, count)
	for range count {
		var data []byte
		if len(payload) > 0 {
			data = payload[0]
		}
		result = append(result, testSegment(marker, data))
	}
	return result
}

func testPhotoMD5(body []byte) string {
	// #nosec G401 -- these fixtures exercise the inputFile MD5 compatibility checksum.
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

func assertPhotoValid(t *testing.T, got photoDimensions, err error, width, height int) {
	t.Helper()
	if err != nil {
		t.Fatalf("validation error = %v, want valid JPEG", err)
	}
	if got.width != width || got.height != height {
		t.Fatalf("dimensions = %dx%d, want %dx%d", got.width, got.height, width, height)
	}
}

func assertSamePhotoDimensions(t *testing.T, first, second photoDimensions) {
	t.Helper()
	if first != second {
		t.Fatalf("nondeterministic dimensions: %+v and %+v", first, second)
	}
}

func assertPhotoVerdict(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("validation succeeded, want %s", want)
	}
	var verdict interface{ Verdict() string }
	if !errors.As(err, &verdict) {
		t.Fatalf("validation error = %v, want typed %s verdict", err, want)
	}
	if got := verdict.Verdict(); got != want {
		t.Fatalf("validation verdict = %s, want %s (%v)", got, want, err)
	}
}

type photoShortReader struct {
	reader   *bytes.Reader
	maxChunk int
}

func (r *photoShortReader) Read(p []byte) (int, error) {
	if len(p) > r.maxChunk {
		p = p[:r.maxChunk]
	}
	return r.reader.Read(p)
}

type photoFailureReader struct {
	err error
}

func (r *photoFailureReader) Read([]byte) (int, error) {
	return 0, r.err
}

type photoZeroReader struct {
	reads int
}

func (r *photoZeroReader) Read([]byte) (int, error) {
	r.reads++
	return 0, nil
}

type photoCountingReader struct {
	reads int
}

func (r *photoCountingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}
