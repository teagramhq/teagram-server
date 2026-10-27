package stickervalidation

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"
)

const (
	ebmlHeaderID     = 0x1a45dfa3
	ebmlSegmentID    = 0x18538067
	ebmlDocTypeID    = 0x4282
	ebmlInfoID       = 0x1549a966
	ebmlTracksID     = 0x1654ae6b
	ebmlTrackEntryID = 0xae
	ebmlClusterID    = 0x1f43b675
)

var errInvalidWEBM = errors.New("invalid WEBM")

func validateWEBM(data []byte) (int, int, float64, error) {
	header, err := readEBMLElement(data)
	if err != nil || header.id != ebmlHeaderID || header.unknown || !validWebMDocType(header.payload) {
		return 0, 0, 0, errInvalidWEBM
	}
	segment, err := readEBMLElement(header.rest)
	if err != nil || segment.id != ebmlSegmentID {
		return 0, 0, 0, errInvalidWEBM
	}
	if !segment.unknown && len(segment.rest) != 0 {
		return 0, 0, 0, errInvalidWEBM
	}
	return parseWebMSegment(segment.payload)
}

func validWebMDocType(data []byte) bool {
	seen := false
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return false
		}
		if element.id == ebmlDocTypeID {
			if seen || string(element.payload) != "webm" {
				return false
			}
			seen = true
		}
		data = element.rest
	}
	return seen
}

func parseWebMSegment(data []byte) (int, int, float64, error) {
	var (
		infoSeen            bool
		tracksSeen          bool
		clusterSeen         bool
		timeScale           uint64 = 1_000_000
		containerDuration   float64
		videoTrackNumber    uint64
		defaultDurationNS   uint64
		trackTimestampScale float64 = 1
		blockTrackNumber    uint64
		blockTrackSeen      bool
		mixedBlockTracks    bool
		clusterPayloads     [][]byte
		blocks              []webMPresentationBlock
		width               int
		height              int
	)
	for len(data) > 0 {
		element, err := readWebMSegmentElement(data)
		if err != nil {
			return 0, 0, 0, errInvalidWEBM
		}
		if element.unknown && element.id != ebmlClusterID {
			return 0, 0, 0, errInvalidWEBM
		}
		switch element.id {
		case ebmlInfoID:
			if infoSeen {
				return 0, 0, 0, errInvalidWEBM
			}
			infoSeen = true
			timeScale, containerDuration, err = parseWebMInfo(element.payload)
			if err != nil {
				return 0, 0, 0, errInvalidWEBM
			}
		case ebmlTracksID:
			if tracksSeen {
				return 0, 0, 0, errInvalidWEBM
			}
			tracksSeen = true
			videoTrackNumber, width, height, defaultDurationNS, trackTimestampScale, err = parseWebMTracks(element.payload)
			if err != nil {
				return 0, 0, 0, errInvalidWEBM
			}
		case ebmlClusterID:
			clusterSeen = true
			clusterPayloads = append(clusterPayloads, element.payload)
		}
		data = element.rest
	}
	if !infoSeen || !tracksSeen || !clusterSeen || timeScale == 0 || !isFinite(trackTimestampScale) || trackTimestampScale <= 0 || !isFinite(containerDuration) || containerDuration <= 0 {
		return 0, 0, 0, errInvalidWEBM
	}
	for _, clusterPayload := range clusterPayloads {
		timeline, err := parseWebMCluster(clusterPayload, timeScale, trackTimestampScale, defaultDurationNS)
		if err != nil {
			return 0, 0, 0, errInvalidWEBM
		}
		if !blockTrackSeen {
			blockTrackNumber = timeline.trackNumber
			blockTrackSeen = true
		} else if timeline.trackNumber != blockTrackNumber {
			mixedBlockTracks = true
		}
		mixedBlockTracks = mixedBlockTracks || timeline.mixedTrackNumbers
		blocks = append(blocks, timeline.blocks...)
	}
	if !blockTrackSeen || mixedBlockTracks || blockTrackNumber != videoTrackNumber {
		return 0, 0, 0, errInvalidWEBM
	}
	containerDurationNS := containerDuration * float64(timeScale)
	maxBlockEndNS, err := webMFrameTimelineEndNS(blocks, containerDurationNS)
	if err != nil {
		return 0, 0, 0, errInvalidWEBM
	}
	seconds := maxBlockEndNS / 1_000_000_000
	if !isFinite(seconds) || seconds <= 0 {
		return 0, 0, 0, errInvalidWEBM
	}
	return width, height, seconds, nil
}

