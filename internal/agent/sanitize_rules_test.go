package agent

import (
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
)

func TestSanitizeRulesTrustOnlyDeclaredEnumProvenance(t *testing.T) {
	def := &ir.ApiDefinition{
		Schemas: []ir.NamedSchema{
			{Schema: ir.IrSchemaNode{Properties: []ir.PropertySchema{
				{Name: "status", Schema: ir.IrSchemaNode{
					EnumValues: &ir.Prov[[]any]{Value: []any{"ACTIVE", "PENDING"}, Provenance: ir.ProvenanceExplicit},
				}},
				{Name: "guess", Schema: ir.IrSchemaNode{
					EnumValues: &ir.Prov[[]any]{Value: []any{"SECRET"}, Provenance: ir.ProvenanceInferred},
				}},
			}}},
		},
	}
	rules := sanitizeRules(def)
	if len(rules) != 2 {
		t.Fatalf("trusted enum rules = %d, want 2: %#v", len(rules), rules)
	}
	for _, rule := range rules {
		if rule.Mode != sanitize.ModeAllow || rule.Field != "status" || len(rule.AllowedValues) != 1 {
			t.Fatalf("unexpected rule: %#v", rule)
		}
	}
}
