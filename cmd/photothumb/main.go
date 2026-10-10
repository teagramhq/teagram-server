// Command photothumb turns a parent-validated JPEG into bounded Telegram photo
// derivatives. Invoke it as `photothumb worker <bytes> <width> <height> 2560000`
// only after the original Put and structural validation succeed. The caller
// writes exactly <bytes> original bytes to stdin, then closes it; EOF is the
// validated-input completion handshake and decoding starts only after it.
//
// Stdout is one bounded PTD1 version 1 frame: magic, version, flags, big-endian
// m width, m height and length, m bytes, then big-endian stripped length and
// bytes. Flag bit 0 marks m as present; bit 1 marks the quality retry. Stripped
// bytes include Telegram's 0x01/height/width prefix followed by JPEG scan
// entropy. Any failure writes no usable frame.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
)

const (
	maxEncodedBytes       = 10 << 20
	maxEligiblePixels     = 2_560_000
	maxThumbnailBytes     = 65_536
	maxStrippedBytes      = 2_048
	maxFrameBytes         = 4 + 1 + 1 + 2 + 2 + 4 + maxThumbnailBytes + 2 + maxStrippedBytes
	workerGoMemoryLimit   = 224 << 20
	thumbnailLongSide     = 320
	strippedLongSide      = 40
	thumbnailQuality      = 85
	thumbnailRetryQuality = 60
	strippedQuality       = 20
)

const telegramJPEGTemplateHex = "ffd8ffe000104a46494600010100000100010000ffdb004300281c1e231e19282321232d2b28303c64413c37373c7b585d4964918099968f808c8aa0b4e6c3a0aadaad8a8cc8ffcbdaeef5ffffff9bc1fffffffaffe6fdfff8ffdb0043012b2d2d3c353c76414176f8a58ca5f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8ffc00011080000000003012200021101031101ffc4001f0000010501010101010100000000000000000102030405060708090a0bffc400b5100002010303020403050504040000017d01020300041105122131410613516107227114328191a1082342b1c11552d1f02433627282090a161718191a25262728292a3435363738393a434445464748494a535455565758595a636465666768696a737475767778797a838485868788898a92939495969798999aa2a3a4a5a6a7a8a9aab2b3b4b5b6b7b8b9bac2c3c4c5c6c7c8c9cad2d3d4d5d6d7d8d9dae1e2e3e4e5e6e7e8e9eaf1f2f3f4f5f6f7f8f9faffc4001f0100030101010101010101010000000000000102030405060708090a0bffc400b51100020102040403040705040400010277000102031104052131061241510761711322328108144291a1b1c109233352f0156272d10a162434e125f11718191a262728292a35363738393a434445464748494a535455565758595a636465666768696a737475767778797a82838485868788898a92939495969798999aa2a3a4a5a6a7a8a9aab2b3b4b5b6b7b8b9bac2c3c4c5c6c7c8c9cad2d3d4d5d6d7d8d9dae2e3e4e5e6e7e8e9eaf2f3f4f5f6f7f8f9faffda000c03010002110311003f00"

func main() {
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(workerGoMemoryLimit)
	if err := setWorkerLimits(); err != nil {
		fail(err)
	}
	if len(os.Args) < 2 || os.Args[1] != "worker" {
		fail(errors.New("usage: photothumb worker <input-bytes> <width> <height> <pixel-cap>"))
	}
	if err := workerMain(os.Args[2:], os.Stdin, os.Stdout); err != nil {
		fail(err)
	}
}

func fail(err error) {
	if _, writeErr := fmt.Fprintln(os.Stderr, "photothumb:", err); writeErr != nil {
		os.Exit(2)
	}
	os.Exit(2)
}

