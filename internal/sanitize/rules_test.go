package sanitize

import "testing"

// A spec-derived enum Rule (issue #28): SCREAMING_SNAKE and ISO-code
// vocabularies must survive sanitization in clear text, but only for the
// field the rule names, and only for values that still look like enum
// members — never a blanket allow on the field.
func TestEnumRuleSurvivesRedaction(t *testing.T) {
	tok := NewTokenizer("rules-test-key-0123456789ab", "local", 1)
	rules := []Rule{{Field: "status", Mode: ModeAllow, Values: []string{"ACTIVE", "PENDING"}}}

	cases := []struct {
		name string
		key  string
		val  string
		want string // "" means "must not be the raw value" (dropped or tokenized)
	}{
		{"declared member", "status", "ACTIVE", "ACTIVE"},
		{"new but enum-shaped member", "status", "SUSPENDED", "SUSPENDED"},
		{"snake_case shaped member", "status", "on_hold", "on_hold"},
		{"free text under the same field fails closed", "status", "payment could not be completed", ""},
		{"unrelated field is untouched by the rule", "currency", "NGN", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Sanitize(map[string]any{c.key: c.val}, tok, rules, false)
			body, _ := res.Sanitized.(map[string]any)
			got, _ := body[c.key].(string)
			if c.want != "" {
				if got != c.want {
					t.Fatalf("Sanitize(%q:%q) = %v, want raw %q preserved", c.key, c.val, body[c.key], c.want)
				}
			} else if got == c.val {
				t.Fatalf("Sanitize(%q:%q) survived raw — expected it to fail closed (drop/tokenize)", c.key, c.val)
			}
		})
	}
}

// A rule with no Values is unconditional, matching pre-existing behaviour —
// this is the override point issue #28 found built but never wired up.
func TestUnconditionalRuleStillOverridesUnconditionally(t *testing.T) {
	tok := NewTokenizer("rules-test-key-0123456789ab", "local", 1)
	rules := []Rule{{Field: "internal_note", Mode: ModeDrop}}
	res := Sanitize(map[string]any{"internal_note": "anything at all"}, tok, rules, false)
	body, _ := res.Sanitized.(map[string]any)
	if _, present := body["internal_note"]; present {
		t.Fatalf("unconditional DROP rule should have removed the field, got %v", body)
	}
}

// A rule's Values gate must not let a non-string leaf (which never carries
// PII risk from shape, but also never "matches" an enum) through the rule
// path — it just falls back to the ordinary detector.
func TestEnumRuleIgnoresNonStringLeaves(t *testing.T) {
	tok := NewTokenizer("rules-test-key-0123456789ab", "local", 1)
	rules := []Rule{{Field: "status", Mode: ModeAllow, Values: []string{"ACTIVE"}}}
	res := Sanitize(map[string]any{"status": float64(1)}, tok, rules, false)
	body, _ := res.Sanitized.(map[string]any)
	if body["status"] != float64(1) {
		t.Fatalf("numeric leaf should fall through to the default (ALLOW), got %v", body["status"])
	}
}
