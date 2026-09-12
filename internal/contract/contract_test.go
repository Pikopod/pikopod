package contract

import (
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/proxy"
)

const specJSON = `{
  "openapi": "3.0.0",
  "info": {"title": "t", "version": "1"},
  "paths": {"/charges": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
    "type": "object",
    "properties": {
      "id": {"type": "string"},
      "amount": {"type": "integer"},
      "status": {"type": "string", "enum": ["success", "failed"]}
    }
  }}}}}}}}
}`

func specDef(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func record(status string, extra map[string]any, redacted ...proxy.SectionRedaction) *proxy.Record {
	body := map[string]any{"id": "tok_abc", "amount": float64(100), "status": status}
	for k, v := range extra {
		body[k] = v
	}
	return &proxy.Record{
		Method: "GET", Path: "/charges", Status: 200, RespKind: "json",
		RespBody: body, Redacted: redacted,
	}
}

func matureRefiner(t *testing.T) *Refiner {
	t.Helper()
	r := NewRefiner("prov", t.TempDir(), 10, 0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return base })
	return r
}

// Undeclared fields join the contract at OBSERVED tier once gated; presence
// becomes confidence.
func TestAdmitUndeclaredField(t *testing.T) {
	r := matureRefiner(t)
	for i := 0; i < 12; i++ {
		r.Observe(record("success", map[string]any{"fee_bearer": "merchant"}))
	}
	n := r.Admit(specDef(t), false)
	if n == 0 {
		t.Fatal("expected admissions")
	}
	eff := ResolveAt(r.Snapshot(), r.Snapshot().Version)
	added := eff.AddedFields["GET|/charges"]
	spec, ok := added["fee_bearer"]
	if !ok {
		t.Fatalf("fee_bearer must be admitted: %+v", added)
	}
	if spec.Provenance.Provenance != ProvenanceObserved {
		t.Fatalf("traffic-derived fields carry OBSERVED, got %s", spec.Provenance.Provenance)
	}
	if spec.Presence < 0.9 {
		t.Fatalf("presence: %v", spec.Presence)
	}
	// Idempotent: re-admitting journals nothing new.
	if again := r.Admit(specDef(t), false); again != 0 {
		t.Fatalf("re-admission must be idempotent, journaled %d", again)
	}
}

// Traffic WINS a sustained type conflict; the spec's claim
// survives in the contradiction record.
func TestTrafficWinsSustainedTypeConflict(t *testing.T) {
	r := matureRefiner(t)
	for i := 0; i < 12; i++ {
		// amount arrives as a STRING in 100% of samples, contradicting the
		// spec's integer claim.
		rec := record("success", nil)
		rec.RespBody.(map[string]any)["amount"] = "100"
		r.Observe(rec)
	}
	r.Admit(specDef(t), false)
	eff := ResolveAt(r.Snapshot(), r.Snapshot().Version)
	if got := eff.TypeOverrides["GET|/charges"]["amount"]; got != "string" {
		t.Fatalf("sustained traffic must win the type conflict, got %q", got)
	}
	found := false
	for _, c := range eff.Contradictions {
		if c.Field == "amount" && c.SpecClaim == "number" && c.Observed == "string" && c.Winner == "traffic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the spec's claim must survive in the contradiction record: %+v", eff.Contradictions)
	}

	// The knob points the other way: prefer_spec gates the override but
	// still records the contradiction.
	r2 := matureRefiner(t)
	for i := 0; i < 12; i++ {
		rec := record("success", nil)
		rec.RespBody.(map[string]any)["amount"] = "100"
		r2.Observe(rec)
	}
	r2.Admit(specDef(t), true)
	eff2 := ResolveAt(r2.Snapshot(), r2.Snapshot().Version)
	if len(eff2.TypeOverrides) != 0 {
		t.Fatal("prefer_spec must gate the override")
	}
	if len(eff2.Contradictions) == 0 || eff2.Contradictions[0].Winner != "spec" {
		t.Fatal("gated conflicts must still be recorded, winner=spec")
	}
}

// An UNSUSTAINED conflict (mixed types) never overrides — spec wins, gated.
func TestUnsustainedConflictDoesNotOverride(t *testing.T) {
	r := matureRefiner(t)
	for i := 0; i < 12; i++ {
		rec := record("success", nil)
		if i%3 == 0 {
			rec.RespBody.(map[string]any)["amount"] = "100" // a third are strings
		}
		r.Observe(rec)
	}
	r.Admit(specDef(t), false)
	eff := ResolveAt(r.Snapshot(), r.Snapshot().Version)
	if len(eff.TypeOverrides) != 0 {
		t.Fatal("66% is not sustained; the spec must hold")
	}
}

