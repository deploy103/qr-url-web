// Package qr implements QR Code (ISO/IEC 18004) generation entirely in Go.
//
// No external service, network call or third party library is involved: the
// data encoding, Reed-Solomon error correction, module placement, masking and
// rendering are all performed locally. The payload handed to Encode is stored
// verbatim inside the symbol, so decoding a generated code always yields the
// original input.
package qr

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxPayloadBytes is the largest payload a version 40 level L symbol can hold
// in byte mode.
const MaxPayloadBytes = 2953

// ErrEmptyPayload is returned by Encode when the payload has no content.
var ErrEmptyPayload = errors.New("qr: payload is empty")

// Code is a fully rendered QR Code symbol.
type Code struct {
	version int
	size    int
	level   Level
	mask    int
	payload string

	modules  []bool // row-major, size*size
	function []bool // modules belonging to a function pattern
}

// Options tunes the symbol generation. The zero value selects level M, the
// smallest possible version and the mask with the lowest penalty score.
type Options struct {
	// Level is the error correction level.
	Level Level
	// MinVersion, when greater than zero, forces at least this symbol version.
	MinVersion int
	// Mask selects the data mask pattern 0-7. A negative value selects the
	// mask with the lowest penalty score, as recommended by the standard.
	Mask int
}

// Encode builds a QR Code for text using the given error correction level,
// the smallest fitting version and the best scoring mask.
func Encode(text string, level Level) (*Code, error) {
	return EncodeWithOptions(text, Options{Level: level, Mask: -1})
}

// EncodeWithOptions builds a QR Code for text with explicit options.
func EncodeWithOptions(text string, opts Options) (*Code, error) {
	if text == "" {
		return nil, ErrEmptyPayload
	}
	if len(text) > MaxPayloadBytes {
		return nil, ErrTooLong
	}
	if !utf8.ValidString(text) {
		return nil, errors.New("qr: payload is not valid UTF-8")
	}
	if !opts.Level.Valid() {
		return nil, fmt.Errorf("qr: invalid error correction level %d", opts.Level)
	}
	if opts.Mask > 7 {
		return nil, fmt.Errorf("qr: invalid mask pattern %d", opts.Mask)
	}
	if opts.MinVersion > MaxVersion {
		return nil, fmt.Errorf("qr: invalid minimum version %d", opts.MinVersion)
	}

	m := selectMode(text)
	version, err := chooseVersion(m, text, opts.Level, opts.MinVersion)
	if err != nil {
		return nil, err
	}
	codewords, err := buildCodewords(text, opts.Level, version, m)
	if err != nil {
		return nil, err
	}

	size := version*4 + 17
	c := &Code{
		version:  version,
		size:     size,
		level:    opts.Level,
		payload:  text,
		modules:  make([]bool, size*size),
		function: make([]bool, size*size),
	}
	c.drawFunctionPatterns()
	c.drawCodewords(codewords)
	c.applyBestMask(opts.Mask)
	return c, nil
}

// Version returns the symbol version, between 1 and 40.
func (c *Code) Version() int { return c.version }

// Size returns the side length of the symbol in modules, excluding the quiet
// zone.
func (c *Code) Size() int { return c.size }

// Level returns the error correction level of the symbol.
func (c *Code) Level() Level { return c.level }

// Mask returns the data mask pattern that was applied, between 0 and 7.
func (c *Code) Mask() int { return c.mask }

// Payload returns the exact string that was encoded into the symbol.
func (c *Code) Payload() string { return c.payload }

// Module reports whether the module at column x, row y is dark. Coordinates
// outside the symbol are reported as light, which matches the quiet zone.
func (c *Code) Module(x, y int) bool {
	if x < 0 || y < 0 || x >= c.size || y >= c.size {
		return false
	}
	return c.modules[y*c.size+x]
}

func (c *Code) set(x, y int, dark bool) { c.modules[y*c.size+x] = dark }

func (c *Code) setFunction(x, y int, dark bool) {
	c.modules[y*c.size+x] = dark
	c.function[y*c.size+x] = true
}

func (c *Code) isFunction(x, y int) bool { return c.function[y*c.size+x] }

