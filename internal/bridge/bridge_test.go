package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario"
)

const widgetsV1 = `{
  "openapi": "3.0.0", "info": {"title": "W", "version": "1"},
  "paths": {"/widgets": {"post": {
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
    "responses": {"201": {"description": "c", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
  }}},
  "components": {"schemas": {"Widget": {"type": "object", "properties": {
    "id": {"type": "string"}, "name": {"type": "string"}
  }}}}
}`

const widgetsV2 = `{
  "openapi": "3.0.0", "info": {"title": "W", "version": "2"},
  "paths": {"/widgets": {"post": {
    "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
    "responses": {"201": {"description": "c", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
  }}},
  "components": {"schemas": {"Widget": {"type": "object", "properties": {
    "id": {"type": "string"}, "name": {"type": "string"}, "fee_bearer": {"type": "string"}
  }}}}
}`

func runAgainst(t *testing.T, spec string, pack map[string]any) *scenario.RunResult {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(spec))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	vr, parsed := scenario.ValidateScenario(pack["definition"], def)
	if !vr.Valid {
		t.Fatalf("generated pack does not ground: %+v", vr.Errors)
	}
	store, err := sandbox.OpenMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	eng, err := sandbox.NewEngine(def, sandbox.Config{ID: "sbx_bridge", Seed: "bridge"}, store)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scenario.Run(eng, parsed, nil, "bridge")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestFromDriftPinsBaselineBothDirections(t *testing.T) {
	ev := &alert.DriftEvent{
		SchemaVersion: "1", Fingerprint: "fp_abc123def456",
		Upstream: "widgets", Method: "POST", Endpoint: "/widgets",
		StatusClass: "2xx", Kind: drift.FieldAdded, Field: "fee_bearer", After: "string",
		FirstSeen: time.Now(), LastSeen: time.Now(), Occurrences: 3,
	}
	name, pack, err := Build(ev, 3)
	if err != nil {
		t.Fatal(err)
	}
	if name != "drift-abc123def456" {
		t.Fatalf("pack name wrong: %s", name)
	}

	if res := runAgainst(t, widgetsV1, pack); res.Status != scenario.RunPassed {
		steps, _ := json.MarshalIndent(res.Steps, "", " ")
		t.Fatalf("baseline sandbox must pass: %s (%s)\n%s", res.Status, res.Summary, steps)
	}
	if res := runAgainst(t, widgetsV2, pack); res.Status != scenario.RunFailed {
		steps, _ := json.MarshalIndent(res.Steps, "", " ")
		t.Fatalf("changed sandbox must FAIL the pinned baseline: %s (%s)\n%s", res.Status, res.Summary, steps)
	}
}

func TestFromDriftItemEndpointSeedsProbe(t *testing.T) {
	ev := &alert.DriftEvent{
		Fingerprint: "fp_001122334455", Upstream: "pay", Method: "GET",
		Endpoint: "/charges/{id}", StatusClass: "2xx",
		Kind: drift.FieldRemoved, Field: "fee/currency", Before: "string",
	}
	_, pack, err := Build(ev, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pack["definition"])
	var def struct {
		Steps []struct {
			Type   string         `json:"type"`
			Config map[string]any `json:"config"`
		} `json:"steps"`
	}
	json.Unmarshal(raw, &def)
	if len(def.Steps) != 3 || def.Steps[1].Type != "SEED_STATE" {
		t.Fatalf("expected NOTE, SEED_STATE, REQUEST — got %+v", def.Steps)
	}
	resources := def.Steps[1].Config["resources"].([]any)
	res0 := resources[0].(map[string]any)
	if res0["type"] != "/charges" || res0["resourceKey"] != "drift_probe" {
		t.Fatalf("probe seeding wrong: %+v", res0)
	}
	attrs := res0["attributes"].(map[string]any)
	fee, _ := attrs["fee"].(map[string]any)
	if fee["currency"] != "baseline" {
		t.Fatalf("nested baseline sample missing: %+v", attrs)
	}
	if def.Steps[2].Config["path"] != "/charges/drift_probe" {
		t.Fatalf("request path wrong: %v", def.Steps[2].Config["path"])
	}
}

func TestFromDriftRefusesUnpinnableShapes(t *testing.T) {
	multi := &alert.DriftEvent{Fingerprint: "fp_x", Method: "GET", Endpoint: "/a/{b}/c/{d}", Kind: drift.FieldAdded, Field: "x"}
	if _, _, err := Build(multi, 0); err == nil {
		t.Fatal("multi-param template must refuse loudly")
	}
	nested := &alert.DriftEvent{Fingerprint: "fp_y", Method: "GET", Endpoint: "/a", Kind: drift.FieldAdded, Field: "x[]/y[]/z"}
	if _, _, err := Build(nested, 0); err == nil {
		t.Fatal("nested-array field must refuse loudly")
	}
}

func TestFindEventScansLog(t *testing.T) {
	dir := t.TempDir()
	events := []alert.DriftEvent{
		{Fingerprint: "fp_one", Occurrences: 1, Kind: drift.FieldAdded},
		{Fingerprint: "fp_two", Occurrences: 1, Kind: drift.FieldRemoved},
		{Fingerprint: "fp_one", Occurrences: 5, Kind: drift.FieldAdded},
	}
	f, _ := os.Create(filepath.Join(dir, "events.ndjson"))
	enc := json.NewEncoder(f)
	for _, ev := range events {
		enc.Encode(ev)
	}
	f.Close()

	got, err := FindEvent(dir, "fp_one")
	if err != nil {
		t.Fatal(err)
	}
	if got.Occurrences != 5 {
		t.Fatalf("last matching line must win, got occurrences=%d", got.Occurrences)
	}
	if _, err := FindEvent(dir, "fp_nope"); err == nil {
		t.Fatal("unknown fingerprint must error")
	}
	if _, err := FindEvent(t.TempDir(), "fp_one"); err == nil {
		t.Fatal("missing log must error with guidance")
	}
}
