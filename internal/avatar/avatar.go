// Package avatar renders an agent's default avatar and validates uploaded ones.
//
// The generator is a faithful port of the authoritative "Appendix A" algorithm in docs/ROADMAP.md:
// 128x128 canvas, an 8x8 grid of 16px blocks, a transparent background, ONE random colour from a
// fixed 16-colour palette for the whole image, left->right mirroring (only the 4x8 left half is
// chosen, mirrored to the right), and a fixed ~33% density of 11 mirrored pairs. The only change
// from the reference is dropping the package main / os.Create / println wrappers (encode the PNG in
// memory) and seeding a dedicated *rand.Rand so a given agent name always yields the same image.
package avatar

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
)

const (
	// Size is the exact side length (px) an avatar must have, custom or generated.
	Size = 128
	// MIMEPNG / MIMEJPEG are the only accepted avatar encodings.
	MIMEPNG  = "image/png"
	MIMEJPEG = "image/jpeg"
)

// palette is the fixed 16-colour Material-style set from Appendix A: optimized for contrast on both
// light and dark backgrounds. One colour is chosen at random and used for the whole avatar.
var palette = []color.RGBA{
	{211, 47, 47, 255},  // Red
	{194, 24, 91, 255},  // Pink
	{123, 31, 162, 255}, // Purple
	{81, 45, 168, 255},  // Deep Purple
	{48, 63, 159, 255},  // Indigo
	{25, 118, 210, 255}, // Blue
	{2, 136, 209, 255},  // Light Blue
	{0, 151, 167, 255},  // Cyan
	{0, 121, 107, 255},  // Teal
	{56, 142, 60, 255},  // Green
	{104, 159, 56, 255}, // Light Green
	{158, 157, 36, 255}, // Olive
	{245, 127, 23, 255}, // Dark Amber
	{245, 124, 0, 255},  // Orange
	{230, 74, 25, 255},  // Deep Orange
	{84, 110, 122, 255}, // Blue Grey
}

// PNG returns a deterministic default avatar for name as PNG bytes. The same name always produces
// the same image (seeded from a hash of the name), so defaults are stable across sessions/deploys.
func PNG(name string) []byte {
	seed := int64(binary.BigEndian.Uint64(sha256sum(name)[:8]))
	rng := rand.New(rand.NewSource(seed))
	return generate(rng)
}

// generate is the Appendix A body, operating on the supplied rng and encoding to memory.
func generate(rng *rand.Rand) []byte {
	const imgSize = Size
	const gridSize = 8
	blockSize := imgSize / gridSize // 16 pixels per block

	// image.NewRGBA initializes all pixels to transparent black (R=0,G=0,B=0,A=0): a transparent
	// background, NOT filled black.
	img := image.NewRGBA(image.Rect(0, 0, imgSize, imgSize))

	// Pick ONE random colour from the palette for this entire avatar.
	avatarColor := palette[rng.Intn(len(palette))]

	// ~33% density: fill round(32 * 0.33) = 11 mirrored pairs (the 4x8 left half has 32 blocks).
	halfWidth := gridSize / 2
	halfBlocks := halfWidth * gridSize
	pairsToFill := int(math.Round(float64(halfBlocks) * 0.33))

	// Index the LEFT half only [0..31], then Fisher-Yates shuffle.
	indices := make([]int, halfBlocks)
	for i := range indices {
		indices[i] = i
	}
	for i := len(indices) - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		indices[i], indices[j] = indices[j], indices[i]
	}

	// Draw the selected blocks and their horizontal mirrors.
	for i := 0; i < pairsToFill; i++ {
		idx := indices[i]
		col := idx % halfWidth // 0..3 (left half columns)
		row := idx / halfWidth // 0..7

		leftX := col * blockSize
		leftY := row * blockSize
		rightX := (gridSize - 1 - col) * blockSize
		rightY := row * blockSize

		for dy := 0; dy < blockSize; dy++ {
			for dx := 0; dx < blockSize; dx++ {
				img.SetRGBA(leftX+dx, leftY+dy, avatarColor)
				img.SetRGBA(rightX+dx, rightY+dy, avatarColor)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil { // should never fail for an in-memory RGBA
		panic("avatar: png encode: " + err.Error())
	}
	return buf.Bytes()
}

// Validate decodes data as PNG or JPEG and requires it to be exactly Size x Size. It returns the
// detected MIME type, or an error describing why the image is not an acceptable avatar. Dimension
// checking is on the decoded image, never on headers.
func Validate(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty image")
	}
	var (
		img  image.Image
		mime string
		err  error
	)
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")): // JPEG SOI marker
		mime = MIMEJPEG
		img, err = jpeg.Decode(bytes.NewReader(data))
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		mime = MIMEPNG
		img, err = png.Decode(bytes.NewReader(data))
	default:
		return "", fmt.Errorf("not a PNG or JPEG")
	}
	if err != nil {
		return "", fmt.Errorf("decode failed: %v", err)
	}
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size {
		return "", fmt.Errorf("must be exactly %dx%d px, got %dx%d px", Size, Size, b.Dx(), b.Dy())
	}
	return mime, nil
}

func sha256sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
