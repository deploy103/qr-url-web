package qrdecode

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"regexp"
	"strconv"
)

// bitmap is a binarised view of an image: true means a dark module.
type bitmap struct {
	w, h int
	dark []bool
}

func (b *bitmap) at(x, y int) bool {
	if x < 0 || y < 0 || x >= b.w || y >= b.h {
		return false
	}
	return b.dark[y*b.w+x]
}

// binarise thresholds an image at the midpoint between its darkest and
// lightest pixel rather than at a fixed grey, so a scan with poor contrast or
// a photograph with uneven exposure still separates cleanly.
func binarise(img image.Image) *bitmap {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	luma := make([]int, w*h)

	minLuma, maxLuma := 1<<31-1, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			var l int
			if a == 0 {
				l = 0xffff // treat fully transparent as background
			} else {
				l = (299*int(r) + 587*int(g) + 114*int(bl)) / 1000
			}
			luma[y*w+x] = l
			if l < minLuma {
				minLuma = l
			}
			if l > maxLuma {
				maxLuma = l
			}
		}
	}

	threshold := (minLuma + maxLuma) / 2
	if maxLuma-minLuma < 0x1000 {
		// An almost uniform image has no usable midpoint; fall back to the
		// halfway grey so a blank image simply yields no dark modules.
		threshold = 0x8000
	}

	out := &bitmap{w: w, h: h, dark: make([]bool, w*h)}
	for i, l := range luma {
		out.dark[i] = l < threshold
	}
	return out
}

// inverted returns the negative, which decodes a light-on-dark symbol.
func (b *bitmap) inverted() *bitmap {
	out := &bitmap{w: b.w, h: b.h, dark: make([]bool, len(b.dark))}
	for i, v := range b.dark {
		out.dark[i] = !v
	}
	return out
}

// rotated90 turns the bitmap a quarter turn clockwise.
func (b *bitmap) rotated90() *bitmap {
	out := &bitmap{w: b.h, h: b.w, dark: make([]bool, len(b.dark))}
	for y := 0; y < b.h; y++ {
		for x := 0; x < b.w; x++ {
			if b.dark[y*b.w+x] {
				nx, ny := b.h-1-y, x
				out.dark[ny*out.w+nx] = true
			}
		}
	}
	return out
}

// gridFromBitmap locates the symbol by trimming the quiet zone, derives the
// module pitch from the top left finder pattern and samples the centre of
// every module. The pitch is kept as a float so a resized image, where one
// module is not a whole number of pixels, still samples correctly.
func gridFromBitmap(b *bitmap) (*Grid, error) {
	if b.w < 21 || b.h < 21 {
		return nil, errors.New("qrdecode: image is too small to contain a symbol")
	}

	minX, minY, maxX, maxY := b.w, b.h, -1, -1
	for y := 0; y < b.h; y++ {
		for x := 0; x < b.w; x++ {
			if !b.dark[y*b.w+x] {
				continue
			}
			minX = min(minX, x)
			minY = min(minY, y)
			maxX = max(maxX, x)
			maxY = max(maxY, y)
		}
	}
	if maxX < minX || maxY < minY {
		return nil, errors.New("qrdecode: image contains no dark modules")
	}

	width := float64(maxX - minX + 1)
	height := float64(maxY - minY + 1)
	// A rendered symbol is square. Allow a pixel of slack for rounding after a
	// resize, but reject anything that is clearly not a QR Code.
	if ratio := width / height; ratio < 0.95 || ratio > 1.05 {
		return nil, fmt.Errorf("qrdecode: symbol bounding box is not square (%.0fx%.0f)", width, height)
	}

	// The first row of the symbol starts with the seven dark modules of the
	// top left finder pattern.
	run := 0
	for x := minX; x <= maxX && b.at(x, minY); x++ {
		run++
	}
	if run < 7 {
		return nil, fmt.Errorf("qrdecode: cannot derive the module pitch (finder run %d)", run)
	}
	pitch := float64(run) / 7.0
	size := int(width/pitch + 0.5)
	if size < 21 || size > 177 || (size-17)%4 != 0 {
		return nil, fmt.Errorf("qrdecode: derived module size %d is not a valid symbol size", size)
	}

	// Re-derive the pitch from the full width so rounding errors do not
	// accumulate across the symbol.
	pitchX := width / float64(size)
	pitchY := height / float64(size)

	g := NewGrid(size)
	for y := 0; y < size; y++ {
		py := int(float64(minY) + (float64(y)+0.5)*pitchY)
		for x := 0; x < size; x++ {
			px := int(float64(minX) + (float64(x)+0.5)*pitchX)
			g.Set(x, y, b.at(px, py))
		}
	}
	return g, nil
}

