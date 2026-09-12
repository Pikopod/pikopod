package sandbox

import (
	"math"
	"unicode/utf16"
)

// Prng is xmur3 feeding mulberry32; all sandbox randomness flows through it, so a
// DETERMINISTIC sandbox replays byte-for-byte. Goldens pin the exact arithmetic.
type Prng struct {
	a uint32
}

func NewPrng(seed string) *Prng {
	// xmur3 over UTF-16 code units (JS charCodeAt), one finalizer call.
	h := uint32(1779033703) ^ uint32(len(utf16.Encode([]rune(seed))))
	for _, cu := range utf16.Encode([]rune(seed)) {
		h = (h ^ uint32(cu)) * 3432918353
		h = (h << 13) | (h >> 19)
	}
	h = (h ^ (h >> 16)) * 2246822507
	h = (h ^ (h >> 13)) * 3266489909
	h ^= h >> 16
	return &Prng{a: h}
}

// Next returns a float in [0, 1) — mulberry32.
func (p *Prng) Next() float64 {
	p.a += 0x6d2b79f5
	t := (p.a ^ (p.a >> 15)) * (1 | p.a)
	t = (t + (t^(t>>7))*(61|t)) ^ t
	return float64(t^(t>>14)) / 4294967296
}

// Int returns an integer in [min, max] inclusive.
func (p *Prng) Int(min, max int) int {
	if max <= min {
		return min
	}
	return min + int(math.Floor(p.Next()*float64(max-min+1)))
}

func (p *Prng) Bool() bool { return p.Next() < 0.5 }

func (p *Prng) Pick(items []string) string { return items[p.Int(0, len(items)-1)] }

const (
	prngLower = "abcdefghijklmnopqrstuvwxyz"
	prngAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	prngHex   = "0123456789abcdef"
)

func (p *Prng) fromAlphabet(alphabet string, length int) string {
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		out[i] = alphabet[p.Int(0, len(alphabet)-1)]
	}
	return string(out)
}

// Word returns a lowercase word of 3–9 letters.
func (p *Prng) Word() string { return p.fromAlphabet(prngLower, p.Int(3, 9)) }

func (p *Prng) Token(length int) string { return p.fromAlphabet(prngAlnum, length) }

func (p *Prng) Hex(length int) string { return p.fromAlphabet(prngHex, length) }
