package qr

import (
	"errors"
	"fmt"
	"strings"
)

// ErrTooLong is returned when the payload does not fit into a version 40
// symbol at the requested error correction level.
var ErrTooLong = errors.New("qr: payload does not fit in a version 40 symbol")

// mode describes one of the QR Code data encoding modes.
type mode struct {
	name      string
	indicator uint32
	countBits [3]int // versions 1-9, 10-26, 27-40
}

var (
	modeNumeric      = mode{name: "numeric", indicator: 0x1, countBits: [3]int{10, 12, 14}}
	modeAlphanumeric = mode{name: "alphanumeric", indicator: 0x2, countBits: [3]int{9, 11, 13}}
	modeByte         = mode{name: "byte", indicator: 0x4, countBits: [3]int{8, 16, 16}}
	modeKanji        = mode{name: "kanji", indicator: 0x8, countBits: [3]int{8, 10, 12}}
)

// alphanumericCharset is the 45 character set of the alphanumeric mode. The
// index of a character is its encoded value.
const alphanumericCharset = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

// bitBuffer accumulates a big-endian stream of bits.
type bitBuffer struct {
	bits []bool
}

func (b *bitBuffer) appendBits(value uint32, n int) {
	if n < 0 || n > 32 || (n < 32 && value>>uint(n) != 0) {
		panic("qr: value does not fit in the requested number of bits")
	}
	for i := n - 1; i >= 0; i-- {
		b.bits = append(b.bits, (value>>uint(i))&1 == 1)
	}
}

func (b *bitBuffer) len() int { return len(b.bits) }

// bytes packs the buffer into codewords. The buffer length must be a multiple
// of eight.
func (b *bitBuffer) bytes() []byte {
	out := make([]byte, len(b.bits)/8)
	for i, bit := range b.bits {
		if bit {
			out[i>>3] |= 1 << uint(7-i&7)
		}
	}
	return out
}

// isNumeric reports whether every character of s is an ASCII digit.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isAlphanumeric reports whether every character of s belongs to the QR Code
// alphanumeric character set.
func isAlphanumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(alphanumericCharset, s[i]) < 0 {
			return false
		}
	}
	return true
}

// selectMode picks the most compact single-segment mode able to represent
// text. Kanji mode is only worth using when every character fits it, in which
// case it stores 13 bits per character instead of the 24 that UTF-8 byte mode
// would need for the same glyph.
func selectMode(text string) mode {
	switch {
	case isNumeric(text):
		return modeNumeric
	case isAlphanumeric(text):
		return modeAlphanumeric
	default:
		if _, ok := shiftJISKanji(text); ok {
			return modeKanji
		}
		return modeByte
	}
}

// payloadBits returns the number of bits the raw payload occupies in the given
// mode, excluding the mode indicator and the character count indicator.
func payloadBits(m mode, text string) int {
	switch m.indicator {
	case modeKanji.indicator:
		chars, _ := shiftJISKanji(text)
		return len(chars) * 13
	case modeNumeric.indicator:
		n := len(text)
		bits := n / 3 * 10
		switch n % 3 {
		case 1:
			bits += 4
		case 2:
			bits += 7
		}
		return bits
	case modeAlphanumeric.indicator:
		n := len(text)
		return n/2*11 + n%2*6
	default:
		return len(text) * 8
	}
}

// charCount returns the value written into the character count indicator. For
// Kanji mode the unit is the double byte character, not the byte.
func charCount(m mode, text string) int {
	if m.indicator == modeKanji.indicator {
		chars, _ := shiftJISKanji(text)
		return len(chars)
	}
	return len(text)
}

