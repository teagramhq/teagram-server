package api

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- the inputFile protocol requires this non-security checksum.
	"encoding/hex"
	"errors"
	"hash"
	"io"
)

const (
	maxPhotoJPEGBytes       = int64(10 * 1024 * 1024)
	maxPhotoJPEGMarkers     = 2048
	maxPhotoJPEGScans       = 64
	maxPhotoJPEGAppMarkers  = 64
	maxPhotoJPEGDHTSegments = 32
	maxPhotoJPEGDQTSegments = 16
)

type photoDimensions struct {
	width  int
	height int
}

type jpegValidationError struct {
	verdict string
}

func (e jpegValidationError) Error() string {
	return e.verdict
}

func (e jpegValidationError) Verdict() string {
	return e.verdict
}

func invalidJPEG() error {
	return jpegValidationError{verdict: "MEDIA_INVALID"}
}

func invalidPhotoDimensions() error {
	return jpegValidationError{verdict: "PHOTO_INVALID_DIMENSIONS"}
}

type jpegComponent struct {
	id         byte
	quantTable byte
}

type photoJPEGValidator struct {
	input *jpegInput

	markerCount int
	appCount    int
	dhtCount    int
	dqtCount    int
	scanCount   int

	frameMarker byte
	width       int
	height      int
	components  [3]jpegComponent
	componentN  int
	frameSeen   bool

	quantTables          byte
	dcTables             byte
	acTables             byte
	restart              uint16
	hasProgressiveEOBRUN bool

	sequentialScans uint8
	progression     [3][64]int8
}

// validateJPEG checks framing and table/scan structure while consuming the
// supplied body once. It deliberately does not decode entropy-coded pixels.
// The encoded size comes from the upload-part summary, and the optional MD5 is
// checked against the bytes consumed from this reader.
func validateJPEG(r io.Reader, encodedSize int64, checksum string) (photoDimensions, error) {
	if r == nil || encodedSize < 1 || encodedSize > maxPhotoJPEGBytes {
		return photoDimensions{}, invalidJPEG()
	}

	var expectedMD5 []byte
	if checksum != "" {
		if len(checksum) != md5.Size*2 {
			return photoDimensions{}, invalidJPEG()
		}
		decoded, err := hex.DecodeString(checksum)
		if err != nil {
			return photoDimensions{}, invalidJPEG()
		}
		expectedMD5 = decoded
	}

	input := &jpegInput{reader: r, expected: encodedSize}
	if len(expectedMD5) != 0 {
		// #nosec G401 -- inputFile defines MD5 as a compatibility checksum, not a security primitive.
		input.digest = md5.New()
		copy(input.expectedMD5[:], expectedMD5)
	}
	validator := photoJPEGValidator{input: input}
	for component := range validator.progression {
		for coefficient := range validator.progression[component] {
			validator.progression[component][coefficient] = -1
		}
	}
	return validator.validate()
}

func (p *photoJPEGValidator) validate() (photoDimensions, error) {
	marker, err := p.readMarker()
	if err != nil {
		return photoDimensions{}, err
	}
	if marker != 0xd8 {
		return photoDimensions{}, invalidJPEG()
	}
	p.markerCount = 1
	marker, err = p.readMarker()
	if err != nil {
		return photoDimensions{}, err
	}

	for {
		if p.markerCount >= maxPhotoJPEGMarkers {
			return photoDimensions{}, invalidJPEG()
		}
		p.markerCount++

		switch {
		case marker == 0xd9:
			if err := p.validateFrameComplete(); err != nil {
				return photoDimensions{}, err
			}
			if err := p.input.finish(); err != nil {
				return photoDimensions{}, err
			}
			return photoDimensions{width: p.width, height: p.height}, nil
		case marker == 0xc0 || marker == 0xc1 || marker == 0xc2:
			if err := p.parseFrame(marker); err != nil {
				return photoDimensions{}, err
			}
		case marker == 0xc4:
			p.dhtCount++
			if p.dhtCount > maxPhotoJPEGDHTSegments {
				return photoDimensions{}, invalidJPEG()
			}
			if err := p.parseHuffmanTables(); err != nil {
				return photoDimensions{}, err
			}
		case marker == 0xdb:
			p.dqtCount++
			if p.dqtCount > maxPhotoJPEGDQTSegments {
				return photoDimensions{}, invalidJPEG()
			}
			if err := p.parseQuantizationTables(); err != nil {
				return photoDimensions{}, err
			}
		case marker == 0xdd:
			if err := p.parseRestartInterval(); err != nil {
				return photoDimensions{}, err
			}
		case marker >= 0xe0 && marker <= 0xef || marker == 0xfe:
			p.appCount++
			if p.appCount > maxPhotoJPEGAppMarkers {
				return photoDimensions{}, invalidJPEG()
			}
			if err := p.skipSegment(); err != nil {
				return photoDimensions{}, err
			}
		case marker == 0xda:
			p.scanCount++
			if p.scanCount > maxPhotoJPEGScans {
				return photoDimensions{}, invalidJPEG()
			}
			if err := p.parseScan(); err != nil {
				return photoDimensions{}, err
			}
			marker, err = p.readScanMarker()
			if err != nil {
				return photoDimensions{}, err
			}
			continue
		default:
			return photoDimensions{}, invalidJPEG()
		}
		marker, err = p.readMarker()
		if err != nil {
			return photoDimensions{}, err
		}
	}
}

