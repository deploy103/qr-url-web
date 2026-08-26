package qr

// Arithmetic in GF(2^8) with the QR Code primitive polynomial
// x^8 + x^4 + x^3 + x^2 + 1 (0x11D).

const gfPrimitive = 0x11D

// gfMul multiplies two field elements using the Russian peasant algorithm.
func gfMul(a, b byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		// z = z*2 mod primitive
		z = byte(int(z)<<1 ^ (int(z)>>7)*gfPrimitive)
		z ^= byte((int(b)>>i)&1) * a
	}
	return z
}

// rsGeneratorPoly returns the coefficients (excluding the leading 1) of the
// Reed-Solomon generator polynomial of the given degree, i.e. the expansion of
// (x - a^0)(x - a^1) ... (x - a^(degree-1)) where a = 0x02.
func rsGeneratorPoly(degree int) []byte {
	if degree < 1 || degree > 255 {
		panic("qr: invalid generator polynomial degree")
	}
	result := make([]byte, degree)
	result[degree-1] = 1

	root := byte(1)
	for i := 0; i < degree; i++ {
		// Multiply the current product by (x - root).
		for j := 0; j < degree; j++ {
			result[j] = gfMul(result[j], root)
			if j+1 < degree {
				result[j] ^= result[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return result
}

// rsRemainder computes the Reed-Solomon error correction codewords for data.
func rsRemainder(data []byte, generator []byte) []byte {
	result := make([]byte, len(generator))
	for _, b := range data {
		factor := b ^ result[0]
		copy(result, result[1:])
		result[len(result)-1] = 0
		for i, g := range generator {
			result[i] ^= gfMul(g, factor)
		}
	}
	return result
}