func readWebMSegmentElement(data []byte) (ebmlElement, error) {
	element, err := readEBMLElement(data)
	if err != nil || !element.unknown {
		return element, err
	}
	if element.id != ebmlClusterID {
		return ebmlElement{}, errInvalidWEBM
	}

	for rest := element.payload; len(rest) > 0; {
		childID, _, _, err := readEBMLVINT(rest, true)
		if err != nil {
			return ebmlElement{}, errInvalidWEBM
		}
		if childID == ebmlClusterID {
			consumed := len(element.payload) - len(rest)
			element.payload = element.payload[:consumed]
			element.rest = rest
			return element, nil
		}
		child, err := readEBMLElement(rest)
		if err != nil || child.unknown {
			return ebmlElement{}, errInvalidWEBM
		}
		rest = child.rest
	}
	return element, nil
}

func parseWebMInfo(data []byte) (uint64, float64, error) {
	const (
		timecodeScaleID = 0x2ad7b1
		durationID      = 0x4489
	)
	timeScale := uint64(1_000_000)
	var duration float64
	scaleSeen := false
	durationSeen := false
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return 0, 0, errInvalidWEBM
		}
		switch element.id {
		case timecodeScaleID:
			if scaleSeen {
				return 0, 0, errInvalidWEBM
			}
			scaleSeen = true
			timeScale, err = ebmlUnsigned(element.payload)
			if err != nil || timeScale == 0 {
				return 0, 0, errInvalidWEBM
			}
		case durationID:
			if durationSeen {
				return 0, 0, errInvalidWEBM
			}
			durationSeen = true
			duration, err = ebmlFloat(element.payload)
			if err != nil || !isFinite(duration) {
				return 0, 0, errInvalidWEBM
			}
		}
		data = element.rest
	}
	if !durationSeen {
		return 0, 0, errInvalidWEBM
	}
	return timeScale, duration, nil
}

func parseWebMTracks(data []byte) (uint64, int, int, uint64, float64, error) {
	trackCount := 0
	var trackNumber uint64
	var defaultDurationNS uint64
	trackTimestampScale := float64(1)
	var width, height int
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return 0, 0, 0, 0, 0, errInvalidWEBM
		}
		if element.id == ebmlTrackEntryID {
			trackCount++
			if trackCount > 1 {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			trackNumber, width, height, defaultDurationNS, trackTimestampScale, err = parseWebMTrack(element.payload)
			if err != nil {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
		}
		data = element.rest
	}
	if trackCount != 1 {
		return 0, 0, 0, 0, 0, errInvalidWEBM
	}
	return trackNumber, width, height, defaultDurationNS, trackTimestampScale, nil
}

