package qrstream

// Luby Transform core for the fountain mode (FORMAT.md, flags bit 0).
//
// Derived from github.com/google/gofountain (Apache License 2.0,
// Copyright 2014 Google Inc.): MT19937, soliton CDF, degree and index
// sampling, and the triangular sparse-matrix decoder; protocol use
// informed by the public protocol design in github.com/divan/txqr.
// No txqr source code is included. The gofountain code is copied
// rather than imported because its repository is archived. Reduced
// for qrstream:
// every block has the same fixed length (the symbol chunk size), so
// the variable-length padding and RFC 5053 partitioning bookkeeping
// of the original are gone. The applicable Apache license is in
// LICENSE-APACHE.

import (
	"math"
	"math/rand"
	"sort"
)

// mersenneTwister is the MT19937 PRNG of Matsumoto and Nishimura
// (32-bit variant), the PRNG fixed by the wire format for deriving
// block compositions from seeds. Satisfies math/rand.Source.
type mersenneTwister struct {
	mt          [624]uint32
	index       int
	initialized bool
}

// Seed converts the seed to 32 bits by XORing the high and low halves.
func (t *mersenneTwister) Seed(seed int64) {
	t.initialize(uint32(((seed >> 32) ^ seed) & math.MaxUint32))
}

// Int63 combines the bits of two Uint32 values.
func (t *mersenneTwister) Int63() int64 {
	a := t.Uint32()
	b := t.Uint32()
	return (int64(a) << 31) ^ int64(b)
}

func (t *mersenneTwister) Uint32() uint32 {
	if !t.initialized {
		t.initialize(4357) // value from the original paper
	}
	// every 624 calls, revolve the untempered seed matrix
	if t.index == 0 {
		t.generateUntempered()
	}
	y := t.mt[t.index]
	t.index++
	if t.index >= len(t.mt) {
		t.index = 0
	}
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}

func (t *mersenneTwister) initialize(seed uint32) {
	t.index = 0
	t.mt[0] = seed
	for i := 1; i < len(t.mt); i++ {
		t.mt[i] = (1812433253*(t.mt[i-1]^(t.mt[i-1]>>30)) + uint32(i)) & math.MaxUint32
	}
	t.initialized = true
}

func (t *mersenneTwister) generateUntempered() {
	mag01 := [2]uint32{0x0, 0x9908b0df}
	for i := range len(t.mt) {
		y := (t.mt[i] & 0x80000000) | (t.mt[(i+1)%len(t.mt)] & 0x7fffffff)
		t.mt[i] = (t.mt[(i+397)%len(t.mt)] ^ (y >> 1)) ^ mag01[y&0x01]
	}
}

// solitonDistribution returns the CDF of the ideal soliton
// distribution, one-based: the probability of degree 1 is cdf[1].
// To sample, pick r in [0,1) and find the smallest i with cdf[i] >= r.
func solitonDistribution(n int) []float64 {
	cdf := make([]float64, n+1)
	cdf[1] = 1 / float64(n)
	for i := 2; i < len(cdf); i++ {
		cdf[i] = cdf[i-1] + (1 / (float64(i) * float64(i-1)))
	}
	return cdf
}

// pickDegree returns the smallest index i such that cdf[i] > r.
func pickDegree(random *rand.Rand, cdf []float64) int {
	r := random.Float64()
	d := sort.SearchFloat64s(cdf, r)
	if cdf[d] > r {
		return d
	}
	if d < len(cdf)-1 {
		return d + 1
	}
	return len(cdf) - 1
}

// sampleUniform picks num distinct numbers from [0,max) uniformly,
// sorted. If num >= max it returns all indices without touching the
// PRNG.
func sampleUniform(random *rand.Rand, num, max int) []int {
	if num >= max {
		picks := make([]int, max)
		for i := range picks {
			picks[i] = i
		}
		return picks
	}
	picks := make([]int, num)
	seen := make(map[int]bool)
	for i := range num {
		p := random.Intn(max)
		for seen[p] {
			p = random.Intn(max)
		}
		picks[i] = p
		seen[p] = true
	}
	sort.Ints(picks)
	return picks
}