// appendSegment writes the mode indicator, character count indicator and the
// encoded payload of text into buf.
func appendSegment(buf *bitBuffer, m mode, text string, version int) error {
	countBits := charCountBits(m, version)
	count := charCount(m, text)
	if count >= 1<<uint(countBits) {
		return fmt.Errorf("qr: %d characters exceed the %s mode count indicator of version %d", count, m.name, version)
	}
	buf.appendBits(m.indicator, 4)
	buf.appendBits(uint32(count), countBits)

	switch m.indicator {
	case modeKanji.indicator:
		chars, ok := shiftJISKanji(text)
		if !ok {
			return fmt.Errorf("qr: payload is not representable in kanji mode")
		}
		for _, c := range chars {
			buf.appendBits(kanjiValue(c), 13)
		}
	case modeNumeric.indicator:
		for i := 0; i < len(text); i += 3 {
			end := min(i+3, len(text))
			chunk := text[i:end]
			var value uint32
			for j := 0; j < len(chunk); j++ {
				value = value*10 + uint32(chunk[j]-'0')
			}
			buf.appendBits(value, len(chunk)*3+1)
		}
	case modeAlphanumeric.indicator:
		for i := 0; i+1 < len(text); i += 2 {
			v1 := uint32(strings.IndexByte(alphanumericCharset, text[i]))
			v2 := uint32(strings.IndexByte(alphanumericCharset, text[i+1]))
			buf.appendBits(v1*45+v2, 11)
		}
		if len(text)%2 == 1 {
			v := uint32(strings.IndexByte(alphanumericCharset, text[len(text)-1]))
			buf.appendBits(v, 6)
		}
	default:
		for i := 0; i < len(text); i++ {
			buf.appendBits(uint32(text[i]), 8)
		}
	}
	return nil
}

// chooseVersion returns the smallest version in [minVersion, MaxVersion] able
// to hold text at the given error correction level.
func chooseVersion(m mode, text string, level Level, minVersion int) (int, error) {
	for version := max(minVersion, MinVersion); version <= MaxVersion; version++ {
		capacityBits := numDataCodewords(version, level) * 8
		needed := 4 + charCountBits(m, version) + payloadBits(m, text)
		if needed <= capacityBits && charCount(m, text) < 1<<uint(charCountBits(m, version)) {
			return version, nil
		}
	}
	return 0, ErrTooLong
}

// buildCodewords produces the final, interleaved codeword sequence (data plus
// error correction) that is written into the symbol matrix.
func buildCodewords(text string, level Level, version int, m mode) ([]byte, error) {
	buf := &bitBuffer{}
	if err := appendSegment(buf, m, text, version); err != nil {
		return nil, err
	}

	capacityBits := numDataCodewords(version, level) * 8
	if buf.len() > capacityBits {
		return nil, ErrTooLong
	}

	// Terminator: up to four zero bits.
	buf.appendBits(0, min(4, capacityBits-buf.len()))
	// Pad to a codeword boundary.
	buf.appendBits(0, (8-buf.len()%8)%8)
	// Fill the remaining capacity with the alternating pad codewords.
	for pad := uint32(0xEC); buf.len() < capacityBits; pad ^= 0xEC ^ 0x11 {
		buf.appendBits(pad, 8)
	}

	return interleave(buf.bytes(), level, version), nil
}

// interleave splits the data codewords into blocks, appends the Reed-Solomon
// error correction codewords of every block and interleaves the result as
// specified by ISO/IEC 18004 section 7.6.
func interleave(data []byte, level Level, version int) []byte {
	numBlocks := numEccBlocks[level][version]
	eccLen := eccCodewordsPerBlock[level][version]
	rawCodewords := numCodewords(version)
	// The data codewords are spread as evenly as possible over the blocks: the
	// first numShortBlocks blocks hold shortBlockLen codewords, the remaining
	// ones hold a single codeword more.
	shortBlockLen := rawCodewords/numBlocks - eccLen
	numShortBlocks := numBlocks - rawCodewords%numBlocks

	generator := rsGeneratorPoly(eccLen)

	blocks := make([][]byte, numBlocks)
	offset := 0
	for i := 0; i < numBlocks; i++ {
		dataLen := shortBlockLen
		if i >= numShortBlocks {
			dataLen++
		}
		dat := data[offset : offset+dataLen]
		offset += dataLen
		ecc := rsRemainder(dat, generator)
		block := make([]byte, 0, dataLen+eccLen)
		block = append(block, dat...)
		block = append(block, ecc...)
		blocks[i] = block
	}

	result := make([]byte, 0, rawCodewords)
	maxDataLen := shortBlockLen
	if numShortBlocks < numBlocks {
		maxDataLen++
	}
	for i := 0; i < maxDataLen; i++ {
		for j, block := range blocks {
			// The short blocks have no codeword at the last data index.
			if i < shortBlockLen || j >= numShortBlocks {
				result = append(result, block[i])
			}
		}
	}
	for i := 0; i < eccLen; i++ {
		for _, block := range blocks {
			result = append(result, block[len(block)-eccLen+i])
		}
	}
	return result
}
