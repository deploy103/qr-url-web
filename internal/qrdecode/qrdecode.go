// Package qrdecode implements a standalone QR Code reader.
//
// It exists so that the symbols produced by the internal/qr encoder can be
// verified end to end: a rendered PNG or SVG is turned back into a module
// grid, the format information is read and error corrected, the data mask is
// removed, the codewords are de-interleaved, Reed-Solomon corrected and finally
// parsed back into the original payload. The package intentionally duplicates
// the specification tables rather than importing them from the encoder, so a
// successful round trip is a meaningful check.
package qrdecode

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/encoding/japanese"
)

// Result is a decoded QR Code symbol.
type Result struct {
	Text        string
	Version     int
	Level       string
	Mask        int
	Corrections int
}

// Grid is a square matrix of QR Code modules without the quiet zone.
type Grid struct {
	size int
	dark []bool
}

// NewGrid allocates an all-light grid of the given module size.
func NewGrid(size int) *Grid {
	return &Grid{size: size, dark: make([]bool, size*size)}
}

// Size returns the side length of the grid in modules.
func (g *Grid) Size() int { return g.size }

// Set marks the module at column x, row y.
func (g *Grid) Set(x, y int, dark bool) { g.dark[y*g.size+x] = dark }

// Get reports whether the module at column x, row y is dark.
func (g *Grid) Get(x, y int) bool {
	if x < 0 || y < 0 || x >= g.size || y >= g.size {
		return false
	}
	return g.dark[y*g.size+x]
}

// Specification tables, kept independent from the encoder package.
var eccPerBlock = [4][41]int{
	{-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
	{-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	{-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
}

var blockCount = [4][41]int{
	{-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},
	{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},
	{-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68},
	{-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81},
}

// levelFromBits maps the two format information bits onto a level index and
// name. The bit patterns are 01=L, 00=M, 11=Q, 10=H.
var levelFromBits = [4]struct {
	index int
	name  string
}{
	{1, "M"}, {0, "L"}, {3, "H"}, {2, "Q"},
}

func rawDataModules(version int) int {
	size := version*4 + 17
	result := size * size
	result -= 8 * 8 * 3
	result -= 15*2 + 1
	result -= (size - 16) * 2
	if version >= 2 {
		numAlign := version/7 + 2
		result -= (numAlign - 1) * (numAlign - 1) * 25
		result -= (numAlign - 2) * 2 * 20
		if version >= 7 {
			result -= 6 * 3 * 2
		}
	}
	return result
}

func totalCodewords(version int) int { return rawDataModules(version) / 8 }

func alignmentPositions(version int) []int {
	if version == 1 {
		return nil
	}
	numAlign := version/7 + 2
	var step int
	if version == 32 {
		step = 26
	} else {
		step = (version*4 + numAlign*2 + 1) / (numAlign*2 - 2) * 2
	}
	size := version*4 + 17
	tail := make([]int, 0, numAlign)
	for pos := size - 7; len(tail) < numAlign-1; pos -= step {
		tail = append(tail, pos)
	}
	out := []int{6}
	for i := len(tail) - 1; i >= 0; i-- {
		out = append(out, tail[i])
	}
	return out
}

// functionMap marks every module that belongs to a function pattern, the
// format information or the version information.
func functionMap(version int) []bool {
	size := version*4 + 17
	fn := make([]bool, size*size)
	mark := func(x, y int) {
		if x >= 0 && y >= 0 && x < size && y < size {
			fn[y*size+x] = true
		}
	}
	// Finder patterns, separators, format information and the dark module.
	// Top left is a 9x9 corner, top right is 8 columns by 9 rows and bottom
	// left is 9 columns by 8 rows.
	for y := 0; y <= 8; y++ {
		for x := 0; x <= 8; x++ {
			mark(x, y)
		}
		for x := size - 8; x < size; x++ {
			mark(x, y)
		}
	}
	for y := size - 8; y < size; y++ {
		for x := 0; x <= 8; x++ {
			mark(x, y)
		}
	}
	// Timing patterns.
	for i := 0; i < size; i++ {
		mark(6, i)
		mark(i, 6)
	}
	// Alignment patterns.
	positions := alignmentPositions(version)
	last := len(positions) - 1
	for i, cy := range positions {
		for j, cx := range positions {
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					mark(cx+dx, cy+dy)
				}
			}
		}
	}
	// Version information.
	if version >= 7 {
		for i := 0; i < 18; i++ {
			a := size - 11 + i%3
			b := i / 3
			mark(a, b)
			mark(b, a)
		}
	}
	return fn
}

func formatInfoWord(levelBits, mask int) uint32 {
	data := uint32(levelBits)<<3 | uint32(mask)
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	return (data<<10 | rem) ^ 0x5412
}

func versionInfoWord(version int) uint32 {
	rem := uint32(version)
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	return uint32(version)<<12 | rem
}

func popcount(v uint32) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
}

