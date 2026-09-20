package sanitize

import "testing"

func TestValueGatedRuleAllowsOnlyTheDeclaredSet(t *testing.T) {
	tok := NewTokenizer("k", "s", 1)
	rules := []Rule{{Field: "currency", Mode: ModeAllow, AllowedValues: []string{"NGN", "USD"}}}
	in := map[string]any{"currency": "NGN", "amount": 5.0}
	res := Sanitize(in, tok, rules, false)
	if got := res.Sanitized.(map[string]any)["currency"]; got != "NGN" {
		t.Fatalf("declared value must survive: %v", got)
	}
	for _, v := range []any{"some free text error", "GHS", 7.0} {
		with := Sanitize(map[string]any{"currency": v}, tok, rules, false)
		without := Sanitize(map[string]any{"currency": v}, tok, nil, false)
		if got, want := with.Sanitized.(map[string]any)["currency"], without.Sanitized.(map[string]any)["currency"]; got != want {
			t.Fatalf("%v: outside the set must classify exactly as today: got %v want %v", v, got, want)
		}
		if len(with.Redactions) != len(without.Redactions) {
			t.Fatalf("%v: redactions must match today's: %v vs %v", v, with.Redactions, without.Redactions)
		}
	}
}

func TestRuleWithoutValuesStillMatchesAnyLeaf(t *testing.T) {
	tok := NewTokenizer("k", "s", 1)
	res := Sanitize(map[string]any{"note": "free text"}, tok, []Rule{{Field: "note", Mode: ModeAllow}}, false)
	if res.Sanitized.(map[string]any)["note"] != "free text" {
		t.Fatal("an unconditional rule keeps today's semantics")
	}
}
