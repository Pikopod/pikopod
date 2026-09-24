package bridge

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
)

func TestAssertionsForEveryKind(t *testing.T) {
	cases := []struct {
		name    string
		ev      alert.DriftEvent
		wantErr string
		wantOp  string
	}{
		{"type-changed", alert.DriftEvent{Kind: drift.TypeChanged, Field: "fee", Before: "number", After: "string"}, "", "equals"},
		{"enum-new", alert.DriftEvent{Kind: drift.EnumValueNew, Field: "status", Before: "active,failed,…", After: "on_hold"}, "", "isOneOf"},
		{"status-new-5xx", alert.DriftEvent{Kind: drift.StatusNew, After: "5xx"}, "", "lt"},
		{"status-new-4xx", alert.DriftEvent{Kind: drift.StatusNew, After: "4xx"}, "", "lt"},
		{"status-code-single", alert.DriftEvent{Kind: drift.StatusCodeChanged, Before: "200", After: "201"}, "", "equals"},
		{"status-code-multi", alert.DriftEvent{Kind: drift.StatusCodeChanged, Before: "200,201", After: "204"}, "multi-code", ""},
		{"status-code-garbage", alert.DriftEvent{Kind: drift.StatusCodeChanged, Before: "abc", After: "204"}, "not numeric", ""},
		{"nullable", alert.DriftEvent{Kind: drift.FieldNullable, Field: "x", Before: "string"}, "not pinnable", ""},
		{"error-shape", alert.DriftEvent{Kind: drift.ErrorShapeChanged}, "not pinnable", ""},
		{"declared", alert.DriftEvent{Kind: drift.Kind("declared:endpoint-removed")}, "not replayable", ""},
		{"unknown", alert.DriftEvent{Kind: drift.Kind("future_kind")}, "unknown drift kind", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			as, _, err := assertionsFor(&c.ev)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("want refusal containing %q, got err=%v assertions=%v", c.wantErr, err, as)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if len(as) == 0 {
				t.Fatal("pinnable kind produced no assertions")
			}
			if op := as[0].(map[string]any)["op"]; op != c.wantOp {
				t.Fatalf("op %v, want %s (%+v)", op, c.wantOp, as[0])
			}
		})
	}

	as, _, err := assertionsFor(&alert.DriftEvent{Kind: drift.EnumValueNew, Field: "s", Before: "a,b,…", After: "c"})
	if err != nil {
		t.Fatal(err)
	}
	vals := as[0].(map[string]any)["expected"].([]any)
	for _, v := range vals {
		if v == "…" {
			t.Fatal("truncation marker must not become an allowed enum value")
		}
	}
}