func workerMain(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 4 {
		return errors.New("worker expects input-bytes, width, height and pixel-cap")
	}
	inputBytes, err := strconv.Atoi(args[0])
	if err != nil || inputBytes < 1 || inputBytes > maxEncodedBytes {
		return errors.New("input byte count is outside the accepted range")
	}
	width, err := strconv.Atoi(args[1])
	if err != nil {
		return errors.New("invalid input width")
	}
	height, err := strconv.Atoi(args[2])
	if err != nil {
		return errors.New("invalid input height")
	}
	pixelCap, err := strconv.Atoi(args[3])
	if err != nil || pixelCap != maxEligiblePixels {
		return errors.New("pixel cap does not match the qualified ceiling")
	}
	if !validDimensions(width, height) {
		return errors.New("input dimensions are outside the eligible range")
	}

	original, err := readExactInput(input, inputBytes)
	if err != nil {
		return err
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(original))
	if err != nil {
		return fmt.Errorf("read JPEG dimensions: %w", err)
	}
	if config.Width != width || config.Height != height {
		return fmt.Errorf("JPEG dimensions %dx%d differ from validated dimensions %dx%d", config.Width, config.Height, width, height)
	}
	if !validDimensions(config.Width, config.Height) {
		return errors.New("JPEG dimensions exceed the eligible pixel ceiling")
	}

	decoded, err := jpeg.Decode(bytes.NewReader(original))
	if err != nil {
		return fmt.Errorf("decode JPEG: %w", err)
	}
	if decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height {
		return errors.New("decoded JPEG bounds differ from validated dimensions")
	}

	frame, err := makeDerivatives(decoded, width, height)
	if err != nil {
		return err
	}
	if len(frame) > maxFrameBytes {
		return errors.New("derivative frame exceeds the output ceiling")
	}
	if err := writeAll(output, frame); err != nil {
		return fmt.Errorf("write derivative frame: %w", err)
	}
	return nil
}

func validDimensions(width, height int) bool {
	if width < 1 || height < 1 || width > 10_000 || height > 10_000 || width+height > 10_000 {
		return false
	}
	short, long := width, height
	if short > long {
		short, long = long, short
	}
	if long > short*20 {
		return false
	}
	return int64(width)*int64(height) <= maxEligiblePixels
}

func readExactInput(input io.Reader, size int) ([]byte, error) {
	if input == nil || size < 1 || size > maxEncodedBytes {
		return nil, errors.New("invalid input framing")
	}
	original := make([]byte, size)
	if _, err := io.ReadFull(input, original); err != nil {
		return nil, fmt.Errorf("read original bytes: %w", err)
	}
	var extra [1]byte
	n, err := io.ReadFull(input, extra[:])
	if n != 0 {
		return nil, errors.New("input contains bytes beyond its declared length")
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("confirm input completion: %w", err)
	}
	return original, nil
}

func makeDerivatives(decoded image.Image, width, height int) ([]byte, error) {
	var mBytes []byte
	mWidth, mHeight := 0, 0
	retried := false
	if max(width, height) > thumbnailLongSide {
		mWidth, mHeight = scaledDimensions(width, height, thumbnailLongSide)
		thumbnail := resizeArea(decoded, mWidth, mHeight)
		encoded, err := encodeJPEG(thumbnail, thumbnailQuality)
		if err != nil {
			return nil, fmt.Errorf("encode m at quality %d: %w", thumbnailQuality, err)
		}
		if len(encoded) > maxThumbnailBytes {
			retried = true
			encoded, err = encodeJPEG(thumbnail, thumbnailRetryQuality)
			if err != nil {
				return nil, fmt.Errorf("retry m at quality %d: %w", thumbnailRetryQuality, err)
			}
		}
		if len(encoded) > maxThumbnailBytes {
			return nil, errors.New("m remains oversized after the bounded quality retry")
		}
		if err := validateThumbnail(encoded, mWidth, mHeight); err != nil {
			return nil, fmt.Errorf("validate m: %w", err)
		}
		mBytes = encoded
	}

	stripWidth, stripHeight := scaledDimensions(width, height, strippedLongSide)
	strippedImage := resizeArea(decoded, stripWidth, stripHeight)
	stripJPEG, err := encodeJPEG(toYCbCr420(strippedImage), strippedQuality)
	if err != nil {
		return nil, fmt.Errorf("encode stripped preview: %w", err)
	}
	entropy, err := clientCompatibleEntropy(stripJPEG, stripWidth, stripHeight)
	if err != nil {
		return nil, fmt.Errorf("check stripped JPEG compatibility: %w", err)
	}
	stripped := make([]byte, 3, 3+len(entropy))
	stripped[0], stripped[1], stripped[2] = 1, byte(stripHeight), byte(stripWidth) // #nosec G115 -- scaledDimensions bounds each side at 40.
	stripped = append(stripped, entropy...)
	if len(stripped) > maxStrippedBytes {
		return nil, errors.New("stripped preview exceeds its byte ceiling")
	}
	if err := validateReconstructedStrip(stripped); err != nil {
		return nil, fmt.Errorf("validate reconstructed stripped preview: %w", err)
	}
	return makeFrame(mBytes, mWidth, mHeight, retried, stripped), nil
}

