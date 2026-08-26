package qrdecode

import "errors"

// Reed-Solomon decoding over GF(2^8) with the QR Code primitive polynomial
// 0x11D. The implementation is deliberately written from the decoder side only
// (syndromes, Berlekamp-Massey, Chien search and Forney magnitudes) so that it
// shares no code with the encoder and can act as an independent check.

var (
	gfExp [512]byte
	gfLog [256]byte
)

func init() {
	x := byte(1)
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = byte(i)
		// x = x * 2 in GF(2^8)
		if x&0x80 != 0 {
			x = x<<1 ^ 0x1D
		} else {
			x <<= 1
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func gfDiv(a, b byte) byte {
	if b == 0 {
		panic("qrdecode: division by zero")
	}
	if a == 0 {
		return 0
	}
	return gfExp[(int(gfLog[a])+255-int(gfLog[b]))%255]
}

func gfInv(a byte) byte { return gfExp[255-int(gfLog[a])] }

// gfPow returns x^power, accepting negative exponents.
func gfPow(x byte, power int) byte {
	if x == 0 {
		if power == 0 {
			return 1
		}
		return 0
	}
	e := (int(gfLog[x]) * power) % 255
	if e < 0 {
		e += 255
	}
	return gfExp[e]
}

// Polynomials are stored with the highest degree coefficient first.

func polyEval(p []byte, x byte) byte {
	y := p[0]
	for i := 1; i < len(p); i++ {
		y = gfMul(y, x) ^ p[i]
	}
	return y
}

func polyScale(p []byte, s byte) []byte {
	r := make([]byte, len(p))
	for i, c := range p {
		r[i] = gfMul(c, s)
	}
	return r
}

func polyAdd(p, q []byte) []byte {
	n := max(len(p), len(q))
	r := make([]byte, n)
	for i, c := range p {
		r[i+n-len(p)] = c
	}
	for i, c := range q {
		r[i+n-len(q)] ^= c
	}
	return r
}

func polyMul(p, q []byte) []byte {
	r := make([]byte, len(p)+len(q)-1)
	for j, cq := range q {
		if cq == 0 {
			continue
		}
		for i, cp := range p {
			r[i+j] ^= gfMul(cp, cq)
		}
	}
	return r
}

func reversed(p []byte) []byte {
	r := make([]byte, len(p))
	for i, c := range p {
		r[len(p)-1-i] = c
	}
	return r
}

// ErrUncorrectable reports a codeword block that carries more errors than the
// error correction level can repair.
var ErrUncorrectable = errors.New("qrdecode: too many errors to correct")

// calcSyndromes returns [0, S0, S1, ... S(nsym-1)] with Si = msg(alpha^i).
func calcSyndromes(msg []byte, nsym int) []byte {
	synd := make([]byte, nsym+1)
	for i := 0; i < nsym; i++ {
		synd[i+1] = polyEval(msg, gfPow(2, i))
	}
	return synd
}

func syndromesAllZero(synd []byte) bool {
	for _, s := range synd {
		if s != 0 {
			return false
		}
	}
	return true
}

// findErrorLocator runs the Berlekamp-Massey algorithm.
func findErrorLocator(synd []byte, nsym int) ([]byte, error) {
	errLoc := []byte{1}
	oldLoc := []byte{1}
	syndShift := len(synd) - nsym

	for i := 0; i < nsym; i++ {
		k := i + syndShift
		delta := synd[k]
		for j := 1; j < len(errLoc); j++ {
			delta ^= gfMul(errLoc[len(errLoc)-1-j], synd[k-j])
		}
		oldLoc = append(oldLoc, 0)
		if delta != 0 {
			if len(oldLoc) > len(errLoc) {
				newLoc := polyScale(oldLoc, delta)
				oldLoc = polyScale(errLoc, gfInv(delta))
				errLoc = newLoc
			}
			errLoc = polyAdd(errLoc, polyScale(oldLoc, delta))
		}
	}
	for len(errLoc) > 0 && errLoc[0] == 0 {
		errLoc = errLoc[1:]
	}
	if errs := len(errLoc) - 1; errs*2 > nsym {
		return nil, ErrUncorrectable
	}
	return errLoc, nil
}

// findErrors runs a Chien search for the error positions.
func findErrors(errLoc []byte, msgLen int) ([]int, error) {
	errs := len(errLoc) - 1
	var positions []int
	for i := 0; i < msgLen; i++ {
		if polyEval(errLoc, gfPow(2, i)) == 0 {
			positions = append(positions, msgLen-1-i)
		}
	}
	if len(positions) != errs {
		return nil, ErrUncorrectable
	}
	return positions, nil
}

func findErrataLocator(positions []int) []byte {
	eLoc := []byte{1}
	for _, p := range positions {
		eLoc = polyMul(eLoc, polyAdd([]byte{1}, []byte{gfPow(2, p), 0}))
	}
	return eLoc
}

func findErrorEvaluator(synd, errLoc []byte, nsym int) []byte {
	remainder := polyMul(synd, errLoc)
	return remainder[len(remainder)-(nsym+1):]
}

// correctErrata applies the Forney algorithm and returns the repaired message.
func correctErrata(msg, synd []byte, errPos []int) ([]byte, error) {
	coefPos := make([]int, len(errPos))
	for i, p := range errPos {
		coefPos[i] = len(msg) - 1 - p
	}
	errLoc := findErrataLocator(coefPos)
	errEval := reversed(findErrorEvaluator(reversed(synd), errLoc, len(errLoc)-1))

	xs := make([]byte, len(coefPos))
	for i, p := range coefPos {
		xs[i] = gfPow(2, p-255)
	}

	e := make([]byte, len(msg))
	for i, xi := range xs {
		xiInv := gfInv(xi)
		locPrime := byte(1)
		for j, xj := range xs {
			if j != i {
				locPrime = gfMul(locPrime, 1^gfMul(xiInv, xj))
			}
		}
		if locPrime == 0 {
			return nil, ErrUncorrectable
		}
		y := polyEval(reversed(errEval), xiInv)
		y = gfMul(gfPow(xi, 1), y)
		e[errPos[i]] = gfDiv(y, locPrime)
	}
	return polyAdd(msg, e), nil
}

// rsDecode repairs a codeword block that carries nsym error correction
// codewords and returns the corrected block plus the number of repaired bytes.
func rsDecode(msg []byte, nsym int) ([]byte, int, error) {
	synd := calcSyndromes(msg, nsym)
	if syndromesAllZero(synd) {
		return msg, 0, nil
	}
	errLoc, err := findErrorLocator(synd, nsym)
	if err != nil {
		return nil, 0, err
	}
	positions, err := findErrors(reversed(errLoc), len(msg))
	if err != nil {
		return nil, 0, err
	}
	fixed, err := correctErrata(msg, synd, positions)
	if err != nil {
		return nil, 0, err
	}
	if !syndromesAllZero(calcSyndromes(fixed, nsym)) {
		return nil, 0, ErrUncorrectable
	}
	return fixed, len(positions), nil
}