// drawFunctionPatterns draws the finder patterns, separators, timing patterns,
// alignment patterns, the dark module and reserves the format and version
// information areas.
func (c *Code) drawFunctionPatterns() {
	// Timing patterns.
	for i := 0; i < c.size; i++ {
		dark := i%2 == 0
		c.setFunction(6, i, dark)
		c.setFunction(i, 6, dark)
	}

	// Finder patterns with their separators.
	c.drawFinderPattern(3, 3)
	c.drawFinderPattern(c.size-4, 3)
	c.drawFinderPattern(3, c.size-4)

	// Alignment patterns, skipping the three that would overlap a finder.
	positions := alignmentPatternPositions(c.version)
	last := len(positions) - 1
	for i, cy := range positions {
		for j, cx := range positions {
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			c.drawAlignmentPattern(cx, cy)
		}
	}

	// Reserve the format and version information areas; the real bits are
	// written once the mask is known.
	c.drawFormatBits(0)
	c.drawVersionBits()
}

func (c *Code) drawFinderPattern(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= c.size || y >= c.size {
				continue
			}
			dist := max(abs(dx), abs(dy)) // Chebyshev distance
			c.setFunction(x, y, dist != 2 && dist != 4)
		}
	}
}

func (c *Code) drawAlignmentPattern(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			c.setFunction(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

// formatInfoBits returns the 15 bit format information word for the given
// error correction level and mask pattern.
func formatInfoBits(level Level, mask int) uint32 {
	data := levelFormatBits[level]<<3 | uint32(mask)
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

// drawFormatBits writes both copies of the format information for the given
// mask pattern.
func (c *Code) drawFormatBits(mask int) {
	bits := formatInfoBits(c.level, mask)
	get := func(i int) bool { return (bits>>uint(i))&1 == 1 }

	// Copy near the top-left finder pattern.
	for i := 0; i <= 5; i++ {
		c.setFunction(8, i, get(i))
	}
	c.setFunction(8, 7, get(6))
	c.setFunction(8, 8, get(7))
	c.setFunction(7, 8, get(8))
	for i := 9; i < 15; i++ {
		c.setFunction(14-i, 8, get(i))
	}

	// Copy split across the top-right and bottom-left finder patterns.
	for i := 0; i < 8; i++ {
		c.setFunction(c.size-1-i, 8, get(i))
	}
	for i := 8; i < 15; i++ {
		c.setFunction(8, c.size-15+i, get(i))
	}
	c.setFunction(8, c.size-8, true) // the always dark module
}

// versionInfoBits returns the 18 bit version information word, or false when
// the version does not carry one.
func versionInfoBits(version int) (uint32, bool) {
	if version < 7 {
		return 0, false
	}
	rem := uint32(version)
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return uint32(version)<<12 | rem, true
}

func (c *Code) drawVersionBits() {
	bits, ok := versionInfoBits(c.version)
	if !ok {
		return
	}
	for i := 0; i < 18; i++ {
		dark := (bits>>uint(i))&1 == 1
		a := c.size - 11 + i%3
		b := i / 3
		c.setFunction(a, b, dark)
		c.setFunction(b, a, dark)
	}
}

// drawCodewords places the interleaved codeword stream into the symbol
// following the upward/downward zigzag of two module wide columns.
func (c *Code) drawCodewords(data []byte) {
	i := 0
	total := len(data) * 8
	for right := c.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // skip the vertical timing pattern column
		}
		for vert := 0; vert < c.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = c.size - 1 - vert
				}
				if c.isFunction(x, y) {
					continue
				}
				dark := false
				if i < total {
					dark = (data[i>>3]>>uint(7-i&7))&1 == 1
					i++
				}
				c.set(x, y, dark)
			}
		}
	}
}

// maskCondition reports whether the module at x, y is inverted by the given
// mask pattern.
func maskCondition(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	case 7:
		return ((x+y)%2+x*y%3)%2 == 0
	}
	panic("qr: invalid mask pattern")
}

// applyMask inverts every non-function module selected by the mask pattern.
// Applying the same mask twice restores the original matrix.
func (c *Code) applyMask(mask int) {
	for y := 0; y < c.size; y++ {
		for x := 0; x < c.size; x++ {
			if c.isFunction(x, y) {
				continue
			}
			if maskCondition(mask, x, y) {
				c.modules[y*c.size+x] = !c.modules[y*c.size+x]
			}
		}
	}
}