// Observed enum values UNION into declared enums after the count floor.
func TestEnumValueUnion(t *testing.T) {
	r := matureRefiner(t)
	for i := 0; i < 12; i++ {
		r.Observe(record("refunded", nil)) // docs declare success|failed only
	}
	r.Admit(specDef(t), false)
	eff := ResolveAt(r.Snapshot(), r.Snapshot().Version)
	vals := eff.ValueUnions["GET|/charges"]["status"]
	if len(vals) != 1 || vals[0] != "refunded" {
		t.Fatalf("observed enum value must union in: %v", vals)
	}
}

// Sanitizer awareness: redacted fields teach PRESENCE but never types or
// values — the refiner must not learn the sanitizer's artifacts.
func TestRedactedFieldsTeachPresenceOnly(t *testing.T) {
	r := matureRefiner(t)
	for i := 0; i < 12; i++ {
		// customer_name was DROPPED at record time: absent from the body,
		// present in the redaction pointers.
		r.Observe(record("success", nil, proxy.SectionRedaction{Section: "resp_body", Pointer: "/customer_name", Mode: "DROP"}))
	}
	r.Admit(specDef(t), false)
	eff := ResolveAt(r.Snapshot(), r.Snapshot().Version)
	spec, ok := eff.AddedFields["GET|/charges"]["customer_name"]
	if !ok {
		t.Fatal("a DROPped field is still PRESENT on the wire and must be admitted")
	}
	if spec.Type != "" {
		t.Fatalf("no type may be learned from a redacted field, got %q", spec.Type)
	}
	snap := r.Snapshot()
	f := snap.Endpoints[endpointKey("GET", "/charges", "2xx")].Fields["customer_name"]
	if len(f.Types) != 0 || len(f.Values) != 0 {
		t.Fatalf("redacted fields must never teach types/values: %+v", f)
	}
	if f.Redactions["DROP"] != 12 {
		t.Fatalf("redaction evidence missing: %+v", f.Redactions)
	}
}

// ResolveAt reproduces history: a pin at version N is untouched by later
// admissions.
func TestResolveAtPinsHistory(t *testing.T) {
	r := matureRefiner(t)
	for i := 0; i < 12; i++ {
		r.Observe(record("success", map[string]any{"fee_bearer": "merchant"}))
	}
	r.Admit(specDef(t), false)
	pinned := r.Snapshot().Version

	for i := 0; i < 12; i++ {
		r.Observe(record("success", map[string]any{"fee_bearer": "merchant", "later_field": "x"}))
	}
	r.Admit(specDef(t), false)
	latest := r.Snapshot().Version
	if latest <= pinned {
		t.Fatal("later admissions must advance the version")
	}

	old := ResolveAt(r.Snapshot(), pinned)
	if _, leaked := old.AddedFields["GET|/charges"]["later_field"]; leaked {
		t.Fatal("ResolveAt(pinned) must not see later admissions — pins are immovable")
	}
	now := ResolveAt(r.Snapshot(), latest)
	if _, ok := now.AddedFields["GET|/charges"]["later_field"]; !ok {
		t.Fatal("ResolveAt(latest) must see the new admission")
	}
}

// Warmup gates: nothing admits before the endpoint matures.
func TestWarmupGatesAdmission(t *testing.T) {
	r := NewRefiner("prov", t.TempDir(), 10, 0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return base })
	for i := 0; i < 5; i++ { // below minSamples
		r.Observe(record("success", map[string]any{"fee_bearer": "merchant"}))
	}
	if n := r.Admit(specDef(t), false); n != 0 {
		t.Fatalf("nothing may admit before warmup, journaled %d", n)
	}
}

// Persistence round-trips the journal and version.
func TestOverlayPersistence(t *testing.T) {
	dir := t.TempDir()
	r := NewRefiner("prov", dir, 10, 0)
	r.SetClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	for i := 0; i < 12; i++ {
		r.Observe(record("success", map[string]any{"fee_bearer": "merchant"}))
	}
	r.Admit(specDef(t), false)
	if err := r.Persist(); err != nil {
		t.Fatal(err)
	}
	r2 := NewRefiner("prov", dir, 10, 0)
	snap := r2.Snapshot()
	if snap.Version == 0 || len(snap.Admissions) == 0 {
		t.Fatalf("overlay must round-trip: %+v", snap)
	}
}