// DecodeImage locates the symbol inside an image and decodes it.
//
// The image may be scaled to any size, may carry an arbitrary light border and
// may be rotated by a multiple of a quarter turn, in either polarity. What it
// may not be is skewed: there is no perspective correction, so a photograph
// taken at an angle will not decode.
func DecodeImage(img image.Image) (*Result, error) {
	base := binarise(img)

	var firstErr error
	for _, polarity := range []*bitmap{base, base.inverted()} {
		candidate := polarity
		for turn := 0; turn < 4; turn++ {
			if turn > 0 {
				candidate = candidate.rotated90()
			}
			g, err := gridFromBitmap(candidate)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			res, err := DecodeGrid(g)
			if err == nil {
				return res, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr == nil {
		firstErr = errors.New("qrdecode: no symbol found")
	}
	return nil, firstErr
}

// DecodePNG decodes a QR Code from encoded PNG bytes.
func DecodePNG(data []byte) (*Result, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("qrdecode: decode png: %w", err)
	}
	return DecodeImage(img)
}

var (
	svgViewBoxRe = regexp.MustCompile(`viewBox="0 0 (\d+) (\d+)"`)
	svgPathRe    = regexp.MustCompile(`<path[^>]*\sd="([^"]*)"`)
	svgRunRe     = regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`)
)

// DecodeSVG decodes a QR Code from an SVG document produced by the internal/qr
// renderer, which emits dark modules as horizontal unit-height runs.
func DecodeSVG(data []byte) (*Result, error) {
	doc := string(data)
	vb := svgViewBoxRe.FindStringSubmatch(doc)
	if vb == nil {
		return nil, errors.New("qrdecode: svg has no recognisable viewBox")
	}
	gridSize, err := strconv.Atoi(vb[1])
	if err != nil {
		return nil, fmt.Errorf("qrdecode: bad viewBox: %w", err)
	}
	pm := svgPathRe.FindStringSubmatch(doc)
	if pm == nil {
		return nil, errors.New("qrdecode: svg has no module path")
	}

	type run struct{ x, y, n int }
	var runs []run
	minX, minY := gridSize, gridSize
	maxX, maxY := -1, -1
	matches := svgRunRe.FindAllStringSubmatch(pm[1], -1)
	if len(matches) == 0 {
		return nil, errors.New("qrdecode: svg path contains no module runs")
	}
	consumed := 0
	for _, m := range matches {
		consumed += len(m[0])
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		n, _ := strconv.Atoi(m[3])
		if m[3] != m[4] {
			return nil, errors.New("qrdecode: svg run is not a closed rectangle")
		}
		runs = append(runs, run{x, y, n})
		minX, minY = min(minX, x), min(minY, y)
		maxX, maxY = max(maxX, x+n-1), max(maxY, y)
	}
	if consumed != len(pm[1]) {
		return nil, errors.New("qrdecode: svg path contains unexpected commands")
	}

	size := max(maxX-minX+1, maxY-minY+1)
	if size < 21 || size > 177 || (size-17)%4 != 0 {
		return nil, fmt.Errorf("qrdecode: derived module size %d is not a valid symbol size", size)
	}
	g := NewGrid(size)
	for _, r := range runs {
		for i := 0; i < r.n; i++ {
			g.Set(r.x-minX+i, r.y-minY, true)
		}
	}
	return DecodeGrid(g)
}