func (p *photoJPEGValidator) validateFrameComplete() error {
	if !p.frameSeen || p.scanCount == 0 {
		return invalidJPEG()
	}
	if p.frameMarker == 0xc2 {
		for component := range p.componentN {
			if p.progression[component][0] < 0 {
				return invalidJPEG()
			}
		}
		return nil
	}
	want := uint8(1<<p.componentN) - 1
	if p.sequentialScans != want {
		return invalidJPEG()
	}
	return nil
}

func (p *photoJPEGValidator) parseFrame(marker byte) error {
	if p.frameSeen {
		return invalidJPEG()
	}
	segment, err := p.openSegment()
	if err != nil {
		return err
	}
	if segment.remaining < 6 {
		return invalidJPEG()
	}
	precision, err := segment.readByte()
	if err != nil {
		return err
	}
	heightHigh, err := segment.readByte()
	if err != nil {
		return err
	}
	heightLow, err := segment.readByte()
	if err != nil {
		return err
	}
	widthHigh, err := segment.readByte()
	if err != nil {
		return err
	}
	widthLow, err := segment.readByte()
	if err != nil {
		return err
	}
	componentN, err := segment.readByte()
	if err != nil {
		return err
	}
	if precision != 8 || componentN != 1 && componentN != 3 || segment.remaining != int64(componentN)*3 {
		return invalidJPEG()
	}

	var components [3]jpegComponent
	var componentIDs [256]bool
	for i := range int(componentN) {
		id, err := segment.readByte()
		if err != nil {
			return err
		}
		sampling, err := segment.readByte()
		if err != nil {
			return err
		}
		quantTable, err := segment.readByte()
		if err != nil {
			return err
		}
		horizontal := sampling >> 4
		vertical := sampling & 0x0f
		if componentIDs[id] || horizontal < 1 || horizontal > 4 || vertical < 1 || vertical > 4 || quantTable > 3 {
			return invalidJPEG()
		}
		componentIDs[id] = true
		components[i] = jpegComponent{id: id, quantTable: quantTable}
	}
	if segment.remaining != 0 {
		return invalidJPEG()
	}

	width := int(widthHigh)<<8 | int(widthLow)
	height := int(heightHigh)<<8 | int(heightLow)
	if invalidDimensions(width, height) {
		return invalidPhotoDimensions()
	}
	if marker != 0xc2 && p.hasProgressiveEOBRUN {
		return invalidJPEG()
	}
	p.frameMarker = marker
	p.width = width
	p.height = height
	p.components = components
	p.componentN = int(componentN)
	p.frameSeen = true
	return nil
}

func invalidDimensions(width, height int) bool {
	if width < 1 || height < 1 || width > 10000 || height > 10000 || width+height > 10000 {
		return true
	}
	short, long := width, height
	if short > long {
		short, long = long, short
	}
	return long > short*20 || uint64(width)*uint64(height) > 16_777_216
}

func (p *photoJPEGValidator) parseQuantizationTables() error {
	segment, err := p.openSegment()
	if err != nil {
		return err
	}
	if segment.remaining == 0 {
		return invalidJPEG()
	}
	for segment.remaining > 0 {
		info, err := segment.readByte()
		if err != nil {
			return err
		}
		precision := info >> 4
		tableID := info & 0x0f
		if precision != 0 || tableID > 3 {
			return invalidJPEG()
		}
		valueBytes := int64(64)
		if segment.remaining < valueBytes {
			return invalidJPEG()
		}
		for range valueBytes {
			value, err := segment.readByte()
			if err != nil {
				return err
			}
			if value == 0 {
				return invalidJPEG()
			}
		}
		mask := byte(1 << tableID)
		p.quantTables |= mask
	}
	return nil
}

