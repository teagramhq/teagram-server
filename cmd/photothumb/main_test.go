package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testMaxThumbnail = 65_536
	testMaxStripped  = 2_048
	testPixelCap     = 2_560_000
)

var testWorkerBinary string

func TestMain(m *testing.M) {
	if os.Getenv("PHOTOTHUMB_RESOURCE_LIMIT_PROBE") != "" {
		os.Exit(m.Run())
	}
	testDirectory, err := os.MkdirTemp("", "photothumb-test-")
	if err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "create worker test directory:", err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
	testWorkerBinary = filepath.Join(testDirectory, "photothumb")
	buildContext, cancelBuild := context.WithTimeout(context.Background(), time.Minute)
	build := exec.CommandContext(buildContext, "go", "build", "-o", testWorkerBinary, ".") // #nosec G204 -- fixed local Go build target and generated temporary output path.
	output, buildErr := build.CombinedOutput()
	cancelBuild()
	if buildErr != nil {
		if _, writeErr := fmt.Fprintf(os.Stderr, "build photothumb test binary: %v\n%s", buildErr, output); writeErr != nil {
			os.Exit(2)
		}
		if removeErr := os.RemoveAll(testDirectory); removeErr != nil {
			if _, writeErr := fmt.Fprintln(os.Stderr, "remove worker test directory:", removeErr); writeErr != nil {
				os.Exit(2)
			}
		}
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(testDirectory); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "remove worker test directory:", err); writeErr != nil {
			os.Exit(2)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestWorkerProducesClientCompatibleDerivatives(t *testing.T) {
	input := testJPEG(t, 1600, 1600, false, false)
	output, err := runWorker(t, input, 1600, 1600)
	if err != nil {
		t.Fatalf("worker failed: %v", err)
	}

	m, mw, mh, stripped := parseWorkerFrame(t, output)
	if len(m) == 0 || len(m) > testMaxThumbnail {
		t.Fatalf("m size = %d, want 1..%d", len(m), testMaxThumbnail)
	}
	if mw != 320 || mh != 320 {
		t.Fatalf("m dimensions = %dx%d, want 320x320", mw, mh)
	}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(m)); err != nil || cfg.Width != mw || cfg.Height != mh {
		t.Fatalf("m JPEG config = %+v, %v", cfg, err)
	}
	if !hasBaselineSOF(m) {
		t.Fatal("m is not a baseline JPEG")
	}
	if len(stripped) > testMaxStripped || len(stripped) < 4 {
		t.Fatalf("stripped size = %d, want 4..%d", len(stripped), testMaxStripped)
	}
	if stripped[0] != 1 || stripped[1] != 40 || stripped[2] != 40 {
		t.Fatalf("stripped prefix = %v, want [1 40 40]", stripped[:3])
	}
	preview := decodeStripped(t, stripped)
	if preview.Bounds().Dx() != 40 || preview.Bounds().Dy() != 40 {
		t.Fatalf("reconstructed stripped dimensions = %v, want 40x40", preview.Bounds())
	}
}

func TestWorkerPreservesAspectRatio(t *testing.T) {
	input := testJPEG(t, 1600, 997, false, false)
	output, err := runWorker(t, input, 1600, 997)
	if err != nil {
		t.Fatalf("worker failed: %v", err)
	}

	_, mw, mh, stripped := parseWorkerFrame(t, output)
	if mw != 320 || mh != 199 {
		t.Fatalf("m dimensions = %dx%d, want 320x199", mw, mh)
	}
	if stripped[1] != 25 || stripped[2] != 40 {
		t.Fatalf("stripped dimensions = %dx%d, want 40x25", stripped[2], stripped[1])
	}
}

func TestWorkerAcceptsPixelAndAspectBoundaries(t *testing.T) {
	cases := []struct {
		name            string
		width, height   int
		wantMWidth      int
		wantMHeight     int
		wantStripWidth  int
		wantStripHeight int
	}{
		{name: "pixel ceiling", width: 3200, height: 800, wantMWidth: 320, wantMHeight: 80, wantStripWidth: 40, wantStripHeight: 10},
		{name: "aspect ceiling", width: 5000, height: 250, wantMWidth: 320, wantMHeight: 16, wantStripWidth: 40, wantStripHeight: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := testJPEG(t, tc.width, tc.height, false, false)
			output, err := runWorker(t, input, tc.width, tc.height)
			if err != nil {
				t.Fatalf("worker failed: %v", err)
			}
			_, mw, mh, stripped := parseWorkerFrame(t, output)
			if mw != tc.wantMWidth || mh != tc.wantMHeight {
				t.Fatalf("m dimensions = %dx%d, want %dx%d", mw, mh, tc.wantMWidth, tc.wantMHeight)
			}
			if int(stripped[2]) != tc.wantStripWidth || int(stripped[1]) != tc.wantStripHeight {
				t.Fatalf("stripped dimensions = %dx%d, want %dx%d", stripped[2], stripped[1], tc.wantStripWidth, tc.wantStripHeight)
			}
		})
	}
}

