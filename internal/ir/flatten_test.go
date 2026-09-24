package ir

import "testing"

func node(typ string, props ...PropertySchema) IrSchemaNode {
	return IrSchemaNode{Type: Prov[ScalarType]{Value: typ}, Properties: props}
}

func p(name string, required bool) PropertySchema {
	return PropertySchema{Name: name, Required: Prov[bool]{Value: required}, Schema: IrSchemaNode{Type: Prov[ScalarType]{Value: "string"}}}
}

func TestFlattenAllOfMergesProperties(t *testing.T) {
	n := IrSchemaNode{Composition: &SchemaComposition{Kind: "allOf", Members: []IrSchemaNode{
		node("object", p("id", true)),
		node("object", p("status", false)),
	}}}
	got := FlattenAllOf(&n, nil)
	if got.Composition != nil || len(got.Properties) != 2 {
		t.Fatalf("merged: %+v", got)
	}
	if got.Type.Value != "object" {
		t.Fatalf("type: %s", got.Type.Value)
	}
}

func TestFlattenAllOfRequiredIsSticky(t *testing.T) {

	n := IrSchemaNode{Composition: &SchemaComposition{Kind: "allOf", Members: []IrSchemaNode{
		node("object", p("id", true)),
		node("object", p("id", false)),
	}}}
	got := FlattenAllOf(&n, nil)
	if len(got.Properties) != 1 || !got.Properties[0].Required.Value {
		t.Fatalf("required must be sticky: %+v", got.Properties)
	}
}

func TestFlattenAllOfTypeConflictBailsOut(t *testing.T) {
	n := IrSchemaNode{Composition: &SchemaComposition{Kind: "allOf", Members: []IrSchemaNode{
		node("object"), node("string"),
	}}}
	got := FlattenAllOf(&n, nil)
	if got.Composition == nil {
		t.Fatal("conflicting member types are unsatisfiable — must stay composed")
	}
}

func TestFlattenAllOfLeavesChoicesAlone(t *testing.T) {
	n := IrSchemaNode{Composition: &SchemaComposition{Kind: "oneOf", Members: []IrSchemaNode{node("string")}}}
	if got := FlattenAllOf(&n, nil); got.Composition == nil || got.Composition.Kind != "oneOf" {
		t.Fatalf("oneOf must pass through untouched: %+v", got)
	}
}

func TestFlattenAllOfThroughRefsAndNesting(t *testing.T) {
	ref := "Base"
	table := map[string]*IrSchemaNode{
		"Base": {Type: Prov[ScalarType]{Value: "object"}, Properties: []PropertySchema{p("id", true)}},
	}
	n := IrSchemaNode{Composition: &SchemaComposition{Kind: "allOf", Members: []IrSchemaNode{
		{Ref: &ref},
		{Composition: &SchemaComposition{Kind: "allOf", Members: []IrSchemaNode{node("object", p("extra", false))}}},
	}}}
	got := FlattenAllOf(&n, table)
	if got.Composition != nil || len(got.Properties) != 2 {
		t.Fatalf("refs + nested allOf must flatten: %+v", got)
	}
}

func TestFlattenAllOfNullableIntersects(t *testing.T) {
	nullable := node("object")
	nullable.Nullable = Prov[bool]{Value: true}
	strict := node("object")
	n := IrSchemaNode{Composition: &SchemaComposition{Kind: "allOf", Members: []IrSchemaNode{nullable, strict}}}
	if got := FlattenAllOf(&n, nil); got.Nullable.Value {
		t.Fatal("null passes only if EVERY member allows it")
	}
}