// ltPicker derives the source-block composition of a code block from
// its seed: reseed MT19937, sample a degree from the soliton CDF,
// sample that many distinct block indices. Encoder and decoder share
// this; it is the normative index derivation of the wire format.
type ltPicker struct {
	k   int
	cdf []float64
	rnd *rand.Rand
}

func newLTPicker(k int) *ltPicker {
	return &ltPicker{k: k, cdf: solitonDistribution(k), rnd: rand.New(&mersenneTwister{})}
}

func (p *ltPicker) indices(seed int64) []int {
	p.rnd.Seed(seed)
	d := pickDegree(p.rnd, p.cdf)
	return sampleUniform(p.rnd, d, p.k)
}

// xorBytes XORs src into dst; both must have the block length.
func xorBytes(dst, src []byte) {
	for i := range src {
		dst[i] ^= src[i]
	}
}

// ltMatrix is the triangular sparse matrix of XOR equations used for
// online decoding (the Bioglio, Grangetto, Gaeta variant). coeff[i]
// holds the sorted source-block indices XORed into value v[i], with
// the invariant coeff[i][0] == i or len(coeff[i]) == 0.
type ltMatrix struct {
	coeff [][]int
	v     [][]byte
}

func newLTMatrix(k int) *ltMatrix {
	return &ltMatrix{coeff: make([][]int, k), v: make([][]byte, k)}
}

// xorRow reduces the candidate equation (indices, b) against matrix
// row s: XOR the values, take the symmetric difference of the sorted
// coefficient sets.
func (m *ltMatrix) xorRow(s int, indices []int, b []byte) ([]int, []byte) {
	xorBytes(b, m.v[s])
	var newIndices []int
	coeffs := m.coeff[s]
	var i, j int
	for i < len(coeffs) && j < len(indices) {
		switch {
		case coeffs[i] == indices[j]:
			i++
			j++
		case coeffs[i] < indices[j]:
			newIndices = append(newIndices, coeffs[i])
			i++
		default:
			newIndices = append(newIndices, indices[j])
			j++
		}
	}
	newIndices = append(newIndices, coeffs[i:]...)
	newIndices = append(newIndices, indices[j:]...)
	return newIndices, b
}

// addEquation reduces the incoming equation by XOR until it either
// fits an empty row (keeping the matrix triangular) or is discarded
// as redundant.
func (m *ltMatrix) addEquation(components []int, b []byte) {
	for len(components) > 0 && len(m.coeff[components[0]]) > 0 {
		s := components[0]
		if len(components) >= len(m.coeff[s]) {
			components, b = m.xorRow(s, components, b)
		} else {
			// swap the shorter new equation in, reduce the old one
			components, m.coeff[s] = m.coeff[s], components
			b, m.v[s] = m.v[s], b
		}
	}
	if len(components) > 0 {
		m.coeff[components[0]] = components
		m.v[components[0]] = b
	}
}

// have counts the recovered (populated) rows.
func (m *ltMatrix) have() int {
	n := 0
	for _, r := range m.coeff {
		if len(r) > 0 {
			n++
		}
	}
	return n
}

// determined reports whether every row is populated, i.e. the system
// is solvable.
func (m *ltMatrix) determined() bool {
	return m.have() == len(m.coeff)
}

// reduce performs the back-substitution over the triangular matrix,
// leaving each row with a single coefficient. Idempotent; only valid
// once determined.
func (m *ltMatrix) reduce() {
	for i := len(m.coeff) - 1; i >= 0; i-- {
		for j := 0; j < i; j++ {
			cj := m.coeff[j]
			for k := 1; k < len(cj); k++ {
				if cj[k] == m.coeff[i][0] {
					xorBytes(m.v[j], m.v[i])
					break
				}
			}
		}
		m.coeff[i] = m.coeff[i][:1]
	}
}

// message concatenates the solved source blocks.
func (m *ltMatrix) message(blockLen int) []byte {
	out := make([]byte, 0, len(m.v)*blockLen)
	for _, b := range m.v {
		out = append(out, b...)
	}
	return out
}