func TestWorkerReconstructsGrayscalePreview(t *testing.T) {
	input := testJPEG(t, 1600, 1600, true, false)
	output, err := runWorker(t, input, 1600, 1600)
	if err != nil {
		t.Fatalf("worker failed: %v", err)
	}
	m, mWidth, mHeight, stripped := parseWorkerFrame(t, output)
	if len(m) == 0 || mWidth != 320 || mHeight != 320 {
		t.Fatalf("grayscale m = %dx%d/%d bytes, want 320x320", mWidth, mHeight, len(m))
	}
	preview := decodeStripped(t, stripped)
	for _, point := range []image.Point{{0, 0}, {20, 20}, {39, 39}} {
		r, g, b, _ := preview.At(point.X, point.Y).RGBA()
		if absTestInt(int(r>>8)-int(g>>8)) > 3 || absTestInt(int(g>>8)-int(b>>8)) > 3 {
			t.Fatalf("grayscale preview pixel at %v is not neutral: %d,%d,%d", point, r>>8, g>>8, b>>8)
		}
		if point == (image.Point{20, 20}) && (r>>8 < 100 || r>>8 > 155) {
			t.Fatalf("grayscale preview lost image content at %v: luma=%d", point, r>>8)
		}
	}
}

func TestWorkerUsesOneQualityRetryForOversizedM(t *testing.T) {
	input := testJPEG(t, 1600, 1600, false, true)
	output, err := runWorker(t, input, 1600, 1600)
	if err != nil {
		t.Fatalf("worker failed: %v", err)
	}
	m, mWidth, mHeight, _ := parseWorkerFrame(t, output)
	if mWidth != 320 || mHeight != 320 {
		t.Fatalf("retried m dimensions = %dx%d, want 320x320", mWidth, mHeight)
	}
	if output[5]&2 == 0 {
		t.Fatal("worker did not report the bounded quality retry")
	}
	if len(m) == 0 || len(m) > testMaxThumbnail {
		t.Fatalf("retried m size = %d, want 1..%d", len(m), testMaxThumbnail)
	}
}

func TestWorkerOmitsMAt320Pixels(t *testing.T) {
	input := testJPEG(t, 320, 240, false, false)
	output, err := runWorker(t, input, 320, 240)
	if err != nil {
		t.Fatalf("worker failed: %v", err)
	}
	m, mw, mh, stripped := parseWorkerFrame(t, output)
	if len(m) != 0 || mw != 0 || mh != 0 {
		t.Fatalf("m = %dx%d/%d bytes, want absent", mw, mh, len(m))
	}
	if stripped[0] != 1 || stripped[1] != 30 || stripped[2] != 40 {
		t.Fatalf("stripped prefix = %v, want [1 30 40]", stripped[:3])
	}
}

