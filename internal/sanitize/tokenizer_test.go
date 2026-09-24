package sanitize

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"
)

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

		again, _ := tok.Tokenize(value)
		if again != got {
			t.Fatalf("rescued token must be deterministic: %q vs %q", got, again)
		}
	}
	if !found {
		t.Fatal("no self-mapping salt in 2000 — the probe stopped exercising the rescue path")
	}

	tok := NewTokenizer("fixed-salt", "local", 1)
	a, _ := tok.Tokenize("4242424242424242")
	b, _ := tok.Tokenize("4242424242424242")
	if a != b || a == "4242424242424242" || len(a) != 16 {
		t.Fatalf("stable format-preserving tokenization broke: %q %q", a, b)
	}
}
