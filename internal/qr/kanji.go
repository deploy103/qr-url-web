package qr

import (
	"golang.org/x/text/encoding/japanese"
)

// Kanji mode packs a Shift-JIS double byte character into 13 bits instead of
// the 16 that byte mode would need, which makes Japanese payloads noticeably
// smaller. ISO/IEC 18004 section 7.4.6 defines the two supported ranges.

// shiftJISKanji converts text to Shift-JIS and reports whether every character
// is a double byte value inside the two ranges Kanji mode can encode.
func shiftJISKanji(text string) ([]uint16, bool) {
	if text == "" {
		return nil, false
	}
	encoded, err := japanese.ShiftJIS.NewEncoder().Bytes([]byte(text))
	if err != nil || len(encoded)%2 != 0 {
		return nil, false
	}
	out := make([]uint16, 0, len(encoded)/2)
	for i := 0; i < len(encoded); i += 2 {
		v := uint16(encoded[i])<<8 | uint16(encoded[i+1])
		switch {
		case v >= 0x8140 && v <= 0x9FFC:
		case v >= 0xE040 && v <= 0xEBBF:
		default:
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// kanjiValue folds a Shift-JIS code point into the 13 bit value the standard
// stores in the symbol.
func kanjiValue(v uint16) uint32 {
	var offset uint32
	if v >= 0xE040 {
		offset = uint32(v) - 0xC140
	} else {
		offset = uint32(v) - 0x8140
	}
	return (offset>>8)*0xC0 + (offset & 0xFF)
}