func TestWorkerRejectsIncompleteOrMismatchedInputWithoutFrame(t *testing.T) {
	input := testJPEG(t, 64, 64, false, false)
	corrupt := corruptJPEGEntropy(t, input)
	cases := []struct {
		name     string
		body     []byte
		declared int
		width    int
		height   int
	}{
		{name: "missing bytes", body: input[:len(input)-1], declared: len(input), width: 64, height: 64},
		{name: "extra bytes", body: append(append([]byte(nil), input...), 0), declared: len(input), width: 64, height: 64},
		{name: "dimension mismatch", body: input, declared: len(input), width: 63, height: 64},
		{name: "decode failure", body: corrupt, declared: len(corrupt), width: 64, height: 64},
		{name: "invalid jpeg", body: []byte("not a jpeg"), declared: len("not a jpeg"), width: 64, height: 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runWorkerWithSize(t, tc.body, tc.declared, tc.width, tc.height)
			if err == nil {
				t.Fatal("worker accepted invalid input")
			}
			if len(output) != 0 {
				t.Fatalf("worker emitted %d bytes on rejected input", len(output))
			}
		})
	}
}

func TestWorkerRejectsAboveEligibilityCeilingWithoutFrame(t *testing.T) {
	input := testJPEG(t, 1601, 1600, false, false)
	output, err := runWorker(t, input, 1601, 1600)
	if err == nil {
		t.Fatal("worker accepted pixels above its eligibility ceiling")
	}
	if len(output) != 0 {
		t.Fatalf("worker emitted %d bytes above the eligibility ceiling", len(output))
	}
}

func TestWorkerRejectsEncodedInputAboveCapBeforeReading(t *testing.T) {
	output, err := runWorkerWithSize(t, nil, (10<<20)+1, 1600, 1600)
	if err == nil {
		t.Fatal("worker accepted a declared input above the encoded-byte cap")
	}
	if len(output) != 0 {
		t.Fatalf("worker emitted %d bytes above the encoded-byte cap", len(output))
	}
}

func TestWorkerWaitsForCompletionEOFBeforeProducingFrame(t *testing.T) {
	input := testJPEG(t, 1600, 1200, false, true)
	cmd := exec.CommandContext(t.Context(), testWorkerBinary, "worker", strconv.Itoa(len(input)), "1600", "1200", strconv.Itoa(testPixelCap)) // #nosec G204 -- local test binary with generated input metadata.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	readDone := make(chan error, 1)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			cleanupWorker(t, stdin, cmd)
		}
	})
	go func() {
		_, err := io.Copy(&output, stdout)
		readDone <- err
	}()
	if _, err := stdin.Write(input); err != nil {
		cleanupWorker(t, stdin, cmd)
		waited = true
		t.Fatalf("write complete input: %v", err)
	}
	if runtime.GOOS == "linux" {
		assertWorkerLimits(t, cmd.Process.Pid)
	}
	select {
	case err := <-readDone:
		cleanupWorker(t, stdin, cmd)
		waited = true
		t.Fatalf("worker wrote output before input completion: %v (%d bytes)", err, output.Len())
	case <-time.After(1500 * time.Millisecond):
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read worker output: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		waited = true
		t.Fatalf("worker failed after input completion: %v: %s", err, stderr.String())
	}
	waited = true
	if output.Len() == 0 {
		t.Fatal("worker produced no frame after input completion")
	}
}

