package testutil

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// gradient fills an RGBA canvas with a deterministic per-pixel pattern, so
// every caller of JPEG and PNG gets the same bytes for the same dimensions
// and decoded pixel equality between an image and a transformed copy of it
// can be asserted exactly.
func gradient(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 17) % 256),
				G: uint8((y * 31) % 256),
				B: uint8((x + y*5) % 256),
				A: 255,
			})
		}
	}
	return img
}

// JPEG returns the bytes of a valid, decodable width x height JPEG holding a
// deterministic gradient. Tests that must exercise revalidation or metadata
// stripping against bytes a real decoder accepts use it as their base.
func JPEG(t testing.TB, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradient(width, height), &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// PNG returns the bytes of a valid, decodable width x height PNG holding a
// deterministic gradient, the PNG counterpart of JPEG.
func PNG(t testing.TB, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, gradient(width, height)); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}
