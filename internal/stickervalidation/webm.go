package stickervalidation

import (
	"encoding/binary"
	"errors"
	"math"
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
		infoSeen          bool
		tracksSeen        bool
		clusterSeen       bool
		timeScale         uint64 = 1_000_000
		containerDuration float64
		width             int
		height            int
	)
	for len(data) > 0 {
		element, err := readEBMLElement(data)
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
			width, height, err = parseWebMTracks(element.payload)
			if err != nil {
				return 0, 0, 0, errInvalidWEBM
			}
		case ebmlClusterID:
			if err := validateWebMCluster(element.payload); err != nil {
				return 0, 0, 0, errInvalidWEBM
			}
			clusterSeen = true
		}
		data = element.rest
		if element.unknown {
			data = nil
		}
	}
	if !infoSeen || !tracksSeen || !clusterSeen || timeScale == 0 || !isFinite(containerDuration) || containerDuration <= 0 {
		return 0, 0, 0, errInvalidWEBM
	}
	seconds := containerDuration * float64(timeScale) / 1_000_000_000
	if !isFinite(seconds) || seconds <= 0 {
		return 0, 0, 0, errInvalidWEBM
	}
	return width, height, seconds, nil
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

func parseWebMTracks(data []byte) (int, int, error) {
	trackCount := 0
	var width, height int
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return 0, 0, errInvalidWEBM
		}
		if element.id == ebmlTrackEntryID {
			trackCount++
			if trackCount > 1 {
				return 0, 0, errInvalidWEBM
			}
			width, height, err = parseWebMTrack(element.payload)
			if err != nil {
				return 0, 0, errInvalidWEBM
			}
		}
		data = element.rest
	}
	if trackCount != 1 {
		return 0, 0, errInvalidWEBM
	}
	return width, height, nil
}

func parseWebMTrack(data []byte) (int, int, error) {
	const (
		trackTypeID = 0x83
		codecID     = 0x86
		videoID     = 0xe0
		audioID     = 0xe1
	)
	var (
		typeSeen  bool
		codecSeen bool
		videoSeen bool
		trackType uint64
		codec     string
		width     int
		height    int
	)
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return 0, 0, errInvalidWEBM
		}
		switch element.id {
		case trackTypeID:
			if typeSeen {
				return 0, 0, errInvalidWEBM
			}
			typeSeen = true
			trackType, err = ebmlUnsigned(element.payload)
			if err != nil {
				return 0, 0, errInvalidWEBM
			}
		case codecID:
			if codecSeen {
				return 0, 0, errInvalidWEBM
			}
			codecSeen = true
			codec = string(element.payload)
		case videoID:
			if videoSeen {
				return 0, 0, errInvalidWEBM
			}
			videoSeen = true
			width, height, err = parseWebMVideo(element.payload)
			if err != nil {
				return 0, 0, errInvalidWEBM
			}
		case audioID:
			return 0, 0, errInvalidWEBM
		}
		data = element.rest
	}
	if !typeSeen || trackType != 1 || !codecSeen || codec != "V_VP9" || !videoSeen {
		return 0, 0, errInvalidWEBM
	}
	return width, height, nil
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

func validateWebMCluster(data []byte) error {
	const (
		timecodeID    = 0xe7
		simpleBlockID = 0xa3
		blockGroupID  = 0xa0
	)
	timecodeSeen := false
	blockSeen := false
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return errInvalidWEBM
		}
		switch element.id {
		case timecodeID:
			if timecodeSeen {
				return errInvalidWEBM
			}
			timecodeSeen = true
			if _, err := ebmlUnsigned(element.payload); err != nil {
				return errInvalidWEBM
			}
		case simpleBlockID:
			if len(element.payload) < 4 {
				return errInvalidWEBM
			}
			blockSeen = true
		case blockGroupID:
			if err := validateWebMBlockGroup(element.payload); err != nil {
				return errInvalidWEBM
			}
			blockSeen = true
		}
		data = element.rest
	}
	if !timecodeSeen || !blockSeen {
		return errInvalidWEBM
	}
	return nil
}

func validateWebMBlockGroup(data []byte) error {
	const blockID = 0xa1
	blockSeen := false
	for len(data) > 0 {
		element, err := readEBMLElement(data)
		if err != nil || element.unknown {
			return errInvalidWEBM
		}
		if element.id == blockID {
			if blockSeen || len(element.payload) < 4 {
				return errInvalidWEBM
			}
			blockSeen = true
		}
		data = element.rest
	}
	if !blockSeen {
		return errInvalidWEBM
	}
	return nil
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