func TestWorkerDoesNotDecodeBeforeCompletionEOF(t *testing.T) {
	input := corruptJPEGEntropy(t, testJPEG(t, 64, 64, false, false))
	cmd := exec.CommandContext(t.Context(), testWorkerBinary, "worker", strconv.Itoa(len(input)), "64", "64", strconv.Itoa(testPixelCap)) // #nosec G204 -- local test binary with generated input metadata.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var stderr bytes.Buffer
	readDone := make(chan error, 1)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			cleanupWorker(t, stdin, cmd)
		}
	})
	go func() {
		_, err := io.Copy(&output, stdout)
		readDone <- err
	}()
	if n, err := stdin.Write(input); err != nil || n != len(input) {
		cleanupWorker(t, stdin, cmd)
		waited = true
		t.Fatalf("write malformed input: wrote %d of %d bytes: %v", n, len(input), err)
	}
	select {
	case err := <-readDone:
		cleanupWorker(t, stdin, cmd)
		waited = true
		t.Fatalf("worker ended before input completion, likely decoding early: %v, stderr=%s", err, stderr.String())
	case <-time.After(500 * time.Millisecond):
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close malformed input: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read worker output: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		waited = true
		t.Fatal("worker accepted entropy-corrupt input")
	}
	waited = true
	if output.Len() != 0 {
		t.Fatalf("worker emitted %d bytes for entropy-corrupt input", output.Len())
	}
}

func assertWorkerLimits(t *testing.T, rootPID int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	workerPID := 0
	for time.Now().Before(deadline) {
		workerPID = findWorkerPID(rootPID)
		if workerPID != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if workerPID == 0 {
		root := strconv.Itoa(rootPID)
		executable, executableErr := os.Readlink("/proc/" + root + "/exe")
		command, commandErr := os.ReadFile("/proc/" + root + "/cmdline")
		children, childrenErr := os.ReadFile("/proc/" + root + "/task/" + root + "/children")
		t.Fatalf("could not find photothumb process: exe=%q (%v) cmdline=%q (%v) children=%q (%v)", executable, executableErr, command, commandErr, children, childrenErr)
	}
	limits, err := os.ReadFile("/proc/" + strconv.Itoa(workerPID) + "/limits")
	if err != nil {
		t.Fatalf("read worker limits: %v", err)
	}
	for _, want := range [][]string{
		{"Max", "data", "size", strconv.Itoa(320 << 20), strconv.Itoa(320 << 20)},
		{"Max", "cpu", "time", "2", "3"},
		{"Max", "file", "size", "0", "0"},
		{"Max", "open", "files", "8", "8"},
	} {
		found := false
		for line := range strings.SplitSeq(string(limits), "\n") {
			fields := strings.Fields(line)
			if len(fields) < len(want) {
				continue
			}
			matches := true
			for index, value := range want {
				if fields[index] != value {
					matches = false
					break
				}
			}
			if matches {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("worker limits do not contain %v:\n%s", want, limits)
		}
	}
	preference, err := os.ReadFile("/proc/" + strconv.Itoa(workerPID) + "/oom_score_adj")
	if err != nil {
		t.Fatalf("read worker OOM preference: %v", err)
	}
	if strings.TrimSpace(string(preference)) != "1000" {
		t.Errorf("worker oom_score_adj = %q, want 1000", strings.TrimSpace(string(preference)))
	}
}

func findWorkerPID(rootPID int) int {
	root := strconv.Itoa(rootPID)
	if executable, err := os.Readlink("/proc/" + root + "/exe"); err == nil && strings.Contains(executable, "/photothumb") {
		return rootPID
	}
	children, err := os.ReadFile("/proc/" + root + "/task/" + root + "/children")
	if err != nil {
		return 0
	}
	childIDs := strings.Fields(string(children))
	if len(childIDs) == 0 {
		return 0
	}
	for _, child := range childIDs {
		pid, err := strconv.Atoi(child)
		if err != nil {
			continue
		}
		if workerPID := findWorkerPID(pid); workerPID != 0 {
			return workerPID
		}
	}
	return 0
}

func cleanupWorker(t *testing.T, stdin io.Closer, cmd *exec.Cmd) {
	t.Helper()
	if err := stdin.Close(); err != nil {
		t.Logf("close worker input during cleanup: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Logf("wait for worker during cleanup: %v", err)
	}
}

func runWorker(t *testing.T, input []byte, width, height int) ([]byte, error) {
	t.Helper()
	return runWorkerWithSize(t, input, len(input), width, height)
}

func runWorkerWithSize(t *testing.T, input []byte, declared, width, height int) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), testWorkerBinary, "worker", strconv.Itoa(declared), strconv.Itoa(width), strconv.Itoa(height), strconv.Itoa(testPixelCap)) // #nosec G204 -- local test binary with bounded synthetic arguments.
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.Bytes(), err
}

