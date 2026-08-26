package qr

import (
	"fmt"
	"strings"
)

// Level is the QR Code error correction level defined by ISO/IEC 18004.
type Level int

// Error correction levels, ordered by increasing redundancy.
const (
	LevelL Level = iota // ~7% recovery
	LevelM              // ~15% recovery
	LevelQ              // ~25% recovery
	LevelH              // ~30% recovery
)

var levelNames = [4]string{"L", "M", "Q", "H"}

// formatBits holds the 2-bit level indicator used inside the format information.
var levelFormatBits = [4]uint32{1, 0, 3, 2}

func (l Level) String() string {
	if l < LevelL || l > LevelH {
		return "?"
	}
	return levelNames[l]
}

// Valid reports whether l is one of the four defined levels.
func (l Level) Valid() bool { return l >= LevelL && l <= LevelH }

// ParseLevel converts "L", "M", "Q" or "H" (case-insensitive) into a Level.
// An empty string defaults to LevelM.
func ParseLevel(s string) (Level, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "M":
		return LevelM, nil
	case "L":
		return LevelL, nil
	case "Q":
		return LevelQ, nil
	case "H":
		return LevelH, nil
	}
	return 0, fmt.Errorf("qr: unknown error correction level %q", s)
}
