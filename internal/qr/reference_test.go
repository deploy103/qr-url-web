package qr_test

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"testing"

	"github.com/mvtp/qrurl/internal/qr"
)

// The fixtures in testdata were produced by segno 1.6.6, an unrelated
// implementation of ISO/IEC 18004. Comparing against them turns a possible
// transcription error in the specification tables of this package into a test
// failure instead of a subtly broken symbol.

type eccReference struct {
	TotalCodewords []int `json:"totalCodewords"`
	Levels         map[string][]struct {
		ECPerBlock     int `json:"ecPerBlock"`
		Blocks         int `json:"blocks"`
		TotalCodewords int `json:"totalCodewords"`
		DataCodewords  int `json:"dataCodewords"`
		ByteCapacity   int `json:"byteCapacity"`
	} `json:"levels"`
}

func loadECCReference(t *testing.T) eccReference {
	t.Helper()
	f, err := os.Open("testdata/ecc_reference.json")
	if err != nil {
		t.Fatalf("open reference: %v", err)
	}
	defer f.Close()
	var ref eccReference
	if err := json.NewDecoder(f).Decode(&ref); err != nil {
		t.Fatalf("decode reference: %v", err)
	}
	return ref
}

func TestCapacitiesMatchIndependentImplementation(t *testing.T) {
	ref := loadECCReference(t)
	levels := map[string]qr.Level{"L": qr.LevelL, "M": qr.LevelM, "Q": qr.LevelQ, "H": qr.LevelH}
	for name, level := range levels {
		rows := ref.Levels[name]
		if len(rows) != 40 {
			t.Fatalf("reference for level %s has %d rows", name, len(rows))
		}
		for i, want := range rows {
			version := i + 1
			if got := qr.ByteCapacity(version, level); got != want.ByteCapacity {
				t.Errorf("ByteCapacity(%d, %s) = %d, want %d", version, name, got, want.ByteCapacity)
			}
			if got := qr.DebugTotalCodewords(version); got != want.TotalCodewords {
				t.Errorf("total codewords for version %d = %d, want %d", version, got, want.TotalCodewords)
			}
			gotEC, gotBlocks, gotData := qr.DebugBlockLayout(version, level)
			if gotEC != want.ECPerBlock || gotBlocks != want.Blocks || gotData != want.DataCodewords {
				t.Errorf("block layout v%d/%s = (ec %d, blocks %d, data %d), want (ec %d, blocks %d, data %d)",
					version, name, gotEC, gotBlocks, gotData, want.ECPerBlock, want.Blocks, want.DataCodewords)
			}
		}
	}
}

type segnoCase struct {
	Content       string   `json:"content"`
	Level         string   `json:"level"`
	RequestedMask int      `json:"requestedMask"`
	Version       int      `json:"version"`
	Mask          int      `json:"mask"`
	Matrix        []string `json:"matrix"`
}

func loadSegnoVectors(t *testing.T) []segnoCase {
	t.Helper()
	f, err := os.Open("testdata/segno_vectors.json.gz")
	if err != nil {
		t.Fatalf("open vectors: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip vectors: %v", err)
	}
	defer zr.Close()
	var doc struct {
		Cases []segnoCase `json:"cases"`
	}
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no vectors in fixture")
	}
	return doc.Cases
}

// TestMatricesMatchIndependentImplementation compares every generated module
// against segno for a wide range of payloads, versions, levels and masks.
func TestMatricesMatchIndependentImplementation(t *testing.T) {
	cases := loadSegnoVectors(t)
	autoTotal, autoSameMask := 0, 0

	for i, c := range cases {
		level, err := qr.ParseLevel(c.Level)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		code, err := qr.EncodeWithOptions(c.Content, qr.Options{
			Level:      level,
			MinVersion: c.Version,
			Mask:       c.RequestedMask,
		})
		if err != nil {
			t.Fatalf("case %d (v%d/%s): Encode: %v", i, c.Version, c.Level, err)
		}
		if code.Version() != c.Version {
			t.Fatalf("case %d: version = %d, want %d", i, code.Version(), c.Version)
		}
		if c.RequestedMask < 0 {
			autoTotal++
			if code.Mask() == c.Mask {
				autoSameMask++
			}
			// A different mask is still a valid symbol, so only compare the
			// modules when both sides settled on the same mask.
			if code.Mask() != c.Mask {
				continue
			}
		} else if code.Mask() != c.Mask {
			t.Fatalf("case %d: mask = %d, want %d", i, code.Mask(), c.Mask)
		}

		if len(c.Matrix) != code.Size() {
			t.Fatalf("case %d: matrix has %d rows, symbol size is %d", i, len(c.Matrix), code.Size())
		}
		for y, row := range c.Matrix {
			if len(row) != code.Size() {
				t.Fatalf("case %d: matrix row %d has length %d", i, y, len(row))
			}
			for x := 0; x < len(row); x++ {
				want := row[x] == '1'
				if got := code.Module(x, y); got != want {
					t.Fatalf("case %d (v%d/%s mask %d): module (%d,%d) = %v, want %v",
						i, c.Version, c.Level, code.Mask(), x, y, got, want)
				}
			}
		}
	}

	// Implementations legitimately differ on whether the format information is
	// already in place while the mask penalty is scored: this package and
	// ZXing include it, segno explicitly does not. Every mask yields a valid
	// symbol, so a disagreement here is not a defect; the number is reported
	// only to make an accidental regression visible.
	t.Logf("automatic mask selection agreed with the reference on %d of %d cases", autoSameMask, autoTotal)
	if autoSameMask == 0 {
		t.Error("automatic mask selection never agreed with the reference, which suggests a scoring bug")
	}
}

// TestAutomaticMaskIsOptimalUnderOurRule settles the one place where this
// package and segno disagree. Implementations differ on whether the format
// information is already in place while the mask penalty is scored; this
// package scores the complete symbol, as ZXing does. That is a choice, but it
// must at least be applied correctly: the mask picked automatically has to be
// the lowest scoring of the eight under that rule, for every vector.
func TestAutomaticMaskIsOptimalUnderOurRule(t *testing.T) {
	cases := loadSegnoVectors(t)
	checked := 0

	for i, c := range cases {
		if c.RequestedMask >= 0 {
			continue // only the automatic choices are interesting here
		}
		level, err := qr.ParseLevel(c.Level)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		auto, err := qr.EncodeWithOptions(c.Content, qr.Options{
			Level: level, MinVersion: c.Version, Mask: -1,
		})
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		best := auto.DebugPenaltyScore()

		for m := 0; m < 8; m++ {
			forced, err := qr.EncodeWithOptions(c.Content, qr.Options{
				Level: level, MinVersion: c.Version, Mask: m,
			})
			if err != nil {
				t.Fatalf("case %d mask %d: %v", i, m, err)
			}
			if score := forced.DebugPenaltyScore(); score < best {
				t.Fatalf("case %d (v%d/%s): mask %d scores %d but the automatic choice %d scores %d",
					i, c.Version, c.Level, m, score, auto.Mask(), best)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no automatic mask vectors were checked")
	}
	t.Logf("verified the automatic mask is the lowest scoring of the eight on %d symbols", checked)
}