func parseWebMTrack(data []byte) (uint64, int, int, uint64, float64, error) {
	const (
		trackNumberID         = 0xd7
		trackTypeID           = 0x83
		codecID               = 0x86
		videoID               = 0xe0
		audioID               = 0xe1
		defaultDurationID     = 0x23e383
		trackTimestampScaleID = 0x23314f
	)
	var (
		trackNumberSeen     bool
		typeSeen            bool
		codecSeen           bool
		videoSeen           bool
		defaultDurationSeen bool
		trackTimestampSeen  bool
		trackNumber         uint64
		trackType           uint64
		defaultDurationNS   uint64
		trackTimestampScale float64 = 1
		codec               string
		width               int
		height              int
	)
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return 0, 0, 0, 0, 0, errInvalidWEBM
		}
		switch element.id {
		case trackNumberID:
			if trackNumberSeen {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			trackNumberSeen = true
			trackNumber, err = ebmlUnsigned(element.payload)
			if err != nil || trackNumber == 0 {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
		case trackTypeID:
			if typeSeen {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			typeSeen = true
			trackType, err = ebmlUnsigned(element.payload)
			if err != nil {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
		case codecID:
			if codecSeen {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			codecSeen = true
			codec = string(element.payload)
		case videoID:
			if videoSeen {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			videoSeen = true
			width, height, err = parseWebMVideo(element.payload)
			if err != nil {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
		case audioID:
			return 0, 0, 0, 0, 0, errInvalidWEBM
		case defaultDurationID:
			if defaultDurationSeen {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			defaultDurationSeen = true
			defaultDurationNS, err = ebmlUnsigned(element.payload)
			if err != nil || defaultDurationNS == 0 {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
		case trackTimestampScaleID:
			if trackTimestampSeen {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
			trackTimestampSeen = true
			trackTimestampScale, err = ebmlFloat(element.payload)
			if err != nil || !isFinite(trackTimestampScale) || trackTimestampScale <= 0 {
				return 0, 0, 0, 0, 0, errInvalidWEBM
			}
		}
		data = element.rest
	}
	if !trackNumberSeen || !typeSeen || trackType != 1 || !codecSeen || codec != "V_VP9" || !videoSeen {
		return 0, 0, 0, 0, 0, errInvalidWEBM
	}
	return trackNumber, width, height, defaultDurationNS, trackTimestampScale, nil
}

func parseWebMVideo(data []byte) (int, int, error) {
	const (
		pixelWidthID  = 0xb0
		pixelHeightID = 0xba
	)
	var (
		widthSeen  bool
		heightSeen bool
		width      uint64
		height     uint64
	)
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return 0, 0, errInvalidWEBM
		}
		switch element.id {
		case pixelWidthID:
			if widthSeen {
				return 0, 0, errInvalidWEBM
			}
			widthSeen = true
			width, err = ebmlUnsigned(element.payload)
			if err != nil {
				return 0, 0, errInvalidWEBM
			}
		case pixelHeightID:
			if heightSeen {
				return 0, 0, errInvalidWEBM
			}
			heightSeen = true
			height, err = ebmlUnsigned(element.payload)
			if err != nil {
				return 0, 0, errInvalidWEBM
			}
		}
		data = element.rest
	}
	if !widthSeen || !heightSeen || width == 0 || height == 0 || width > 1<<31-1 || height > 1<<31-1 {
		return 0, 0, errInvalidWEBM
	}
	return int(width), int(height), nil
}

type webMClusterTimeline struct {
	trackNumber       uint64
	mixedTrackNumbers bool
	blocks            []webMPresentationBlock
}

type webMBlockTimestamp struct {
	trackNumber        uint64
	relativeTimecode   uint16
	frameCount         uint64
	blockDurationTicks uint64
	hasBlockDuration   bool
}

type webMPresentationBlock struct {
	startNS     float64
	durationNS  float64
	hasDuration bool
}

func parseWebMCluster(data []byte, timeScale uint64, trackTimestampScale float64, defaultDurationNS uint64) (webMClusterTimeline, error) {
	const (
		timecodeID    = 0xe7
		simpleBlockID = 0xa3
		blockGroupID  = 0xa0
	)
	if timeScale == 0 || !isFinite(trackTimestampScale) || trackTimestampScale <= 0 {
		return webMClusterTimeline{}, errInvalidWEBM
	}
	var clusterTimecode uint64
	timecodeSeen := false
	for rest := data; len(rest) > 0; {
		element, err := readEBMLElement(rest)
		if err != nil || element.unknown {
			return webMClusterTimeline{}, errInvalidWEBM
		}
		if element.id == timecodeID {
			if timecodeSeen {
				return webMClusterTimeline{}, errInvalidWEBM
			}
			timecodeSeen = true
			clusterTimecode, err = ebmlUnsigned(element.payload)
			if err != nil {
				return webMClusterTimeline{}, errInvalidWEBM
			}
		}
		rest = element.rest
	}
	if !timecodeSeen {
		return webMClusterTimeline{}, errInvalidWEBM
	}

	var timeline webMClusterTimeline
	blockSeen := false
	for rest := data; len(rest) > 0; {
		element, err := readEBMLElement(rest)
		if err != nil || element.unknown {
			return webMClusterTimeline{}, errInvalidWEBM
		}
		var block webMBlockTimestamp
		switch element.id {
		case simpleBlockID:
			block, err = parseWebMBlock(element.payload)
			if err != nil {
				return webMClusterTimeline{}, errInvalidWEBM
			}
		case blockGroupID:
			block, err = parseWebMBlockGroup(element.payload)
			if err != nil {
				return webMClusterTimeline{}, errInvalidWEBM
			}
		default:
			rest = element.rest
			continue
		}
		addWebMClusterBlock(block, &blockSeen, &timeline.trackNumber, &timeline.mixedTrackNumbers)
		presentationBlock, err := webMBlockPresentation(clusterTimecode, block, timeScale, trackTimestampScale, defaultDurationNS)
		if err != nil {
			return webMClusterTimeline{}, errInvalidWEBM
		}
		timeline.blocks = append(timeline.blocks, presentationBlock)
		rest = element.rest
	}
	if !blockSeen {
		return webMClusterTimeline{}, errInvalidWEBM
	}
	return timeline, nil
}

func addWebMClusterBlock(block webMBlockTimestamp, blockSeen *bool, trackNumber *uint64, mixedTrackNumbers *bool) {
	if !*blockSeen {
		*blockSeen = true
		*trackNumber = block.trackNumber
		return
	}
	if block.trackNumber != *trackNumber {
		*mixedTrackNumbers = true
	}
}

func parseWebMBlock(data []byte) (webMBlockTimestamp, error) {
	trackNumber, trackLength, unknown, err := readEBMLVINT(data, false)
	if err != nil || unknown || trackNumber == 0 || len(data) <= trackLength+3 {
		return webMBlockTimestamp{}, errInvalidWEBM
	}
	frameCount, err := webMBlockFrameCount(data[trackLength+3:], data[trackLength+2])
	if err != nil {
		return webMBlockTimestamp{}, errInvalidWEBM
	}
	return webMBlockTimestamp{
		trackNumber:      trackNumber,
		relativeTimecode: binary.BigEndian.Uint16(data[trackLength : trackLength+2]),
		frameCount:       frameCount,
	}, nil
}

func webMBlockFrameCount(data []byte, flags byte) (uint64, error) {
	if len(data) == 0 {
		return 0, errInvalidWEBM
	}
	lacing := (flags >> 1) & 0x03
	if lacing == 0 {
		return 1, nil
	}
	if len(data) < 2 {
		return 0, errInvalidWEBM
	}
	frameCount := int(data[0]) + 1
	if frameCount < 2 {
		return 0, errInvalidWEBM
	}
	laceData := data[1:]
	switch lacing {
	case 1:
		if err := validateWebMXiphLacing(laceData, frameCount); err != nil {
			return 0, errInvalidWEBM
		}
	case 2:
		if len(laceData) == 0 || len(laceData)%frameCount != 0 {
			return 0, errInvalidWEBM
		}
	case 3:
		if err := validateWebMEBMLLacing(laceData, frameCount); err != nil {
			return 0, errInvalidWEBM
		}
	}
	return uint64(frameCount), nil
}

func validateWebMXiphLacing(data []byte, frameCount int) error {
	offset := 0
	frameBytes := 0
	for range frameCount - 1 {
		size := 0
		for {
			if offset >= len(data) {
				return errInvalidWEBM
			}
			part := int(data[offset])
			offset++
			size += part
			if part < 255 {
				break
			}
		}
		if size == 0 || size > len(data)-offset-frameBytes {
			return errInvalidWEBM
		}
		frameBytes += size
	}
	if len(data)-offset-frameBytes <= 0 {
		return errInvalidWEBM
	}
	return nil
}

func validateWebMEBMLLacing(data []byte, frameCount int) error {
	firstSize, sizeLength, unknown, err := readEBMLVINT(data, false)
	if err != nil || unknown || firstSize == 0 {
		return errInvalidWEBM
	}
	offset := sizeLength
	frameBytes := firstSize
	previousSize := firstSize
	for i := 1; i < frameCount-1; i++ {
		encodedDelta, deltaLength, _, err := readEBMLVINT(data[offset:], false)
		if err != nil {
			return errInvalidWEBM
		}
		offset += deltaLength
		bias := (uint64(1) << (uint(deltaLength)*7 - 1)) - 1
		var size uint64
		if encodedDelta >= bias {
			delta := encodedDelta - bias
			if previousSize > ^uint64(0)-delta {
				return errInvalidWEBM
			}
			size = previousSize + delta
		} else {
			delta := bias - encodedDelta
			if previousSize <= delta {
				return errInvalidWEBM
			}
			size = previousSize - delta
		}
		if size == 0 || frameBytes > ^uint64(0)-size {
			return errInvalidWEBM
		}
		frameBytes += size
		previousSize = size
	}
	if offset > len(data) {
		return errInvalidWEBM
	}
	remaining := len(data) - offset
	maxInt := uint64(^uint(0) >> 1)
	if frameBytes >= maxInt || int(frameBytes) >= remaining {
		return errInvalidWEBM
	}
	return nil
}

func webMBlockPresentation(clusterTimecode uint64, block webMBlockTimestamp, timeScale uint64, trackTimestampScale float64, defaultDurationNS uint64) (webMPresentationBlock, error) {
	relativeTimecode := int32(block.relativeTimecode)
	if block.relativeTimecode&0x8000 != 0 {
		relativeTimecode -= 1 << 16
	}
	startTicks := float64(clusterTimecode) + float64(relativeTimecode)*trackTimestampScale
	if !isFinite(startTicks) || startTicks < 0 {
		return webMPresentationBlock{}, errInvalidWEBM
	}
	startNS := startTicks * float64(timeScale)
	var durationNS float64
	var hasDuration bool
	if block.hasBlockDuration {
		durationNS = float64(block.blockDurationTicks) * trackTimestampScale * float64(timeScale)
		hasDuration = true
	} else if defaultDurationNS > 0 {
		durationNS = float64(defaultDurationNS) * float64(block.frameCount)
		hasDuration = true
	}
	if !isFinite(startNS) || (hasDuration && (!isFinite(durationNS) || durationNS <= 0)) {
		return webMPresentationBlock{}, errInvalidWEBM
	}
	return webMPresentationBlock{
		startNS:     startNS,
		durationNS:  durationNS,
		hasDuration: hasDuration,
	}, nil
}

func webMFrameTimelineEndNS(blocks []webMPresentationBlock, infoDurationNS float64) (float64, error) {
	if len(blocks) == 0 || !isFinite(infoDurationNS) || infoDurationNS <= 0 {
		return 0, errInvalidWEBM
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].startNS < blocks[j].startNS
	})

	maxEndNS := infoDurationNS
	for i, block := range blocks {
		var endNS float64
		switch {
		case block.hasDuration:
			endNS = block.startNS + block.durationNS
		case i+1 < len(blocks):
			endNS = blocks[i+1].startNS
		default:
			endNS = infoDurationNS
		}
		if !isFinite(endNS) || endNS <= block.startNS {
			return 0, errInvalidWEBM
		}
		if endNS > maxEndNS {
			maxEndNS = endNS
		}
	}
	return maxEndNS, nil
}

func parseWebMBlockGroup(data []byte) (webMBlockTimestamp, error) {
	const (
		blockID         = 0xa1
		blockDurationID = 0x9b
	)
	var (
		block             webMBlockTimestamp
		blockSeen         bool
		blockDurationSeen bool
		blockDuration     uint64
	)
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return webMBlockTimestamp{}, errInvalidWEBM
		}
		switch element.id {
		case blockID:
			if blockSeen {
				return webMBlockTimestamp{}, errInvalidWEBM
			}
			block, err = parseWebMBlock(element.payload)
			if err != nil {
				return webMBlockTimestamp{}, errInvalidWEBM
			}
			blockSeen = true
		case blockDurationID:
			if blockDurationSeen {
				return webMBlockTimestamp{}, errInvalidWEBM
			}
			blockDurationSeen = true
			blockDuration, err = ebmlUnsigned(element.payload)
			if err != nil || blockDuration == 0 {
				return webMBlockTimestamp{}, errInvalidWEBM
			}
		}
		data = element.rest
	}
	if !blockSeen {
		return webMBlockTimestamp{}, errInvalidWEBM
	}
	block.hasBlockDuration = blockDurationSeen
	block.blockDurationTicks = blockDuration
	return block, nil
}

type ebmlElement struct {
	id      uint64
	payload []byte
	rest    []byte
	unknown bool
}

func readEBMLElement(data []byte) (ebmlElement, error) {
	id, idLength, _, err := readEBMLVINT(data, true)
	if err != nil {
		return ebmlElement{}, errInvalidWEBM
	}
	size, sizeLength, unknown, err := readEBMLVINT(data[idLength:], false)
	if err != nil {
		return ebmlElement{}, errInvalidWEBM
	}
	headerLength := idLength + sizeLength
	if unknown {
		return ebmlElement{id: id, payload: data[headerLength:], unknown: true}, nil
	}
	maxInt := uint64(^uint(0) >> 1)
	if size > maxInt {
		return ebmlElement{}, errInvalidWEBM
	}
	remaining := len(data) - headerLength
	if remaining < 0 {
		return ebmlElement{}, errInvalidWEBM
	}
	sizeInt := int(size)
	if sizeInt > remaining {
		return ebmlElement{}, errInvalidWEBM
	}
	payloadEnd := headerLength + sizeInt
	return ebmlElement{id: id, payload: data[headerLength:payloadEnd], rest: data[payloadEnd:]}, nil
}

func readEBMLVINT(data []byte, identifier bool) (uint64, int, bool, error) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false, errInvalidWEBM
	}
	marker := byte(0x80)
	length := 1
	for data[0]&marker == 0 {
		marker >>= 1
		length++
	}
	maxLength := 8
	if identifier {
		maxLength = 4
	}
	if length > maxLength || len(data) < length {
		return 0, 0, false, errInvalidWEBM
	}
	value := uint64(data[0])
	if !identifier {
		value &= uint64(marker - 1)
	}
	for _, b := range data[1:length] {
		value = value<<8 | uint64(b)
	}
	if identifier {
		return value, length, false, nil
	}
	bits := uint(length * 7)
	unknown := value == (uint64(1)<<bits)-1
	return value, length, unknown, nil
}

func ebmlUnsigned(data []byte) (uint64, error) {
	if len(data) < 1 || len(data) > 8 {
		return 0, errInvalidWEBM
	}
	var value uint64
	for _, b := range data {
		value = value<<8 | uint64(b)
	}
	return value, nil
}

func ebmlFloat(data []byte) (float64, error) {
	switch len(data) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(data))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(data)), nil
	default:
		return 0, errInvalidWEBM
	}
}
