package qr

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
)

// DefaultQuietZone is the four module wide light border the standard requires
// around a symbol.
const DefaultQuietZone = 4

// Render size limits accepted by the HTTP layer and the renderers.
const (
	MinRenderSize = 64
	MaxRenderSize = 2048
)

// RenderOptions controls the rasterised and vector output.
type RenderOptions struct {
	// Size is the desired edge length of the image in pixels. The produced
	// image uses the largest whole-pixel module scale that fits, so the real
	// PNG size can be slightly smaller.
	Size int
	// QuietZone is the border width in modules. Zero selects DefaultQuietZone.
	QuietZone int
}

func (o RenderOptions) normalise() RenderOptions {
	if o.Size <= 0 {
		o.Size = 512
	}
	o.Size = clamp(o.Size, MinRenderSize, MaxRenderSize)
	if o.QuietZone <= 0 {
		o.QuietZone = DefaultQuietZone
	}
	if o.QuietZone > 16 {
		o.QuietZone = 16
	}
	return o
}

// gridSize returns the symbol size including the quiet zone, in modules.
func (c *Code) gridSize(quiet int) int { return c.size + quiet*2 }

// scaleFor returns the whole-pixel module scale used for a target image size.
func (c *Code) scaleFor(opts RenderOptions) int {
	grid := c.gridSize(opts.QuietZone)
	scale := opts.Size / grid
	if scale < 1 {
		scale = 1
	}
	return scale
}

// Image renders the symbol as a paletted black and white image.
func (c *Code) Image(opts RenderOptions) image.Image {
	opts = opts.normalise()
	quiet := opts.QuietZone
	scale := c.scaleFor(opts)
	dim := c.gridSize(quiet) * scale

	palette := color.Palette{color.White, color.Black}
	img := image.NewPaletted(image.Rect(0, 0, dim, dim), palette)
	// The zero pixel value is already white; only dark modules need filling.
	for y := 0; y < c.size; y++ {
		for x := 0; x < c.size; x++ {
			if !c.Module(x, y) {
				continue
			}
			px0 := (x + quiet) * scale
			py0 := (y + quiet) * scale
			for py := py0; py < py0+scale; py++ {
				row := img.Pix[py*img.Stride+px0 : py*img.Stride+px0+scale]
				for i := range row {
					row[i] = 1
				}
			}
		}
	}
	return img
}

// PNG renders the symbol as a PNG image.
func (c *Code) PNG(opts RenderOptions) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, c.Image(opts)); err != nil {
		return nil, fmt.Errorf("qr: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// SVG renders the symbol as a standalone SVG document. Dark modules are
// emitted as a single path built from horizontal runs, which keeps the
// document small and crisp at any zoom level.
func (c *Code) SVG(opts RenderOptions) []byte {
	opts = opts.normalise()
	quiet := opts.QuietZone
	grid := c.gridSize(quiet)

	var path strings.Builder
	for y := 0; y < c.size; y++ {
		x := 0
		for x < c.size {
			if !c.Module(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < c.size && c.Module(x+run, y) {
				run++
			}
			path.WriteString("M")
			path.WriteString(strconv.Itoa(x + quiet))
			path.WriteString(" ")
			path.WriteString(strconv.Itoa(y + quiet))
			path.WriteString("h")
			path.WriteString(strconv.Itoa(run))
			path.WriteString("v1h-")
			path.WriteString(strconv.Itoa(run))
			path.WriteString("z")
			x += run
		}
	}

	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	fmt.Fprintf(&buf,
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR Code">`+"\n",
		opts.Size, opts.Size, grid, grid)
	fmt.Fprintf(&buf, `<rect width="%d" height="%d" fill="#ffffff"/>`+"\n", grid, grid)
	if path.Len() > 0 {
		fmt.Fprintf(&buf, `<path fill="#000000" d="%s"/>`+"\n", path.String())
	}
	buf.WriteString("</svg>\n")
	return buf.Bytes()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