func testJPEG(t *testing.T, width, height int, grayscale, noise bool) []byte {
	t.Helper()
	var img image.Image
	if grayscale {
		gray := image.NewGray(image.Rect(0, 0, width, height))
		for y := range height {
			for x := range width {
				gray.SetGray(x, y, color.Gray{Y: uint8((x*3 + y*5) & 0xff)})
			}
		}
		img = gray
	} else {
		rgba := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := range height {
			for x := range width {
				c := color.RGBA{
					R: uint8((x*7 + y*3) & 0xff),
					G: uint8((x*5 + y*11) & 0xff),
					B: uint8((x*13 + y*2) & 0xff),
					A: 255,
				}
				if noise {
					switch {
					case x%5 == 0 && y%5 == 0:
						blockX, blockY := x/5, y/5
						noise := (blockX*73856093 ^ blockY*19349663 ^ blockX*blockY*83492791) & 0x7fffffff
						c.R, c.G, c.B = uint8(noise&0xff), uint8((noise>>8)&0xff), uint8((noise>>16)&0xff)
					case x%5 != 0:
						c = rgba.RGBAAt(x-1, y)
					default:
						c = rgba.RGBAAt(x, y-1)
					}
				}
				rgba.SetRGBA(x, y, c)
			}
		}
		img = rgba
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func parseWorkerFrame(t *testing.T, frame []byte) (m []byte, width, height int, stripped []byte) {
	t.Helper()
	if len(frame) < 18 || len(frame) > 4+1+1+2+2+4+testMaxThumbnail+2+testMaxStripped {
		t.Fatalf("frame size = %d, outside bounds", len(frame))
	}
	if string(frame[:4]) != "PTD1" || frame[4] != 1 || frame[5]&^byte(3) != 0 {
		t.Fatalf("frame prefix = %v, want PTD1 version 1", frame[:6])
	}
	width = int(binary.BigEndian.Uint16(frame[6:8]))
	height = int(binary.BigEndian.Uint16(frame[8:10]))
	mSize := int(binary.BigEndian.Uint32(frame[10:14]))
	if mSize > testMaxThumbnail || 14+mSize+2 > len(frame) {
		t.Fatalf("invalid m length %d in %d-byte frame", mSize, len(frame))
	}
	m = append([]byte(nil), frame[14:14+mSize]...)
	strippedSize := int(binary.BigEndian.Uint16(frame[14+mSize : 16+mSize]))
	if strippedSize < 3 || strippedSize > testMaxStripped || 16+mSize+strippedSize != len(frame) {
		t.Fatalf("invalid stripped length %d in %d-byte frame", strippedSize, len(frame))
	}
	stripped = append([]byte(nil), frame[16+mSize:]...)
	if len(m) == 0 && (width != 0 || height != 0 || frame[5]&1 != 0) {
		t.Fatalf("absent m has dimensions or present flag: %dx%d flags=%d", width, height, frame[5])
	}
	if len(m) != 0 && (width == 0 || height == 0 || frame[5]&1 == 0) {
		t.Fatalf("present m lacks dimensions or flag: %dx%d flags=%d", width, height, frame[5])
	}
	if stripped[0] != 1 || stripped[1] < 1 || stripped[1] > 40 || stripped[2] < 1 || stripped[2] > 40 {
		t.Fatalf("invalid stripped prefix %v", stripped[:3])
	}
	return m, width, height, stripped
}

func decodeStripped(t *testing.T, stripped []byte) image.Image {
	t.Helper()
	if len(stripped) < 4 || stripped[0] != 1 {
		t.Fatalf("invalid stripped payload: %v", stripped[:minTestInt(3, len(stripped))])
	}
	header := clientJPEGTemplate(t)
	header[164] = stripped[1]
	header[166] = stripped[2]
	encoded := make([]byte, 0, len(header)+len(stripped)-3+2)
	encoded = append(encoded, header...)
	encoded = append(encoded, stripped[3:]...)
	encoded = append(encoded, 0xff, 0xd9)
	img, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("reconstruct stripped JPEG: %v", err)
	}
	return img
}

func clientJPEGTemplate(t *testing.T) []byte {
	t.Helper()
	const template = "ffd8ffe000104a46494600010100000100010000ffdb004300281c1e231e19282321232d2b28303c64413c37373c7b585d4964918099968f808c8aa0b4e6c3a0aadaad8a8cc8ffcbdaeef5ffffff9bc1fffffffaffe6fdfff8ffdb0043012b2d2d3c353c76414176f8a58ca5f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8ffc00011080000000003012200021101031101ffc4001f0000010501010101010100000000000000000102030405060708090a0bffc400b5100002010303020403050504040000017d01020300041105122131410613516107227114328191a1082342b1c11552d1f02433627282090a161718191a25262728292a3435363738393a434445464748494a535455565758595a636465666768696a737475767778797a838485868788898a92939495969798999aa2a3a4a5a6a7a8a9aab2b3b4b5b6b7b8b9bac2c3c4c5c6c7c8c9cad2d3d4d5d6d7d8d9dae1e2e3e4e5e6e7e8e9eaf1f2f3f4f5f6f7f8f9faffc4001f0100030101010101010101010000000000000102030405060708090a0bffc400b51100020102040403040705040400010277000102031104052131061241510761711322328108144291a1b1c109233352f0156272d10a162434e125f11718191a262728292a35363738393a434445464748494a535455565758595a636465666768696a737475767778797a82838485868788898a92939495969798999aa2a3a4a5a6a7a8a9aab2b3b4b5b6b7b8b9bac2c3c4c5c6c7c8c9cad2d3d4d5d6d7d8d9dae2e3e4e5e6e7e8e9eaf2f3f4f5f6f7f8f9faffda000c03010002110311003f00"
	out := make([]byte, len(template)/2)
	for i := range out {
		v, err := strconv.ParseUint(template[i*2:i*2+2], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = byte(v)
	}
	if len(out) != 623 {
		t.Fatalf("client template size = %d, want 623", len(out))
	}
	return out
}

func hasBaselineSOF(encoded []byte) bool {
	for i := 2; i+4 < len(encoded); {
		if encoded[i] != 0xff {
			return false
		}
		marker := encoded[i+1]
		if marker == 0xc0 {
			return true
		}
		if marker == 0xda || marker == 0xd9 || i+4 > len(encoded) {
			return false
		}
		segmentLen := int(binary.BigEndian.Uint16(encoded[i+2 : i+4]))
		if segmentLen < 2 || i+2+segmentLen > len(encoded) {
			return false
		}
		i += 2 + segmentLen
	}
	return false
}

func corruptJPEGEntropy(t *testing.T, encoded []byte) []byte {
	t.Helper()
	marker := bytes.Index(encoded, []byte{0xff, 0xda})
	if marker < 0 || marker+4 > len(encoded) {
		t.Fatal("test JPEG has no SOS marker")
	}
	segmentLen := int(binary.BigEndian.Uint16(encoded[marker+2 : marker+4]))
	start := marker + 2 + segmentLen
	if start >= len(encoded)-2 || encoded[len(encoded)-2] != 0xff || encoded[len(encoded)-1] != 0xd9 {
		t.Fatal("test JPEG has no entropy bytes or EOI")
	}
	out := append([]byte(nil), encoded[:start]...)
	out = append(out, 0xff, 0x00, 0xff, 0xd9)
	return out
}

func absTestInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func minTestInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