func (p *photoJPEGValidator) parseHuffmanTables() error {
	segment, err := p.openSegment()
	if err != nil {
		return err
	}
	if segment.remaining == 0 {
		return invalidJPEG()
	}
	for segment.remaining > 0 {
		info, err := segment.readByte()
		if err != nil {
			return err
		}
		class := info >> 4
		tableID := info & 0x0f
		if class > 1 || tableID > 3 || segment.remaining < 16 {
			return invalidJPEG()
		}
		var counts [16]byte
		if err := segment.readFull(counts[:]); err != nil {
			return err
		}
		symbolCount := 0
		availableCodes := 1
		code := 0
		for depth, count := range counts {
			symbolCount += int(count)
			availableCodes = availableCodes*2 - int(count)
			if availableCodes < 0 {
				return invalidJPEG()
			}
			code <<= 1
			if count > 0 && code+int(count)-1 == (1<<(depth+1))-1 {
				return invalidJPEG()
			}
			code += int(count)
		}
		if symbolCount == 0 || symbolCount > 256 || int64(symbolCount) > segment.remaining {
			return invalidJPEG()
		}
		for range symbolCount {
			symbol, err := segment.readByte()
			if err != nil {
				return err
			}
			if class == 0 && symbol > 11 {
				return invalidJPEG()
			}
			if class == 1 {
				run, size := symbol>>4, symbol&0x0f
				if size > 10 {
					return invalidJPEG()
				}
				if size == 0 && run != 0 && run != 15 {
					if p.frameSeen && p.frameMarker != 0xc2 {
						return invalidJPEG()
					}
					p.hasProgressiveEOBRUN = true
				}
			}
		}
		if class == 0 {
			p.dcTables |= 1 << tableID
		} else {
			p.acTables |= 1 << tableID
		}
	}
	return nil
}

func (p *photoJPEGValidator) parseRestartInterval() error {
	segment, err := p.openSegment()
	if err != nil {
		return err
	}
	if segment.remaining != 2 {
		return invalidJPEG()
	}
	high, err := segment.readByte()
	if err != nil {
		return err
	}
	low, err := segment.readByte()
	if err != nil {
		return err
	}
	p.restart = uint16(high)<<8 | uint16(low)
	return nil
}

func (p *photoJPEGValidator) skipSegment() error {
	segment, err := p.openSegment()
	if err != nil {
		return err
	}
	return segment.skip()
}

func (p *photoJPEGValidator) parseScan() error {
	if !p.frameSeen {
		return invalidJPEG()
	}
	segment, err := p.openSegment()
	if err != nil {
		return err
	}
	if segment.remaining < 1 {
		return invalidJPEG()
	}
	componentN, err := segment.readByte()
	if err != nil {
		return err
	}
	if componentN == 0 || int(componentN) > p.componentN || segment.remaining != int64(componentN)*2+3 {
		return invalidJPEG()
	}
	var selected [3]int
	var selectedMask uint8
	var dcSelectors [3]byte
	var acSelectors [3]byte
	for i := range int(componentN) {
		id, err := segment.readByte()
		if err != nil {
			return err
		}
		tables, err := segment.readByte()
		if err != nil {
			return err
		}
		component := p.componentIndex(id)
		if component < 0 || selectedMask&(1<<component) != 0 {
			return invalidJPEG()
		}
		selected[i] = component
		selectedMask |= 1 << component
		dcSelectors[i] = tables >> 4
		acSelectors[i] = tables & 0x0f
	}
	ss, err := segment.readByte()
	if err != nil {
		return err
	}
	se, err := segment.readByte()
	if err != nil {
		return err
	}
	approx, err := segment.readByte()
	if err != nil {
		return err
	}
	if segment.remaining != 0 {
		return invalidJPEG()
	}
	ah, al := approx>>4, approx&0x0f
	if p.frameMarker == 0xc0 || p.frameMarker == 0xc1 {
		if ss != 0 || se != 63 || ah != 0 || al != 0 || selectedMask&p.sequentialScans != 0 {
			return invalidJPEG()
		}
		if !p.tablesAvailable(selected[:], int(componentN), dcSelectors, acSelectors, ss, ah) {
			return invalidJPEG()
		}
		p.sequentialScans |= selectedMask
		return nil
	}
	if p.frameMarker != 0xc2 || ss > se || se > 63 || ah > 13 || al > 13 || ah != 0 && ah != al+1 {
		return invalidJPEG()
	}
	if ss == 0 {
		if se != 0 {
			return invalidJPEG()
		}
		for i := range int(componentN) {
			if acSelectors[i] != 0 || ah > 0 && dcSelectors[i] != 0 {
				return invalidJPEG()
			}
		}
	} else {
		if componentN != 1 {
			return invalidJPEG()
		}
		if dcSelectors[0] != 0 || p.progression[selected[0]][0] < 0 {
			return invalidJPEG()
		}
	}
	if !p.tablesAvailable(selected[:], int(componentN), dcSelectors, acSelectors, ss, ah) {
		return invalidJPEG()
	}
	for i := range int(componentN) {
		component := selected[i]
		for coefficient := int(ss); coefficient <= int(se); coefficient++ {
			previous := p.progression[component][coefficient]
			if ah == 0 {
				if previous >= 0 {
					return invalidJPEG()
				}
			} else if previous != int8(ah) {
				return invalidJPEG()
			}
			p.progression[component][coefficient] = int8(al)
		}
	}
	return nil
}