// readFormatInfo recovers the error correction level and mask pattern from the
// two redundant copies of the format information.
func readFormatInfo(g *Grid) (levelBits, mask int, err error) {
	size := g.size
	bit := func(dark bool, i int) uint32 {
		if dark {
			return 1 << uint(i)
		}
		return 0
	}

	var copy1, copy2 uint32
	for i := 0; i <= 5; i++ {
		copy1 |= bit(g.Get(8, i), i)
	}
	copy1 |= bit(g.Get(8, 7), 6)
	copy1 |= bit(g.Get(8, 8), 7)
	copy1 |= bit(g.Get(7, 8), 8)
	for i := 9; i < 15; i++ {
		copy1 |= bit(g.Get(14-i, 8), i)
	}
	for i := 0; i < 8; i++ {
		copy2 |= bit(g.Get(size-1-i, 8), i)
	}
	for i := 8; i < 15; i++ {
		copy2 |= bit(g.Get(8, size-15+i), i)
	}

	bestDist := 99
	for lb := 0; lb < 4; lb++ {
		for m := 0; m < 8; m++ {
			word := formatInfoWord(lb, m)
			for _, got := range [2]uint32{copy1, copy2} {
				if d := popcount(word ^ got); d < bestDist {
					bestDist, levelBits, mask = d, lb, m
				}
			}
		}
	}
	if bestDist > 3 {
		return 0, 0, errors.New("qrdecode: format information is unreadable")
	}
	return levelBits, mask, nil
}

// verifyVersionInfo checks the redundant version information of version 7 and
// above against the version implied by the grid size.
func verifyVersionInfo(g *Grid, version int) error {
	if version < 7 {
		return nil
	}
	size := g.size
	var word uint32
	for i := 0; i < 18; i++ {
		a := size - 11 + i%3
		b := i / 3
		if g.Get(a, b) {
			word |= 1 << uint(i)
		}
	}
	best, bestDist := 0, 99
	for v := 7; v <= 40; v++ {
		if d := popcount(versionInfoWord(v) ^ word); d < bestDist {
			best, bestDist = v, d
		}
	}
	if bestDist > 3 || best != version {
		return fmt.Errorf("qrdecode: version information mismatch (grid says %d, symbol says %d)", version, best)
	}
	return nil
}

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
	return false
}

// readCodewords walks the zigzag placement order and returns the interleaved
// codeword stream with the data mask removed.
func readCodewords(g *Grid, version, mask int, fn []bool) []byte {
	size := g.size
	total := totalCodewords(version)
	out := make([]byte, 0, total)
	var cur byte
	nbits := 0

	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if fn[y*size+x] {
					continue
				}
				if len(out) == total {
					continue
				}
				dark := g.Get(x, y)
				if maskCondition(mask, x, y) {
					dark = !dark
				}
				cur <<= 1
				if dark {
					cur |= 1
				}
				nbits++
				if nbits == 8 {
					out = append(out, cur)
					cur, nbits = 0, 0
				}
			}
		}
	}
	return out
}

// deinterleave undoes the block interleaving and Reed-Solomon corrects every
// block, returning the concatenated data codewords.
func deinterleave(stream []byte, version, levelIndex int) ([]byte, int, error) {
	numBlocks := blockCount[levelIndex][version]
	eccLen := eccPerBlock[levelIndex][version]
	total := totalCodewords(version)
	if len(stream) != total {
		return nil, 0, fmt.Errorf("qrdecode: expected %d codewords, read %d", total, len(stream))
	}
	shortLen := total/numBlocks - eccLen
	numShort := numBlocks - total%numBlocks

	dataParts := make([][]byte, numBlocks)
	eccParts := make([][]byte, numBlocks)
	for i := 0; i < numBlocks; i++ {
		n := shortLen
		if i >= numShort {
			n++
		}
		dataParts[i] = make([]byte, 0, n)
		eccParts[i] = make([]byte, 0, eccLen)
	}

	pos := 0
	maxData := shortLen
	if numShort < numBlocks {
		maxData++
	}
	for i := 0; i < maxData; i++ {
		for j := 0; j < numBlocks; j++ {
			if i < shortLen || j >= numShort {
				dataParts[j] = append(dataParts[j], stream[pos])
				pos++
			}
		}
	}
	for i := 0; i < eccLen; i++ {
		for j := 0; j < numBlocks; j++ {
			eccParts[j] = append(eccParts[j], stream[pos])
			pos++
		}
	}

	var data []byte
	corrections := 0
	for i := 0; i < numBlocks; i++ {
		block := append(append([]byte{}, dataParts[i]...), eccParts[i]...)
		fixed, n, err := rsDecode(block, eccLen)
		if err != nil {
			return nil, 0, fmt.Errorf("qrdecode: block %d: %w", i, err)
		}
		corrections += n
		data = append(data, fixed[:len(dataParts[i])]...)
	}
	return data, corrections, nil
}

const alphanumericCharset = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

type bitReader struct {
	data []byte
	pos  int
}

func (r *bitReader) remaining() int { return len(r.data)*8 - r.pos }

