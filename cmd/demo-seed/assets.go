package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

// avatarJPEG builds an abstract, fictional avatar locally. It has no external
// image dependency, identifiable person, or brand mark.
func avatarJPEG(index int) ([]byte, error) {
	if index < 0 {
		return nil, errors.New("avatar index must not be negative")
	}
	const size = 384
	palette := [][3]uint8{
		{28, 77, 94}, {45, 89, 78}, {92, 72, 119}, {125, 78, 65}, {45, 81, 133},
		{118, 98, 48}, {73, 87, 101}, {117, 67, 94}, {59, 101, 107}, {97, 78, 51},
	}
	base := palette[index%len(palette)]
	accent := palette[(index*3+2)%len(palette)]
	canvas := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: base[0], G: base[1], B: base[2], A: 255}}, image.Point{}, draw.Src)

	// Concentric, offset shapes and a quiet orbit make each generated file
	// visually distinct while keeping the source deterministic and reviewable.
	cx := 150 + (index*29)%86
	cy := 158 + (index*17)%68
	for ring := 5; ring >= 0; ring-- {
		radius := 58 + ring*28
		alpha := uint8(42 + ring*9)
		shade := color.RGBA{R: accent[0], G: accent[1], B: accent[2], A: alpha}
		fillCircle(canvas, cx, cy, radius, shade)
	}
	for stripe := range 6 {
		x0 := -30 + stripe*79
		for offset := -8; offset <= 8; offset++ {
			drawLine(canvas, x0+offset, 0, x0+size/2+offset, size, color.RGBA{R: 238, G: 226, B: 187, A: 44})
		}
	}

	for n := range 10 {
		x := (index*31 + n*71 + n*n*13) % size
		y := (index*53 + n*47 + n*n*29) % size
		radius := 3 + (index+n*7)%10
		fillCircle(canvas, x, y, radius, color.RGBA{R: 249, G: 235, B: 194, A: 100})
	}

	// A small off-center sun gives the pattern a clear focal point.
	sunX := size/2 + (index%3-1)*34
	sunY := 90 + index%5*9
	fillCircle(canvas, sunX, sunY, 32, color.RGBA{R: 245, G: 196, B: 112, A: 238})
	fillCircle(canvas, sunX, sunY, 20, color.RGBA{R: 255, G: 226, B: 165, A: 255})

	var output bytes.Buffer
	if err := jpeg.Encode(&output, canvas, &jpeg.Options{Quality: 86}); err != nil {
		return nil, fmt.Errorf("encode generated avatar: %w", err)
	}
	return output.Bytes(), nil
}

func fillCircle(target *image.RGBA, centerX, centerY, radius int, shade color.RGBA) {
	left := max(0, centerX-radius)
	top := max(0, centerY-radius)
	right := min(target.Bounds().Dx(), centerX+radius+1)
	bottom := min(target.Bounds().Dy(), centerY+radius+1)
	radiusSquared := radius * radius
	for y := top; y < bottom; y++ {
		for x := left; x < right; x++ {
			dx := x - centerX
			dy := y - centerY
			if dx*dx+dy*dy <= radiusSquared {
				blendPixel(target, x, y, shade)
			}
		}
	}
}

func drawLine(target *image.RGBA, x0, y0, x1, y1 int, shade color.RGBA) {
	dx := x1 - x0
	dy := y1 - y0
	steps := max(absInt(dx), absInt(dy))
	if steps == 0 {
		return
	}
	for step := 0; step <= steps; step++ {
		x := x0 + dx*step/steps
		y := y0 + dy*step/steps
		if image.Pt(x, y).In(target.Bounds()) {
			blendPixel(target, x, y, shade)
		}
	}
}

func blendPixel(target *image.RGBA, x, y int, source color.RGBA) {
	destination := target.RGBAAt(x, y)
	alpha := uint32(source.A)
	inverse := uint32(255) - alpha
	target.SetRGBA(x, y, color.RGBA{
		R: blendChannel(source.R, destination.R, alpha, inverse),
		G: blendChannel(source.G, destination.G, alpha, inverse),
		B: blendChannel(source.B, destination.B, alpha, inverse),
		A: 255,
	})
}

func blendChannel(source, destination uint8, alpha, inverse uint32) uint8 {
	blended := (uint32(source)*alpha + uint32(destination)*inverse) / 255
	return uint8(min(blended, uint32(255)))
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