func (p *photoJPEGValidator) tablesAvailable(components []int, count int, dcSelectors, acSelectors [3]byte, ss, ah byte) bool {
	for i := range count {
		component := p.components[components[i]]
		quantMask := byte(1 << component.quantTable)
		if p.quantTables&quantMask == 0 {
			return false
		}
		if p.frameMarker == 0xc2 {
			if ss == 0 {
				if acSelectors[i] != 0 || ah == 0 && p.dcTables&(1<<dcSelectors[i]) == 0 {
					return false
				}
			} else if dcSelectors[i] != 0 || p.acTables&(1<<acSelectors[i]) == 0 {
				return false
			}
			continue
		}
		if p.dcTables&(1<<dcSelectors[i]) == 0 || p.acTables&(1<<acSelectors[i]) == 0 {
			return false
		}
	}
	return true
}

func (p *photoJPEGValidator) componentIndex(id byte) int {
	for i := range p.componentN {
		if p.components[i].id == id {
			return i
		}
	}
	return -1
}

func (p *photoJPEGValidator) readScanMarker() (byte, error) {
	expectedRestart := byte(0)
	entropySinceRestart := false
	for {
		if p.input.start == p.input.end {
			if err := p.input.fill(); err != nil {
				return 0, structuralReadError(err)
			}
		}
		data := p.input.buffer[p.input.start:p.input.end]
		markerOffset := bytes.IndexByte(data, 0xff)
		if markerOffset < 0 {
			p.input.consumed += int64(len(data))
			p.input.start = p.input.end
			entropySinceRestart = true
			continue
		}
		if markerOffset > 0 {
			p.input.start += markerOffset
			p.input.consumed += int64(markerOffset)
			entropySinceRestart = true
		}
		p.input.start++
		p.input.consumed++
		marker, err := p.readByte()
		if err != nil {
			return 0, err
		}
		if marker == 0x00 {
			entropySinceRestart = true
			continue
		}
		for marker == 0xff {
			marker, err = p.readByte()
			if err != nil {
				return 0, err
			}
		}
		if marker == 0x00 {
			return 0, invalidJPEG()
		}
		if marker >= 0xd0 && marker <= 0xd7 {
			if p.restart == 0 || !entropySinceRestart || marker != 0xd0+expectedRestart {
				return 0, invalidJPEG()
			}
			expectedRestart = (expectedRestart + 1) & 7
			entropySinceRestart = false
			continue
		}
		if !entropySinceRestart {
			return 0, invalidJPEG()
		}
		return marker, nil
	}
}

func (p *photoJPEGValidator) readMarker() (byte, error) {
	first, err := p.readByte()
	if err != nil {
		return 0, err
	}
	if first != 0xff {
		return 0, invalidJPEG()
	}
	marker, err := p.readByte()
	if err != nil {
		return 0, err
	}
	for marker == 0xff {
		marker, err = p.readByte()
		if err != nil {
			return 0, err
		}
	}
	if marker == 0x00 {
		return 0, invalidJPEG()
	}
	return marker, nil
}

func (p *photoJPEGValidator) readByte() (byte, error) {
	b, err := p.input.readByte()
	if err != nil {
		return 0, structuralReadError(err)
	}
	return b, nil
}