func (r *bitReader) read(n int) (int, error) {
	if n > r.remaining() {
		return 0, errors.New("qrdecode: unexpected end of data")
	}
	v := 0
	for i := 0; i < n; i++ {
		bit := (r.data[r.pos>>3] >> uint(7-r.pos&7)) & 1
		v = v<<1 | int(bit)
		r.pos++
	}
	return v, nil
}

func countBits(modeIndicator, version int) int {
	var idx int
	switch {
	case version <= 9:
		idx = 0
	case version <= 26:
		idx = 1
	default:
		idx = 2
	}
	switch modeIndicator {
	case 1:
		return [3]int{10, 12, 14}[idx]
	case 2:
		return [3]int{9, 11, 13}[idx]
	case 4:
		return [3]int{8, 16, 16}[idx]
	case 8:
		return [3]int{8, 10, 12}[idx]
	}
	return 0
}

// parseSegments turns the data codewords back into the encoded string.
func parseSegments(data []byte, version int) (string, error) {
	r := &bitReader{data: data}
	var sb strings.Builder

	for r.remaining() >= 4 {
		modeIndicator, err := r.read(4)
		if err != nil {
			return "", err
		}
		if modeIndicator == 0 { // terminator
			break
		}
		if modeIndicator == 7 { // ECI designator, skipped
			first, err := r.read(8)
			if err != nil {
				return "", err
			}
			switch {
			case first&0x80 == 0: // 1 byte
			case first&0xC0 == 0x80: // 2 bytes
				if _, err := r.read(8); err != nil {
					return "", err
				}
			case first&0xE0 == 0xC0: // 3 bytes
				if _, err := r.read(16); err != nil {
					return "", err
				}
			default:
				return "", errors.New("qrdecode: malformed ECI designator")
			}
			continue
		}

		nbits := countBits(modeIndicator, version)
		if nbits == 0 {
			return "", fmt.Errorf("qrdecode: unsupported mode indicator %d", modeIndicator)
		}
		count, err := r.read(nbits)
		if err != nil {
			return "", err
		}

		switch modeIndicator {
		case 1: // numeric
			for i := 0; i < count/3; i++ {
				v, err := r.read(10)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&sb, "%03d", v)
			}
			switch count % 3 {
			case 1:
				v, err := r.read(4)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&sb, "%d", v)
			case 2:
				v, err := r.read(7)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&sb, "%02d", v)
			}
		case 2: // alphanumeric
			for i := 0; i < count/2; i++ {
				v, err := r.read(11)
				if err != nil {
					return "", err
				}
				sb.WriteByte(alphanumericCharset[v/45])
				sb.WriteByte(alphanumericCharset[v%45])
			}
			if count%2 == 1 {
				v, err := r.read(6)
				if err != nil {
					return "", err
				}
				sb.WriteByte(alphanumericCharset[v])
			}
		case 4: // byte
			for i := 0; i < count; i++ {
				v, err := r.read(8)
				if err != nil {
					return "", err
				}
				sb.WriteByte(byte(v))
			}
		case 8: // kanji, 13 bits per Shift-JIS double byte character
			raw := make([]byte, 0, count*2)
			for i := 0; i < count; i++ {
				v, err := r.read(13)
				if err != nil {
					return "", err
				}
				value := (v/0xC0)<<8 | (v % 0xC0)
				if value+0x8140 <= 0x9FFC {
					value += 0x8140
				} else {
					value += 0xC140
				}
				raw = append(raw, byte(value>>8), byte(value&0xFF))
			}
			decoded, err := japanese.ShiftJIS.NewDecoder().Bytes(raw)
			if err != nil {
				return "", fmt.Errorf("qrdecode: kanji segment: %w", err)
			}
			sb.Write(decoded)
		default:
			return "", fmt.Errorf("qrdecode: unsupported mode indicator %d", modeIndicator)
		}
	}
	return sb.String(), nil
}

// DecodeGrid reads a QR Code from a module grid.
func DecodeGrid(g *Grid) (*Result, error) {
	if g == nil {
		return nil, errors.New("qrdecode: grid is nil")
	}
	if g.size < 21 || g.size > 177 || (g.size-17)%4 != 0 {
		return nil, fmt.Errorf("qrdecode: invalid grid size %d", g.size)
	}
	version := (g.size - 17) / 4
	if err := verifyVersionInfo(g, version); err != nil {
		return nil, err
	}
	levelBits, mask, err := readFormatInfo(g)
	if err != nil {
		return nil, err
	}
	level := levelFromBits[levelBits]

	fn := functionMap(version)
	stream := readCodewords(g, version, mask, fn)
	data, corrections, err := deinterleave(stream, version, level.index)
	if err != nil {
		return nil, err
	}
	text, err := parseSegments(data, version)
	if err != nil {
		return nil, err
	}
	return &Result{
		Text:        text,
		Version:     version,
		Level:       level.name,
		Mask:        mask,
		Corrections: corrections,
	}, nil
}
