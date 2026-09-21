package specdiff

import (
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

func allOf(members ...ir.IrSchemaNode) ir.IrSchemaNode {
	return ir.IrSchemaNode{Composition: &ir.SchemaComposition{Kind: "allOf", Members: members}}
}

func oneOf(members ...ir.IrSchemaNode) ir.IrSchemaNode {
	return ir.IrSchemaNode{Composition: &ir.SchemaComposition{Kind: "oneOf", Members: members}}
}

// allOf is flattened before diffing: a required property removed from one
// MEMBER must surface as a normal property finding, not a silent skip.
func TestAllOfMembersAreDiffed(t *testing.T) {
	oldEp := ep("GET", "/widgets", with200(allOf(
		objSchema(prop("id", true, strSchema())),
		objSchema(prop("status", true, strSchema())),
	)))
	newEp := ep("GET", "/widgets", with200(allOf(
		objSchema(prop("id", true, strSchema())),
		objSchema(),
	)))
	fs := Diff(def(oldEp), def(newEp))
	f := find(t, fs, "response-required-property-removed")
	if f.Level != Warn {
		t.Fatalf("guaranteed removal through allOf derives the same as became-optional: %+v", f)
	}
}

func TestAllOfUnchangedIsQuiet(t *testing.T) {
	mk := func() ir.Endpoint {
		return ep("GET", "/widgets", with200(allOf(
			objSchema(prop("id", true, strSchema())),
			objSchema(prop("status", true, strSchema())),
		)))
	}
	if fs := Diff(def(mk()), def(mk())); len(fs) != 0 {
		t.Fatalf("identical allOf must be silent: %v", ids(fs))
	}
}

func TestOneOfVariantRemovedIsReported(t *testing.T) {
	oldEp := ep("GET", "/widgets", with200(oneOf(strSchema(), objSchema(), strSchema())))
	newEp := ep("GET", "/widgets", with200(oneOf(strSchema(), objSchema())))
	fs := Diff(def(oldEp), def(newEp))
	// Narrows × Response with tolerance derives INFO by the severity law (the output space
	// contracts; consumers that handled the shape keep working) — the same
	// level as response-enum-value-removed.
	f := find(t, fs, "response-variant-removed")
	if f.Level != Info {
		t.Fatalf("removed response variant shrinks — INFO by law: %+v", f)
	}
}

func TestOneOfVariantAddedIsReported(t *testing.T) {
	oldEp := ep("GET", "/widgets", with200(oneOf(strSchema(), objSchema())))
	newEp := ep("GET", "/widgets", with200(oneOf(strSchema(), objSchema(), strSchema())))
	fs := Diff(def(oldEp), def(newEp))
	find(t, fs, "response-variant-added")
}

// A reorder of same-count variants must stay SILENT — member identity is
// positional and pairwise diffing would flood false findings.
func TestOneOfReorderIsSilent(t *testing.T) {
	oldEp := ep("GET", "/widgets", with200(oneOf(strSchema(), objSchema(prop("id", true, strSchema())))))
	newEp := ep("GET", "/widgets", with200(oneOf(objSchema(prop("id", true, strSchema())), strSchema())))
	if fs := Diff(def(oldEp), def(newEp)); len(fs) != 0 {
		t.Fatalf("variant reorder must be silent: %v", ids(fs))
	}
}

func TestCompositionKindChangeIsRestructure(t *testing.T) {
	oldEp := ep("GET", "/widgets", with200(oneOf(strSchema(), objSchema())))
	newSchema := ir.IrSchemaNode{Composition: &ir.SchemaComposition{Kind: "anyOf", Members: []ir.IrSchemaNode{strSchema(), objSchema()}}}
	newEp := ep("GET", "/widgets", with200(newSchema))
	fs := Diff(def(oldEp), def(newEp))
	find(t, fs, "schema-restructured")
}
