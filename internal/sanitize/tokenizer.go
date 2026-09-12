// Package sanitize redacts and tokenizes deterministically and one-way: the same
// value always yields the same shape-preserving token per install. Golden-pinned.
package sanitize

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

type TokenFormat string

const (
	FormatEmail      TokenFormat = "email"
	FormatUUID       TokenFormat = "uuid"
	FormatPrefixedID TokenFormat = "prefixed-id"
	FormatDigits     TokenFormat = "digits"
	FormatAlnum      TokenFormat = "alnum"
	FormatGeneric    TokenFormat = "generic"
)

var (
	uuidRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	emailRE    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	prefixedRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*_)([A-Za-z0-9]+)$`)
	digitsRE   = regexp.MustCompile(`^\d+$`)
	alnumRE    = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

const (
	lowerAlphabet = "abcdefghijklmnopqrstuvwxyz"
	alnumAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	hexAlphabet   = "0123456789abcdef"
	digitAlphabet = "0123456789"
)

// Tokenizer derives orgKey = HMAC(masterKey, "tokenize:<scope>:v<version>").
// The master key is the per-install salt and scope is "local".
type Tokenizer struct {
	orgKey     []byte
	KeyVersion int
}

func NewTokenizer(masterKey, scope string, keyVersion int) *Tokenizer {
	if keyVersion == 0 {
		keyVersion = 1
	}
	mac := hmac.New(sha256.New, []byte(masterKey))
	fmt.Fprintf(mac, "tokenize:%s:v%d", scope, keyVersion)
	return &Tokenizer{orgKey: mac.Sum(nil), KeyVersion: keyVersion}
}

func (t *Tokenizer) DetectFormat(value string) TokenFormat {
	switch {
	case emailRE.MatchString(value):
		return FormatEmail
	case uuidRE.MatchString(value):
		return FormatUUID
	case prefixedRE.MatchString(value):
		return FormatPrefixedID
	case digitsRE.MatchString(value):
		return FormatDigits
	case alnumRE.MatchString(value):
		return FormatAlnum
	default:
		return FormatGeneric
	}
}

// HashOf is the one-way lookup key (never the original value).
func (t *Tokenizer) HashOf(value string) string {
	mac := hmac.New(sha256.New, t.orgKey)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (t *Tokenizer) Tokenize(value string) (token string, format TokenFormat) {
	format = t.DetectFormat(value)
	mac := hmac.New(sha256.New, t.orgKey)
	mac.Write([]byte(value))
	seed := mac.Sum(nil)
	token = t.render(format, value, seed)
	// Over a tiny space a format-preserving token can equal its input, leaving the
	// original bytes on disk; re-derive deterministically until it differs.
	for i := 0; token == value && i < 16; i++ {
		next := hmac.New(sha256.New, seed)
		next.Write([]byte{byte(i)})
		seed = next.Sum(nil)
		token = t.render(format, value, seed)
	}
	if token == value {
		token = "tok_" + pick(seed, 0, alnumAlphabet, 16)
	}
	return token, format
}

func (t *Tokenizer) render(format TokenFormat, value string, seed []byte) string {
	switch format {
	case FormatEmail:
		return pick(seed, 0, lowerAlphabet, 8) + "@" + pick(seed, 8, lowerAlphabet, 6) + ".test"
	case FormatUUID:
		h := expand(seed, 32)
		s := make([]byte, 32)
		for i, b := range h[:32] {
			s[i] = hexAlphabet[int(b)%16]
		}
		// Shape-preserving UUIDv4-ish; deterministic. Slice offsets are exact
		// (13:16 after the literal '4', 17:20 after the 'a').
		return fmt.Sprintf("%s-%s-4%s-a%s-%s", s[0:8], s[8:12], s[13:16], s[17:20], s[20:32])
	case FormatPrefixedID:
		m := prefixedRE.FindStringSubmatch(value)
		return m[1] + pick(seed, 0, alnumAlphabet, len(m[2])) // preserve prefix + length
	case FormatDigits:
		return pick(seed, 0, digitAlphabet, len(value))
	case FormatAlnum:
		return pick(seed, 0, alnumAlphabet, len(value))
	default:
		return "tok_" + pick(seed, 0, alnumAlphabet, 16)
	}
}

func pick(seed []byte, offset int, alphabet string, length int) string {
	bytes := expand(seed, offset+length)[offset : offset+length]
	out := make([]byte, length)
	for i, b := range bytes {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// expand stretches the seed to n deterministic bytes (HMAC counter mode).
func expand(seed []byte, n int) []byte {
	var out []byte
	counter := byte(0)
	for len(out) < n {
		mac := hmac.New(sha256.New, seed)
		mac.Write([]byte{counter})
		out = append(out, mac.Sum(nil)...)
		counter++
	}
	if n < 1 {
		n = 1
	}
	return out[:n]
}
