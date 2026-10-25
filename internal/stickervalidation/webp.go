package stickervalidation

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/image/webp"
)

var errInvalidWEBP = errors.New("invalid WEBP")
var errWEBPDimensions = errors.New("invalid WEBP dimensions")

func validateWEBP(data []byte, target Target) (int, int, error) {
	width, height, err := webpHeaderDimensions(data)
	if err != nil {
		return 0, 0, errInvalidWEBP
	}
	if !validDimensions(target, width, height) {
		return width, height, errWEBPDimensions
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, 0, errInvalidWEBP
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != width || bounds.Dy() != height {
		return 0, 0, errInvalidWEBP
	}
	return width, height, nil
}

func webpHeaderDimensions(data []byte) (int, int, error) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errInvalidWEBP
	}
	if uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return 0, 0, errInvalidWEBP
	}

	var (
		offset       = 12
		chunkIndex   int
		seenVP8X     bool
		seenImage    bool
		canvasWidth  int
		canvasHeight int
		imageWidth   int
		imageHeight  int
		err          error
	)
	for offset < len(data) {
		if len(data)-offset < 8 {
			return 0, 0, errInvalidWEBP
		}
		fourCC := string(data[offset : offset+4])
		chunkSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		chunkStart := offset + 8
		remaining := len(data) - chunkStart
		if remaining < 0 || uint64(chunkSize) > uint64(remaining) {
			return 0, 0, errInvalidWEBP
		}
		chunkSizeInt := int(chunkSize)
		chunkEnd := chunkStart + chunkSizeInt
		paddedEnd := chunkEnd + chunkSizeInt%2
		if paddedEnd > len(data) {
			return 0, 0, errInvalidWEBP
		}
		payload := data[chunkStart:chunkEnd]
		switch fourCC {
		case "ANIM", "ANMF":
			return 0, 0, errInvalidWEBP
		case "VP8X":
			if chunkIndex != 0 || seenVP8X || len(payload) != 10 {
				return 0, 0, errInvalidWEBP
			}
			const animationFlag = 1 << 1
			if payload[0]&animationFlag != 0 || payload[0]&^byte(0x3e) != 0 || payload[1] != 0 || payload[2] != 0 || payload[3] != 0 {
				return 0, 0, errInvalidWEBP
			}
			seenVP8X = true
			canvasWidth = int(readUint24(payload[4:7])) + 1
			canvasHeight = int(readUint24(payload[7:10])) + 1
		case "VP8 ":
			if seenImage {
				return 0, 0, errInvalidWEBP
			}
			imageWidth, imageHeight, err = vp8FrameDimensions(payload)
			if err != nil {
				return 0, 0, errInvalidWEBP
			}
			seenImage = true
		case "VP8L":
			if seenImage {
				return 0, 0, errInvalidWEBP
			}
			imageWidth, imageHeight, err = vp8LosslessDimensions(payload)
			if err != nil {
				return 0, 0, errInvalidWEBP
			}
			seenImage = true
		}
		offset = paddedEnd
		chunkIndex++
	}
	if !seenImage {
		return 0, 0, errInvalidWEBP
	}
	if seenVP8X {
		if canvasWidth != imageWidth || canvasHeight != imageHeight {
			return 0, 0, errInvalidWEBP
		}
		return canvasWidth, canvasHeight, nil
	}
	return imageWidth, imageHeight, nil
}

func vp8FrameDimensions(data []byte) (int, int, error) {
	if len(data) < 10 || data[0]&1 != 0 || !bytes.Equal(data[3:6], []byte{0x9d, 0x01, 0x2a}) {
		return 0, 0, errInvalidWEBP
	}
	width := int(binary.LittleEndian.Uint16(data[6:8]) & 0x3fff)
	height := int(binary.LittleEndian.Uint16(data[8:10]) & 0x3fff)
	if width == 0 || height == 0 {
		return 0, 0, errInvalidWEBP
	}
	return width, height, nil
}

func vp8LosslessDimensions(data []byte) (int, int, error) {
	if len(data) < 5 || data[0] != 0x2f || data[4]>>5 != 0 {
		return 0, 0, errInvalidWEBP
	}
	width := int(uint32(data[1]) | uint32(data[2]&0x3f)<<8)
	height := int(uint32(data[2]>>6) | uint32(data[3])<<2 | uint32(data[4]&0x0f)<<10)
	return width + 1, height + 1, nil
}

func readUint24(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16
}