func (p *photoJPEGValidator) openSegment() (jpegSegment, error) {
	high, err := p.readByte()
	if err != nil {
		return jpegSegment{}, err
	}
	low, err := p.readByte()
	if err != nil {
		return jpegSegment{}, err
	}
	length := int64(high)<<8 | int64(low)
	if length < 2 || length-2 > p.input.remaining() {
		return jpegSegment{}, invalidJPEG()
	}
	return jpegSegment{validator: p, remaining: length - 2}, nil
}

func structuralReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return invalidJPEG()
	}
	return err
}

type jpegSegment struct {
	validator *photoJPEGValidator
	remaining int64
}

func (s *jpegSegment) readByte() (byte, error) {
	if s.remaining <= 0 {
		return 0, invalidJPEG()
	}
	b, err := s.validator.readByte()
	if err != nil {
		return 0, err
	}
	s.remaining--
	return b, nil
}

func (s *jpegSegment) readFull(dst []byte) error {
	if int64(len(dst)) > s.remaining {
		return invalidJPEG()
	}
	if err := s.validator.input.readFull(dst); err != nil {
		return structuralReadError(err)
	}
	s.remaining -= int64(len(dst))
	return nil
}

func (s *jpegSegment) skip() error {
	if err := s.validator.input.skip(s.remaining); err != nil {
		return structuralReadError(err)
	}
	s.remaining = 0
	return nil
}

type jpegInput struct {
	reader      io.Reader
	expected    int64
	loaded      int64
	consumed    int64
	buffer      [4096]byte
	start       int
	end         int
	pendingErr  error
	zeroReads   int
	digest      hash.Hash
	expectedMD5 [md5.Size]byte
}

func (in *jpegInput) remaining() int64 {
	return in.expected - in.consumed
}

func (in *jpegInput) readByte() (byte, error) {
	if in.start == in.end {
		if err := in.fill(); err != nil {
			return 0, err
		}
	}
	b := in.buffer[in.start]
	in.start++
	in.consumed++
	return b, nil
}

func (in *jpegInput) readFull(dst []byte) error {
	for len(dst) > 0 {
		if in.start == in.end {
			if err := in.fill(); err != nil {
				return err
			}
		}
		n := copy(dst, in.buffer[in.start:in.end])
		in.start += n
		in.consumed += int64(n)
		dst = dst[n:]
	}
	return nil
}

func (in *jpegInput) skip(n int64) error {
	if n < 0 || n > in.remaining() {
		return invalidJPEG()
	}
	for n > 0 {
		if in.start == in.end {
			if err := in.fill(); err != nil {
				return err
			}
		}
		available := int64(in.end - in.start)
		available = min(available, n)
		in.start += int(available)
		in.consumed += available
		n -= available
	}
	return nil
}

func (in *jpegInput) fill() error {
	if in.start != in.end {
		return nil
	}
	if in.pendingErr != nil {
		err := in.pendingErr
		in.pendingErr = nil
		return err
	}
	if in.loaded >= in.expected {
		return io.EOF
	}
	in.start = 0
	in.end = 0
	readSize := len(in.buffer)
	if left := in.expected - in.loaded; int64(readSize) > left {
		readSize = int(left)
	}
	for {
		n, err := in.reader.Read(in.buffer[:readSize])
		if n < 0 || n > readSize {
			return errors.New("invalid reader result")
		}
		if n > 0 {
			in.loaded += int64(n)
			in.end = n
			in.zeroReads = 0
			if in.digest != nil {
				if _, writeErr := in.digest.Write(in.buffer[:n]); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				in.pendingErr = err
			}
			return nil
		}
		if err != nil {
			return err
		}
		in.zeroReads++
		if in.zeroReads >= 100 {
			return io.ErrNoProgress
		}
	}
}

func (in *jpegInput) finish() error {
	if in.consumed != in.expected || in.start != in.end {
		return invalidJPEG()
	}
	if err := in.confirmEOF(); err != nil {
		return err
	}
	if in.digest != nil {
		var actual [md5.Size]byte
		in.digest.Sum(actual[:0])
		if !bytes.Equal(actual[:], in.expectedMD5[:]) {
			return invalidJPEG()
		}
	}
	return nil
}

func (in *jpegInput) confirmEOF() error {
	if in.pendingErr != nil {
		err := in.pendingErr
		in.pendingErr = nil
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var probe [1]byte
	zeroReads := 0
	for {
		n, err := in.reader.Read(probe[:])
		if n < 0 || n > len(probe) {
			return errors.New("invalid reader result")
		}
		if n > 0 {
			return invalidJPEG()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		zeroReads++
		if zeroReads >= 100 {
			return io.ErrNoProgress
		}
	}
}
