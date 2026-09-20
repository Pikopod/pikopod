package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConditionalRulesAllowOnlyDeclaredValues(t *testing.T) {
	tok := NewTokenizer("rules-test-key-0123456789ab", "local", 1)
	rules := []Rule{
		{Field: "status", Mode: ModeAllow, AllowedValues: []any{"ACTIVE", "PENDING_APPROVAL"}},
	}

	res := Sanitize(map[string]any{
		"status":  "ACTIVE",
		"message": "ACTIVE",
		"state":   "PENDING_APPROVAL",
	}, tok, rules, false)
	raw, _ := json.Marshal(res.Sanitized)
	text := string(raw)
	if !strings.Contains(text, "\"status\":\"ACTIVE\"") {
		t.Fatalf("declared enum value was not preserved: %s", text)
	}
	if strings.Contains(text, "\"message\":\"ACTIVE\"") || strings.Contains(text, "\"state\":\"PENDING_APPROVAL\"") {
		t.Fatalf("field-scoped rule relaxed unrelated values: %s", text)
	}

	res = Sanitize(map[string]any{"status": "SUSPENDED"}, tok, rules, false)
	if got, ok := res.Sanitized.(map[string]any)["status"]; !ok || got == "SUSPENDED" {
		t.Fatalf("undeclared value bypassed the detector: %#v", res.Sanitized)
	}
}

func TestConditionalRulesCompareJSONNumbers(t *testing.T) {
	tok := NewTokenizer("rules-number-key-0123456789ab", "local", 1)
	rules := []Rule{{Field: "code", Mode: ModeAllow, AllowedValues: []any{float64(200)}}}
	res := Sanitize(map[string]any{"code": json.Number("200")}, tok, rules, false)
	if got := res.Sanitized.(map[string]any)["code"]; got != json.Number("200") {
		t.Fatalf("numeric enum value was not preserved: %#v", got)
	}
}
