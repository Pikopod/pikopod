package sanitize

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"
)

// A format-preserving token over a tiny space (2-digit expiry) can self-map
// under ~1% of salts, leaving the ORIGINAL bytes on disk. Tokenize must
// rescue that case deterministically — this hunts a self-mapping salt and
// proves the rescue engages (the agent's canary gate only catches this one
// run in a hundred; here it is exercised every run).
func TestTokenizeNeverReturnsTheOriginal(t *testing.T) {
	const value = "12"
	found := false
	for i := 0; i < 2000; i++ {
		salt := fmt.Sprintf("salt-%d", i)
		tok := NewTokenizer(salt, "local", 1)
		mac := hmac.New(sha256.New, tok.orgKey)
		mac.Write([]byte(value))
		raw := tok.render(FormatDigits, value, mac.Sum(nil))
		if raw != value {
			continue
		}
		found = true
		got, format := tok.Tokenize(value)
		if got == value {
			t.Fatalf("salt %q: identity token survived Tokenize", salt)
		}
		if format != FormatDigits || len(got) != len(value) {
			t.Fatalf("the rescue must stay format-preserving: %q (%s)", got, format)
		}
		// Determinism: the rescued token is stable per salt.
		again, _ := tok.Tokenize(value)
		if again != got {
			t.Fatalf("rescued token must be deterministic: %q vs %q", got, again)
		}
	}
	if !found {
		t.Fatal("no self-mapping salt in 2000 — the probe stopped exercising the rescue path")
	}
	// The common case is untouched: a non-colliding value tokenizes in one
	// shot and stays referentially stable.
	tok := NewTokenizer("fixed-salt", "local", 1)
	a, _ := tok.Tokenize("4242424242424242")
	b, _ := tok.Tokenize("4242424242424242")
	if a != b || a == "4242424242424242" || len(a) != 16 {
		t.Fatalf("stable format-preserving tokenization broke: %q %q", a, b)
	}
}
