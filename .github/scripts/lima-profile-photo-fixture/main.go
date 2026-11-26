package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
)

const (
	photoWidth  = 1600
	photoHeight = 1600
	photoBytes  = 256 * 1024
	photoPath   = "lima-memory-photo.jpg"
)

func main() {
	if len(os.Args) != 1 {
		fatal(errors.New("generate-lima-profile-photo takes no arguments"))
	}
	if err := generate(); err != nil {
		fatal(err)
	}
}

func generate() error {
	pixels := image.NewRGBA(image.Rect(0, 0, photoWidth, photoHeight))
	for y := range photoHeight {
		for x := range photoWidth {
			pixels.SetRGBA(x, y, color.RGBA{
				R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, pixels, &jpeg.Options{Quality: 85}); err != nil {
		return fmt.Errorf("encode profile photo fixture: %w", err)
	}
	base := encoded.Bytes()
	padding := photoBytes - len(base)
	if padding < 4 {
		return fmt.Errorf("encoded JPEG is %d bytes, too large for %d-byte fixture", len(base), photoBytes)
	}
	var output bytes.Buffer
	output.Grow(photoBytes)
	output.Write(base[:2])
	for padding > 0 {
		segmentSize := min(padding, 65537)
		if remaining := padding - segmentSize; remaining > 0 && remaining < 4 {
			segmentSize -= 4 - remaining
		}
		if segmentSize < 4 {
			return fmt.Errorf("invalid JPEG padding segment size %d", segmentSize)
		}
		output.Write([]byte{0xff, 0xe1, 0, 0})
		binary.BigEndian.PutUint16(output.Bytes()[output.Len()-2:], uint16(segmentSize-2)) //nolint:gosec // segmentSize is capped at 65,537, so the JPEG length fits uint16.
		output.Write(make([]byte, segmentSize-4))
		padding -= segmentSize
	}
	output.Write(base[2:])
	if output.Len() != photoBytes {
		return fmt.Errorf("generated JPEG is %d bytes, want %d", output.Len(), photoBytes)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(output.Bytes()))
	if err != nil {
		return fmt.Errorf("decode generated profile photo fixture: %w", err)
	}
	if config.Width != photoWidth || config.Height != photoHeight {
		return fmt.Errorf("generated JPEG dimensions are %dx%d, want %dx%d", config.Width, config.Height, photoWidth, photoHeight)
	}
	file, err := os.OpenFile(photoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create profile photo fixture: %w", err)
	}
	written, err := file.Write(output.Bytes())
	if err != nil || written != output.Len() {
		if err == nil {
			err = fmt.Errorf("short write: wrote %d of %d bytes", written, output.Len())
		}
		closeErr := file.Close()
		return errors.Join(fmt.Errorf("write profile photo fixture: %w", err), closeErr)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close profile photo fixture: %w", err)
	}
	if _, err := fmt.Printf("Generated %d-byte %dx%d JPEG profile photo fixture\n", photoBytes, photoWidth, photoHeight); err != nil {
		return fmt.Errorf("write fixture result: %w", err)
	}
	return nil
}

func fatal(err error) {
	if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
		os.Exit(1)
	}
	os.Exit(1)
}
