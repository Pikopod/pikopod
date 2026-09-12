package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

// The type-independence contract: a secret is the same secret whatever its
// JSON type. This test is the direct assertion behind every
// knownStricterKeyRE pruning in parity_test.go — each divergence pruned there
// must be pinned here.
func TestSecretsRedactedRegardlessOfJSONType(t *testing.T) {
	tok := NewTokenizer("strictness-test-key-0123456789ab", "local", 1)

	substituteKeys := []string{"cvv", "cvc", "cvv2", "cvc2", "cid", "csc", "pin", "otp", "passcode", "security_code", "card_cvv", "transaction-pin"}
	tokenizeKeys := []string{"exp_month", "exp_year", "expiry", "expiration", "valid_thru", "dob", "date_of_birth", "birthday", "account_number", "bvn", "nin", "card_number"}
	values := []any{float64(123), "123", float64(9471), "9471", float64(12), true}

	for _, k := range substituteKeys {
		for _, v := range values {
			if mode := Classify(k, v, false); mode != ModeSubstitute {
				t.Errorf("Classify(%q, %v [%T]) = %s, want SUBSTITUTE — JSON type must not matter", k, v, v, mode)
			}
		}
	}
	for _, k := range tokenizeKeys {
		for _, v := range values {
			mode := Classify(k, v, false)
			if mode == ModeAllow {
				t.Errorf("Classify(%q, %v [%T]) = ALLOW — this key class must never pass a value through raw", k, v, v)
			}
		}
	}

	// End to end through the walker, at every level the sanitizer walks:
	// top level, nested object, array element, and a header map.
	body := map[string]any{
		"cvv":    float64(947),
		"amount": float64(5000), // control: legitimately allowed
		"card": map[string]any{
			"cvc2":      "9471",
			"exp_month": float64(12),
			"exp_year":  float64(2027),
			"pin":       float64(934187),
		},
		"attempts": []any{map[string]any{"otp": float64(91736408), "dob": float64(19470213)}},
	}
	res := Sanitize(body, tok, nil, false)
	raw, _ := json.Marshal(res.Sanitized)
	text := string(raw)
	for _, leak := range []string{"947,", "\"9471\"", ":12,", ":12}", ":2027", "934187", "91736408", "19470213"} {
		if strings.Contains(text, leak) {
			t.Errorf("sanitized body still contains %q:\n%s", leak, text)
		}
	}
	if !strings.Contains(text, "5000") {
		t.Errorf("control field wrongly redacted (amounts must survive):\n%s", text)
	}

	// Header level: a custom header carrying a short numeric secret.
	headers := map[string]any{"x-card-cvv": "947", "x-otp": "91736408", "content-type": "application/json"}
	hres := Sanitize(headers, tok, nil, true)
	hraw, _ := json.Marshal(hres.Sanitized)
	if strings.Contains(string(hraw), "947") || strings.Contains(string(hraw), "91736408") {
		t.Errorf("header secrets survived: %s", hraw)
	}
	if !strings.Contains(string(hraw), "application/json") {
		t.Errorf("safe header wrongly redacted: %s", hraw)
	}
}
