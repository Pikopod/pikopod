package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/alert"
)

// schema/drift-event.schema.json is the PUBLISHED wire contract for the one
// event pikopod emits. Nothing else in the build ties it to the Go struct, so
// a field added to alert.DriftEvent would silently ship an event the documented
// schema rejects (it is additionalProperties:false). This test is that tie:
// property set, required set (derived from `omitempty`), and the version const
// must all track the struct exactly.
func TestDriftEventSchemaMatchesStruct(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "schema", "drift-event.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var doc struct {
		Title      string                     `json:"title"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if doc.Title != "DriftEvent" {
		t.Fatalf("unexpected schema title %q", doc.Title)
	}

	// Walk the struct's JSON tags: every emitted key, and which of them are
	// unconditionally emitted (no omitempty) and therefore always present.
	var props, required []string
	rt := reflect.TypeOf(alert.DriftEvent{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("field %s has no json tag — it cannot be part of a wire contract", rt.Field(i).Name)
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		props = append(props, name)
		omitempty := false
		for _, opt := range parts[1:] {
			if opt == "omitempty" {
				omitempty = true
			}
		}
		if !omitempty {
			required = append(required, name)
		}
	}

	var schemaProps []string
	for name := range doc.Properties {
		schemaProps = append(schemaProps, name)
	}
	assertSameSet(t, "properties", props, schemaProps)
	assertSameSet(t, "required", required, doc.Required)

	// schema_version's const is the protocol number; it must be the one the
	// producer actually stamps on every event.
	var sv struct {
		Const string `json:"const"`
	}
	if err := json.Unmarshal(doc.Properties["schema_version"], &sv); err != nil {
		t.Fatalf("schema_version property: %v", err)
	}
	if sv.Const != alert.SchemaVersion {
		t.Fatalf("schema pins schema_version const %q but alert.SchemaVersion is %q", sv.Const, alert.SchemaVersion)
	}
}

func assertSameSet(t *testing.T, what string, fromStruct, fromSchema []string) {
	t.Helper()
	have := map[string]bool{}
	for _, s := range fromSchema {
		have[s] = true
	}
	var missing, extra []string
	for _, s := range fromStruct {
		if !have[s] {
			missing = append(missing, s)
		}
		delete(have, s)
	}
	for s := range have {
		extra = append(extra, s)
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("drift-event.schema.json %s drifted from alert.DriftEvent: missing %v, stale %v", what, missing, extra)
	}
}
