package qr

// Test-only accessors for the internal specification tables, used by the
// cross-checks against an independent implementation.

// DebugTotalCodewords returns the total number of codewords of a version.
func DebugTotalCodewords(version int) int { return numCodewords(version) }

// DebugBlockLayout returns the error correction block layout of a symbol.
func DebugBlockLayout(version int, level Level) (ecPerBlock, blocks, data int) {
	return eccCodewordsPerBlock[level][version],
		numEccBlocks[level][version],
		numDataCodewords(version, level)
}

// DebugPenaltyScore returns the mask evaluation score of a finished symbol.
func (c *Code) DebugPenaltyScore() int { return c.penaltyScore() }
