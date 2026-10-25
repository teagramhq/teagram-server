package stickervalidation

import (
	"bytes"
	"errors"
	"time"
)

const (
	maxStickerWEBPBytes   = 512 * 1024
	maxStickerTGSBytes    = 64 * 1024
	maxStickerWEBMBytes   = 256 * 1024
	maxThumbnailWEBPBytes = 128 * 1024
	maxThumbnailAnimBytes = 32 * 1024
	maxTGSInflatedBytes   = 1 * 1024 * 1024
)

type Target string

const (
	TargetSticker      Target = "sticker"
	TargetMaskSticker  Target = "mask_sticker"
	TargetCustomEmoji  Target = "custom_emoji"
	TargetSetThumbnail Target = "set_thumbnail"
)

type Format string

const (
	FormatWEBP Format = "WEBP"
	FormatTGS  Format = "TGS"
	FormatWEBM Format = "WEBM"
)

type Metadata struct {
	Format   Format
	Width    int
	Height   int
	Duration time.Duration
}

type ValidationError struct {
	Reason string
}

func (e *ValidationError) Error() string { return e.Reason }

type sizeLimits struct {
	webp int
	tgs  int
	webm int
}

// Validate checks stored sticker bytes without consulting client metadata.
func Validate(data []byte, target Target) (metadata Metadata, err error) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
			err = reject("STICKER_FILE_INVALID")
		}
	}()

	limits, ok := targetSizeLimits(target)
	if !ok {
		return Metadata{}, reject("STICKER_FILE_INVALID")
	}
	maxSize := max(limits.webp, limits.tgs, limits.webm)
	if len(data) == 0 {
		return Metadata{}, reject("STICKER_FILE_INVALID")
	}
	if len(data) > maxSize {
		return Metadata{}, reject("STICKER_FILE_SIZE")
	}

	switch {
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		if len(data) > limits.webp {
			return Metadata{}, reject("STICKER_FILE_SIZE")
		}
		width, height, err := validateWEBP(data, target)
		if err != nil {
			if errors.Is(err, errWEBPDimensions) {
				return Metadata{}, reject("STICKER_PNG_DIMENSIONS")
			}
			return Metadata{}, reject("STICKER_FILE_INVALID")
		}
		return Metadata{Format: FormatWEBP, Width: width, Height: height}, nil
	case len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b:
		if len(data) > limits.tgs {
			return Metadata{}, reject("STICKER_FILE_SIZE")
		}
		width, height, duration, err := validateTGS(data, target)
		if err != nil {
			return Metadata{}, reject("STICKER_TGS_NOTGS")
		}
		if !validDimensions(target, width, height) {
			return Metadata{}, reject("STICKER_PNG_DIMENSIONS")
		}
		return Metadata{Format: FormatTGS, Width: width, Height: height, Duration: duration}, nil
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
		if len(data) > limits.webm {
			return Metadata{}, reject("STICKER_FILE_SIZE")
		}
		width, height, duration, err := validateWEBM(data)
		if err != nil {
			return Metadata{}, reject("STICKER_VIDEO_NOWEBM")
		}
		if !validDimensions(target, width, height) {
			return Metadata{}, reject("STICKER_PNG_DIMENSIONS")
		}
		if duration > 3 {
			return Metadata{}, reject("STICKER_VIDEO_DURATION")
		}
		return Metadata{Format: FormatWEBM, Width: width, Height: height, Duration: time.Duration(duration * float64(time.Second))}, nil
	default:
		return Metadata{}, reject("STICKER_FILE_INVALID")
	}
}

func targetSizeLimits(target Target) (sizeLimits, bool) {
	switch target {
	case TargetSticker, TargetMaskSticker, TargetCustomEmoji:
		return sizeLimits{webp: maxStickerWEBPBytes, tgs: maxStickerTGSBytes, webm: maxStickerWEBMBytes}, true
	case TargetSetThumbnail:
		return sizeLimits{webp: maxThumbnailWEBPBytes, tgs: maxThumbnailAnimBytes, webm: maxThumbnailAnimBytes}, true
	default:
		return sizeLimits{}, false
	}
}

func validDimensions(target Target, width, height int) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	switch target {
	case TargetSticker, TargetMaskSticker:
		return width <= 512 && height <= 512 && (width == 512 || height == 512)
	case TargetCustomEmoji, TargetSetThumbnail:
		return width == 100 && height == 100
	default:
		return false
	}
}

func reject(reason string) error {
	return &ValidationError{Reason: reason}
}
