package qr

// MinVersion and MaxVersion bound the QR Code symbol versions this package
// supports. Version v produces a (4v+17) x (4v+17) module matrix.
const (
	MinVersion = 1
	MaxVersion = 40
)

// eccCodewordsPerBlock[level][version] is the number of error correction
// codewords contained in every block of the symbol.
// Table 13-22 of ISO/IEC 18004:2015.
var eccCodewordsPerBlock = [4][41]int{
	// 0,  1,  2,  3,  4,  5,  6,  7,  8,  9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40
	{-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},  // L
	{-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28}, // M
	{-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30}, // Q
	{-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30}, // H
}

// numEccBlocks[level][version] is the number of error correction blocks the
// data codewords are split into.
var numEccBlocks = [4][41]int{
	// 0, 1, 2, 3, 4, 5, 6, 7, 8, 9,10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40
	{-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},              // L
	{-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},     // M
	{-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68},  // Q
	{-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81}, // H
}

// numRawDataModules returns the number of modules of a symbol of the given
// version that can hold data or error correction codewords, i.e. every module
// that is not part of a function pattern, the format information or the
// version information.
func numRawDataModules(version int) int {
	if version < MinVersion || version > MaxVersion {
		panic("qr: version out of range")
	}
	size := version*4 + 17
	result := size * size
	result -= 8 * 8 * 3       // three 8x8 finder pattern + separator regions
	result -= 15*2 + 1        // two 15-bit format information areas + dark module
	result -= (size - 16) * 2 // horizontal and vertical timing patterns
	if version >= 2 {
		numAlign := version/7 + 2
		result -= (numAlign - 1) * (numAlign - 1) * 25 // alignment patterns clear of the finders
		result -= (numAlign - 2) * 2 * 20              // alignment patterns overlapping the timing patterns
		if version >= 7 {
			result -= 6 * 3 * 2 // two 6x3 version information blocks
		}
	}
	return result
}

// numCodewords returns the total number of 8-bit codewords (data plus error
// correction) held by a symbol of the given version.
func numCodewords(version int) int { return numRawDataModules(version) / 8 }

// numDataCodewords returns how many of the symbol's codewords carry payload
// data for the given version and error correction level.
func numDataCodewords(version int, level Level) int {
	return numCodewords(version) - eccCodewordsPerBlock[level][version]*numEccBlocks[level][version]
}

// ByteCapacity returns how many bytes a symbol of the given version and error
// correction level can hold in byte mode. It returns 0 for invalid arguments.
func ByteCapacity(version int, level Level) int {
	if version < MinVersion || version > MaxVersion || !level.Valid() {
		return 0
	}
	bits := numDataCodewords(version, level)*8 - 4 - charCountBits(modeByte, version)
	if bits < 0 {
		return 0
	}
	return bits / 8
}

// alignmentPatternPositions returns the centre coordinates of the alignment
// patterns for the given version, in ascending order. Version 1 has none.
func alignmentPatternPositions(version int) []int {
	if version == 1 {
		return nil
	}
	numAlign := version/7 + 2
	var step int
	if version == 32 {
		step = 26
	} else {
		// Round (4*version + 4) / (2*numAlign - 2) up to the next even number.
		step = (version*4 + numAlign*2 + 1) / (numAlign*2 - 2) * 2
	}
	size := version*4 + 17
	result := make([]int, 0, numAlign)
	for pos := size - 7; len(result) < numAlign-1; pos -= step {
		result = append(result, pos)
	}
	// Reverse and prepend the fixed first coordinate.
	out := make([]int, 0, numAlign)
	out = append(out, 6)
	for i := len(result) - 1; i >= 0; i-- {
		out = append(out, result[i])
	}
	return out
}

// charCountBits returns the width in bits of the character count indicator for
// the given mode and version.
func charCountBits(m mode, version int) int {
	switch {
	case version <= 9:
		return m.countBits[0]
	case version <= 26:
		return m.countBits[1]
	default:
		return m.countBits[2]
	}
}