func scaledDimensions(width, height, targetLongSide int) (int, int) {
	if width >= height {
		return targetLongSide, max(1, int(math.Round(float64(height)*float64(targetLongSide)/float64(width))))
	}
	return max(1, int(math.Round(float64(width)*float64(targetLongSide)/float64(height)))), targetLongSide
}

func resizeArea(source image.Image, width, height int) *image.RGBA {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	destination := image.NewRGBA(image.Rect(0, 0, width, height))
	cellArea := int64(sourceWidth) * int64(sourceHeight) // #nosec G115 -- dimensions came from an eligible JPEG capped at 2,560,000 pixels.
	for y := range height {
		y0, y1 := y*sourceHeight, (y+1)*sourceHeight
		firstY, lastY := y0/height, (y1-1)/height
		for x := range width {
			x0, x1 := x*sourceWidth, (x+1)*sourceWidth
			firstX, lastX := x0/width, (x1-1)/width
			var red, green, blue int64
			for sy := firstY; sy <= lastY; sy++ {
				yWeight := min(y1, (sy+1)*height) - max(y0, sy*height)
				for sx := firstX; sx <= lastX; sx++ {
					xWeight := min(x1, (sx+1)*width) - max(x0, sx*width)
					weight := int64(xWeight * yWeight) // #nosec G115 -- overlap weights are positive and bounded by source dimensions.
					r, g, b, _ := source.At(bounds.Min.X+sx, bounds.Min.Y+sy).RGBA()
					red += int64(r) * weight
					green += int64(g) * weight
					blue += int64(b) * weight
				}
			}
			destination.SetRGBA(x, y, color.RGBA{
				R: averageToByte(red, cellArea),
				G: averageToByte(green, cellArea),
				B: averageToByte(blue, cellArea),
				A: 255,
			})
		}
	}
	return destination
}

func averageToByte(total, area int64) uint8 {
	average16 := (total + area/2) / area
	return uint8((average16 + 128) / 257) // #nosec G115 -- an averaged 16-bit color channel converts to at most 255.
}

func toYCbCr420(source image.Image) *image.YCbCr {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	destination := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio420)
	cbSums := make([]uint64, len(destination.Cb))
	crSums := make([]uint64, len(destination.Cr))
	counts := make([]uint8, len(destination.Cb))
	for y := range height {
		for x := range width {
			r, g, b, _ := source.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			yValue, cbValue, crValue := color.RGBToYCbCr(uint8(r>>8), uint8(g>>8), uint8(b>>8)) // #nosec G115 -- Color.RGBA channels are 16-bit values.
			destination.Y[destination.YOffset(x, y)] = yValue
			chromaOffset := destination.COffset(x, y)
			cbSums[chromaOffset] += uint64(cbValue)
			crSums[chromaOffset] += uint64(crValue)
			counts[chromaOffset]++
		}
	}
	for i, count := range counts {
		destination.Cb[i] = uint8((cbSums[i] + uint64(count)/2) / uint64(count)) // #nosec G115 -- chroma averages stay in the byte range.
		destination.Cr[i] = uint8((crSums[i] + uint64(count)/2) / uint64(count)) // #nosec G115 -- chroma averages stay in the byte range.
	}
	return destination
}