// applyBestMask applies the requested mask, or evaluates all eight patterns
// and keeps the one with the lowest penalty score when mask is negative.
func (c *Code) applyBestMask(mask int) {
	if mask < 0 {
		best, bestScore := 0, -1
		for m := 0; m < 8; m++ {
			c.applyMask(m)
			c.drawFormatBits(m)
			if score := c.penaltyScore(); bestScore < 0 || score < bestScore {
				best, bestScore = m, score
			}
			c.applyMask(m) // undo
		}
		mask = best
	}
	c.applyMask(mask)
	c.drawFormatBits(mask)
	c.mask = mask
}

// penaltyScore implements the four mask evaluation rules of ISO/IEC 18004
// section 7.8.3.
func (c *Code) penaltyScore() int {
	const (
		penaltyN1 = 3
		penaltyN2 = 3
		penaltyN3 = 40
		penaltyN4 = 10
	)
	size := c.size
	score := 0

	// Rule 1 and rule 3, scanned horizontally.
	for y := 0; y < size; y++ {
		runColor := false
		runLen := 0
		var history [7]int
		for x := 0; x < size; x++ {
			if dark := c.Module(x, y); dark == runColor {
				runLen++
				if runLen == 5 {
					score += penaltyN1
				} else if runLen > 5 {
					score++
				}
			} else {
				c.finderPenaltyAddHistory(runLen, &history, size)
				if !runColor {
					score += c.finderPenaltyCountPatterns(&history) * penaltyN3
				}
				runColor = dark
				runLen = 1
			}
		}
		score += c.finderPenaltyTerminateAndCount(runColor, runLen, &history, size) * penaltyN3
	}
	// Rule 1 and rule 3, scanned vertically.
	for x := 0; x < size; x++ {
		runColor := false
		runLen := 0
		var history [7]int
		for y := 0; y < size; y++ {
			if dark := c.Module(x, y); dark == runColor {
				runLen++
				if runLen == 5 {
					score += penaltyN1
				} else if runLen > 5 {
					score++
				}
			} else {
				c.finderPenaltyAddHistory(runLen, &history, size)
				if !runColor {
					score += c.finderPenaltyCountPatterns(&history) * penaltyN3
				}
				runColor = dark
				runLen = 1
			}
		}
		score += c.finderPenaltyTerminateAndCount(runColor, runLen, &history, size) * penaltyN3
	}

	// Rule 2: 2x2 blocks of the same colour.
	for y := 0; y < size-1; y++ {
		for x := 0; x < size-1; x++ {
			m := c.Module(x, y)
			if m == c.Module(x+1, y) && m == c.Module(x, y+1) && m == c.Module(x+1, y+1) {
				score += penaltyN2
			}
		}
	}

	// Rule 4: deviation of the dark module ratio from 50%.
	dark := 0
	for _, m := range c.modules {
		if m {
			dark++
		}
	}
	total := size * size
	// k is the smallest integer with dark/total outside [45-5k, 55+5k] percent.
	k := (abs(dark*20-total*10)+total-1)/total - 1
	score += k * penaltyN4
	return score
}

// finderPenaltyAddHistory pushes a run length onto the sliding window used to
// detect the 1:1:3:1:1 finder-like patterns of rule 3.
func (c *Code) finderPenaltyAddHistory(runLen int, history *[7]int, size int) {
	if history[0] == 0 {
		runLen += size // add the implicit light border before the first run
	}
	copy(history[1:], history[:6])
	history[0] = runLen
}

// finderPenaltyCountPatterns counts the finder-like patterns in the window.
func (c *Code) finderPenaltyCountPatterns(history *[7]int) int {
	n := history[1]
	core := n > 0 && history[2] == n && history[3] == n*3 && history[4] == n && history[5] == n
	count := 0
	if core && history[0] >= n*4 && history[6] >= n {
		count++
	}
	if core && history[6] >= n*4 && history[0] >= n {
		count++
	}
	return count
}

// finderPenaltyTerminateAndCount closes the current run and counts any finder
// like pattern that ends at the border of the symbol.
func (c *Code) finderPenaltyTerminateAndCount(currentDark bool, currentRunLen int, history *[7]int, size int) int {
	if currentDark {
		c.finderPenaltyAddHistory(currentRunLen, history, size)
		currentRunLen = 0
	}
	currentRunLen += size // add the implicit light border after the last run
	c.finderPenaltyAddHistory(currentRunLen, history, size)
	return c.finderPenaltyCountPatterns(history)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