func encodeJPEG(source image.Image, quality int) ([]byte, error) {
	var output bytes.Buffer
	if err := jpeg.Encode(&output, source, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func validateThumbnail(encoded []byte, width, height int) error {
	header, err := parseJPEGHeader(encoded)
	if err != nil {
		return err
	}
	if header.frameMarker != 0xc0 || len(header.frame) < 5 {
		return errors.New("m is not baseline JPEG")
	}
	if int(binary.BigEndian.Uint16(header.frame[1:3])) != height || int(binary.BigEndian.Uint16(header.frame[3:5])) != width {
		return errors.New("m dimensions do not match its frame")
	}
	if len(encoded) < 2 || encoded[len(encoded)-2] != 0xff || encoded[len(encoded)-1] != 0xd9 {
		return errors.New("m has no terminal EOI marker")
	}
	return nil
}

type jpegHeader struct {
	quantization map[int][]byte
	huffman      map[int][]byte
	frameMarker  byte
	frame        []byte
	scan         []byte
	scanStart    int
}

func parseJPEGHeader(encoded []byte) (jpegHeader, error) {
	header := jpegHeader{
		quantization: make(map[int][]byte),
		huffman:      make(map[int][]byte),
	}
	if len(encoded) < 4 || encoded[0] != 0xff || encoded[1] != 0xd8 {
		return header, errors.New("missing JPEG SOI")
	}
	for position := 2; position < len(encoded); {
		if encoded[position] != 0xff {
			return header, errors.New("invalid marker prefix")
		}
		for position < len(encoded) && encoded[position] == 0xff {
			position++
		}
		if position >= len(encoded) {
			return header, errors.New("truncated marker")
		}
		marker := encoded[position]
		position++
		if marker == 0xd8 || marker == 0xd9 || marker >= 0xd0 && marker <= 0xd7 || marker == 0x01 {
			return header, errors.New("unexpected standalone marker before SOS")
		}
		if position+2 > len(encoded) {
			return header, errors.New("truncated segment length")
		}
		segmentLength := int(binary.BigEndian.Uint16(encoded[position : position+2]))
		if segmentLength < 2 || position+segmentLength > len(encoded) {
			return header, errors.New("invalid segment length")
		}
		payload := encoded[position+2 : position+segmentLength]
		segmentEnd := position + segmentLength
		switch marker {
		case 0xdb:
			if err := parseQuantizationTables(payload, header.quantization); err != nil {
				return header, err
			}
		case 0xc4:
			if err := parseHuffmanTables(payload, header.huffman); err != nil {
				return header, err
			}
		case 0xc0, 0xc1, 0xc2:
			if header.frame != nil {
				return header, errors.New("multiple frame headers")
			}
			header.frameMarker = marker
			header.frame = append([]byte(nil), payload...)
		case 0xda:
			header.scan = append([]byte(nil), payload...)
			header.scanStart = segmentEnd
			return header, nil
		}
		position = segmentEnd
	}
	return header, errors.New("missing JPEG SOS")
}

func parseQuantizationTables(payload []byte, tables map[int][]byte) error {
	for position := 0; position < len(payload); {
		start := position
		info := payload[position]
		position++
		precision, id := info>>4, info&0x0f
		if precision > 1 || id > 3 {
			return errors.New("invalid quantization table selector")
		}
		tableBytes := 64 * (int(precision) + 1)
		if position+tableBytes > len(payload) {
			return errors.New("truncated quantization table")
		}
		position += tableBytes
		key := int(info)
		if _, exists := tables[key]; exists {
			return errors.New("duplicate quantization table")
		}
		tables[key] = append([]byte(nil), payload[start:position]...)
	}
	return nil
}

func parseHuffmanTables(payload []byte, tables map[int][]byte) error {
	for position := 0; position < len(payload); {
		start := position
		if position+17 > len(payload) {
			return errors.New("truncated Huffman table")
		}
		info := payload[position]
		class, id := info>>4, info&0x0f
		if class > 1 || id > 3 {
			return errors.New("invalid Huffman table selector")
		}
		position++
		symbols := 0
		for _, count := range payload[position : position+16] {
			symbols += int(count)
		}
		position += 16
		if position+symbols > len(payload) {
			return errors.New("truncated Huffman symbols")
		}
		position += symbols
		key := int(info)
		if _, exists := tables[key]; exists {
			return errors.New("duplicate Huffman table")
		}
		tables[key] = append([]byte(nil), payload[start:position]...)
	}
	return nil
}

func clientCompatibleEntropy(encoded []byte, width, height int) ([]byte, error) {
	template, err := hex.DecodeString(telegramJPEGTemplateHex)
	if err != nil {
		return nil, fmt.Errorf("decode pinned client template: %w", err)
	}
	if len(template) != 623 {
		return nil, fmt.Errorf("pinned client template is %d bytes, want 623", len(template))
	}
	actualHeader, err := parseJPEGHeader(encoded)
	if err != nil {
		return nil, fmt.Errorf("parse encoded JPEG header: %w", err)
	}
	templateHeader, err := parseJPEGHeader(template)
	if err != nil {
		return nil, fmt.Errorf("parse pinned client header: %w", err)
	}
	if !sameTables(actualHeader.quantization, templateHeader.quantization) {
		return nil, errors.New("quantization tables differ from the client template")
	}
	if !sameTables(actualHeader.huffman, templateHeader.huffman) {
		return nil, errors.New("huffman tables differ from the client template")
	}
	if actualHeader.frameMarker != 0xc0 || templateHeader.frameMarker != 0xc0 {
		return nil, errors.New("frame marker differs from the baseline client template")
	}
	if len(actualHeader.frame) < 5 || len(templateHeader.frame) < 5 {
		return nil, errors.New("short JPEG frame header")
	}
	if int(binary.BigEndian.Uint16(actualHeader.frame[1:3])) != height || int(binary.BigEndian.Uint16(actualHeader.frame[3:5])) != width {
		return nil, errors.New("encoded dimensions differ from the stripped dimensions")
	}
	if binary.BigEndian.Uint16(templateHeader.frame[1:3]) != 0 || binary.BigEndian.Uint16(templateHeader.frame[3:5]) != 0 {
		return nil, errors.New("pinned client template must have zero dimensions")
	}
	actualFrame := append([]byte(nil), actualHeader.frame...)
	clear(actualFrame[1:5])
	if !bytes.Equal(actualFrame, templateHeader.frame) {
		return nil, errors.New("component sampling differs from the client template")
	}
	if !bytes.Equal(actualHeader.scan, templateHeader.scan) {
		return nil, errors.New("scan layout differs from the client template")
	}
	if actualHeader.scanStart > len(encoded)-2 || encoded[len(encoded)-2] != 0xff || encoded[len(encoded)-1] != 0xd9 {
		return nil, errors.New("encoded JPEG has trailing data or no terminal EOI")
	}
	return append([]byte(nil), encoded[actualHeader.scanStart:len(encoded)-2]...), nil
}

func sameTables(a, b map[int][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for key, table := range a {
		if !bytes.Equal(table, b[key]) {
			return false
		}
	}
	return true
}

func validateReconstructedStrip(stripped []byte) error {
	if len(stripped) < 4 || len(stripped) > maxStrippedBytes || stripped[0] != 1 || stripped[1] == 0 || stripped[1] > strippedLongSide || stripped[2] == 0 || stripped[2] > strippedLongSide {
		return errors.New("invalid stripped prefix or size")
	}
	template, err := hex.DecodeString(telegramJPEGTemplateHex)
	if err != nil {
		return fmt.Errorf("decode pinned client template: %w", err)
	}
	header := append([]byte(nil), template...)
	header[164], header[166] = stripped[1], stripped[2]
	encoded := make([]byte, 0, len(header)+len(stripped)-3+2)
	encoded = append(encoded, header...)
	encoded = append(encoded, stripped[3:]...)
	encoded = append(encoded, 0xff, 0xd9)
	config, err := jpeg.DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("decode reconstructed dimensions: %w", err)
	}
	if config.Width != int(stripped[2]) || config.Height != int(stripped[1]) {
		return errors.New("reconstructed dimensions differ from the stripped prefix")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("decode reconstructed pixels: %w", err)
	}
	if decoded.Bounds().Dx() != int(stripped[2]) || decoded.Bounds().Dy() != int(stripped[1]) {
		return errors.New("reconstructed bounds differ from the stripped prefix")
	}
	return nil
}

func makeFrame(m []byte, width, height int, retried bool, stripped []byte) []byte {
	frame := make([]byte, 4+1+1+2+2+4+len(m)+2+len(stripped))
	copy(frame[:4], "PTD1")
	frame[4] = 1
	if len(m) != 0 {
		frame[5] |= 1
	}
	if retried {
		frame[5] |= 2
	}
	binary.BigEndian.PutUint16(frame[6:8], uint16(width))    // #nosec G115 -- m dimensions are scaled to at most 320.
	binary.BigEndian.PutUint16(frame[8:10], uint16(height))  // #nosec G115 -- m dimensions are scaled to at most 320.
	binary.BigEndian.PutUint32(frame[10:14], uint32(len(m))) // #nosec G115 -- m is capped at 65,536 bytes.
	copy(frame[14:], m)
	position := 14 + len(m)
	binary.BigEndian.PutUint16(frame[position:position+2], uint16(len(stripped))) // #nosec G115 -- stripped output is capped at 2,048 bytes.
	copy(frame[position+2:], stripped)
	return frame
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
