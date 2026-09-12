package drift

import (
	"fmt"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/baseline"
)

// freezeOn observes `body` at `status` until the family freezes, then
// observes the drifted (status, body) record and returns its Observation —
// the exact pipeline the agent runs, so the tests exercise real freezing
// (including RefStatusCodes snapshots), not hand-built fixtures.
func freezeOn(t *testing.T, warmBody any, warmStatus int, driftBody any, driftStatus int) baseline.Observation {
	t.Helper()
	l := baseline.NewLearner("prov", t.TempDir(), baseline.Warmup{MinSamples: 3, MinAge: 0})
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		l.Observe("GET", "/things/t1", warmStatus, warmBody, ts)
	}
	obs := l.Observe("GET", "/things/t1", driftStatus, driftBody, ts.Add(time.Minute))
	if !obs.Ready {
		t.Fatal("family must be frozen before diffing")
	}
	return obs
}

func classOf(obs baseline.Observation) map[string]bool {
	return map[string]bool{obs.Family.StatusClass: true}
}

func kinds(fs []Finding) map[Kind]int {
	out := map[Kind]int{}
	for _, f := range fs {
		out[f.Kind]++
	}
	return out
}

// A never-null field arriving null is FieldNullable, not TypeChanged — a
// different break (parsers must handle null) with its own fingerprint.
func TestNullabilityIsItsOwnKind(t *testing.T) {
	warm := map[string]any{"id": "t1", "fee": "100"}
	obs := freezeOn(t, warm, 200, map[string]any{"id": "t1", "fee": nil}, 200)
	fs := Diff("prov", obs, classOf(obs))
	if len(fs) != 1 || fs[0].Kind != FieldNullable {
		t.Fatalf("want exactly one field_nullable, got %+v", fs)
	}
	if fs[0].Field != "fee" || fs[0].Before != "string" || fs[0].After != "null" {
		t.Fatalf("nullable finding wrong: %+v", fs[0])
	}
	// Its fingerprint is distinct from the TypeChanged spelling of the same
	// divergence — dedupe must treat them as different events.
	asType := fs[0]
	asType.Kind = TypeChanged
	if fs[0].Fingerprint() == asType.Fingerprint() {
		t.Fatal("field_nullable must not share a fingerprint with type_changed")
	}

	// A baseline that HAS seen null stays silent — nullability was normal.
	l := baseline.NewLearner("prov", t.TempDir(), baseline.Warmup{MinSamples: 4, MinAge: 0})
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		body := map[string]any{"id": "t1", "fee": "100"}
		if i%2 == 0 {
			body["fee"] = nil
		}
		l.Observe("GET", "/things/t1", 200, body, ts)
	}
	obs2 := l.Observe("GET", "/things/t1", 200, map[string]any{"id": "t1", "fee": nil}, ts)
	if fs := Diff("prov", obs2, classOf(obs2)); len(fs) != 0 {
		t.Fatalf("known-nullable field must not alert: %+v", fs)
	}

	// A non-null type swap still reads as TypeChanged.
	obs3 := freezeOn(t, warm, 200, map[string]any{"id": "t1", "fee": float64(100)}, 200)
	fs3 := Diff("prov", obs3, classOf(obs3))
	if len(fs3) != 1 || fs3[0].Kind != TypeChanged {
		t.Fatalf("number swap must stay type_changed: %+v", fs3)
	}
}

// 200→201 inside the same class was invisible to StatusNew; it is now a
// first-class kind, claimed only from the frozen exact-code reference.
func TestExactStatusCodeChange(t *testing.T) {
	body := map[string]any{"id": "t1"}
	obs := freezeOn(t, body, 200, body, 201)
	fs := Diff("prov", obs, classOf(obs))
	if len(fs) != 1 || fs[0].Kind != StatusCodeChanged {
		t.Fatalf("want exactly one status_code_changed, got %+v", fs)
	}
	if fs[0].Before != "200" || fs[0].After != "201" {
		t.Fatalf("code transition wrong: %+v", fs[0])
	}
	// Stable fingerprint across occurrences (a fresh learner reproducing
	// the same divergence), distinct from StatusNew.
	obsRepeat := freezeOn(t, body, 200, body, 201)
	fsRepeat := Diff("prov", obsRepeat, classOf(obsRepeat))
	if len(fsRepeat) != 1 || fsRepeat[0].Fingerprint() != fs[0].Fingerprint() {
		t.Fatalf("the same divergence must share a fingerprint: %+v vs %+v", fsRepeat, fs)
	}
	asNew := fs[0]
	asNew.Kind = StatusNew
	if fs[0].Fingerprint() == asNew.Fingerprint() {
		t.Fatal("status_code_changed must not share a fingerprint with status_new")
	}

	// A known code stays silent.
	obsKnown := freezeOn(t, body, 200, body, 200)
	if fs := Diff("prov", obsKnown, classOf(obsKnown)); len(fs) != 0 {
		t.Fatalf("known code must not alert: %+v", fs)
	}

	// Baselines frozen BEFORE code tracking existed (RefStatusCodes nil)
	// skip the claim instead of guessing.
	obs.Family.RefStatusCodes = nil
	if fs := Diff("prov", obs, classOf(obs)); len(fs) != 0 {
		t.Fatalf("nil code reference must skip, not guess: %+v", fs)
	}
	// So does an observation without an exact code (offline callers).
	obs2 := freezeOn(t, body, 200, body, 201)
	obs2.Status = 0
	if fs := Diff("prov", obs2, classOf(obs2)); len(fs) != 0 {
		t.Fatalf("status 0 must skip the exact-status check: %+v", fs)
	}
}

// A restructured error body (fields added AND removed in one record)
// collapses to ONE error_shape_changed finding — one fingerprint under
// dedupe, not a flood of adds+removes.
func TestErrorShapeChangeCollapses(t *testing.T) {
	warm := map[string]any{"error": map[string]any{"code": "invalid", "message": "bad request"}}
	restructured := map[string]any{"code": "invalid", "message": "bad request"}
	obs := freezeOn(t, warm, 400, restructured, 400)
	fs := Diff("prov", obs, classOf(obs))
	if len(fs) != 1 || fs[0].Kind != ErrorShapeChanged {
		t.Fatalf("restructure must collapse to one error_shape_changed, got %+v", fs)
	}
	if fs[0].Before == fs[0].After || fs[0].Before == "" || fs[0].After == "" {
		t.Fatalf("shape transition must show both sides: %+v", fs[0])
	}
	// Deterministic: the same restructured shape shares the fingerprint.
	obsAgain := freezeOn(t, warm, 400, restructured, 400)
	fsAgain := Diff("prov", obsAgain, classOf(obsAgain))
	if len(fsAgain) != 1 || fsAgain[0].Fingerprint() != fs[0].Fingerprint() {
		t.Fatalf("same restructure must share a fingerprint: %+v vs %+v", fsAgain, fs)
	}

	// A pure extension of the error body stays field_added.
	extended := map[string]any{"error": map[string]any{"code": "invalid", "message": "bad request", "doc_url": "https://x"}}
	obsExt := freezeOn(t, warm, 400, extended, 400)
	fsExt := Diff("prov", obsExt, classOf(obsExt))
	if k := kinds(fsExt); k[FieldAdded] != 1 || k[ErrorShapeChanged] != 0 {
		t.Fatalf("pure extension must stay field_added: %+v", fsExt)
	}

	// 2xx bodies never collapse — success-path field findings keep their
	// individual fingerprints (from-drift pins depend on them).
	warm2xx := map[string]any{"a": "1", "b": "2"}
	obs2xx := freezeOn(t, warm2xx, 200, map[string]any{"a": "1", "c": "3"}, 200)
	fs2xx := Diff("prov", obs2xx, classOf(obs2xx))
	k := kinds(fs2xx)
	if k[ErrorShapeChanged] != 0 || k[FieldAdded] != 1 || k[FieldRemoved] != 1 {
		t.Fatalf("2xx adds+removes must stay individual: %+v", fs2xx)
	}
}

// The offline gate sees exact-status drift too (replay --ci threads the
// record's code through).
func TestOfflineDiffRecordCarriesStatus(t *testing.T) {
	l := baseline.NewLearner("prov", t.TempDir(), baseline.Warmup{MinSamples: 3, MinAge: 0})
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := map[string]any{"id": "t1"}
	for i := 0; i < 4; i++ {
		l.Observe("GET", "/things/t1", 200, body, ts)
	}
	fam := l.Families()[0]
	if !fam.Frozen {
		t.Fatal("family must be frozen")
	}
	fs := DiffRecord(fam, 201, body)
	if len(fs) != 1 || fs[0].Kind != string(StatusCodeChanged) || fs[0].Detail != "201" {
		t.Fatalf("offline gate must catch 200→201: %+v", fs)
	}
	if fs := DiffRecord(fam, 0, body); len(fs) != 0 {
		t.Fatalf("status 0 must skip offline too: %+v", fs)
	}
}

// The classic kinds still behave (this package had no tests before the new
// kinds landed; the old contract is pinned here alongside them).
func TestClassicKindsStillFire(t *testing.T) {
	warm := map[string]any{"id": "t1", "state": "active", "amount": float64(5)}
	obs := freezeOn(t, warm, 200, map[string]any{"id": "t1", "state": "paused", "amount": "5", "extra": true}, 200)
	fs := Diff("prov", obs, classOf(obs))
	k := kinds(fs)
	if k[EnumValueNew] != 1 || k[TypeChanged] != 1 || k[FieldAdded] != 1 {
		t.Fatalf("classic kinds regressed: %+v", fs)
	}
}

// Distinct divergences must never share a fingerprint, even when a field
// name or enum value contains the join delimiter — dedupe keyed on a
// collided fingerprint would suppress one real drift behind another forever.
func TestFingerprintCollisionResistance(t *testing.T) {
	base := Finding{Upstream: "up", Method: "GET", Template: "/t", StatusClass: "2xx", Kind: TypeChanged}
	a, b := base, base
	a.Field, a.Before = "a|b", "x"
	b.Field, b.Before = "a", "b|x"
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("shifted delimiter must not collide")
	}
	c, d := base, base
	c.Field = `a\|b` // literal backslash-pipe vs a pipe that gets escaped
	d.Field = "a|b"
	if c.Fingerprint() == d.Fingerprint() {
		t.Fatal("escape output must not collide with literal escape characters")
	}
	// Same divergence → same fingerprint, and each component is load-bearing.
	if a.Fingerprint() != a.Fingerprint() {
		t.Fatal("fingerprint must be deterministic")
	}
	e := a
	e.StatusClass = "5xx"
	if e.Fingerprint() == a.Fingerprint() {
		t.Fatal("status class must be part of the identity")
	}
}

// The scheme is a persistence contract: alerts/state.json and from-drift
// pins are keyed by these strings across restarts AND upgrades. This pins
// one known fingerprint so any change to the scheme fails loudly here
// instead of silently re-alerting every historical drift.
func TestFingerprintSchemeIsStable(t *testing.T) {
	f := Finding{Upstream: "fakepay", Method: "GET", Template: "/transaction/tx_{id}", StatusClass: "2xx",
		Kind: EnumValueNew, Field: "status", Before: "success", After: "succeeded"}
	const pinned = "fp_385153d1776c"
	if got := f.Fingerprint(); got != pinned {
		t.Fatalf("fingerprint scheme changed: %s != %s — persisted dedupe state and pins will stop matching", got, pinned)
	}
}

// Presence exactly AT the 0.98 floor counts as always-there (>=): a field
// present in 49 of 50 reference samples alerts when missing; 48 of 50 does
// not. The boundary itself is load-bearing — it decides whether an optional
// field can page someone.
func TestPresenceFloorBoundary(t *testing.T) {
	run := func(presentIn int) []Finding {
		l := baseline.NewLearner("prov", t.TempDir(), baseline.Warmup{MinSamples: 50, MinAge: 0})
		ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 50; i++ {
			body := map[string]any{"id": "t1"}
			if i < presentIn {
				body["opt"] = "x"
			}
			l.Observe("GET", "/things/t1", 200, body, ts)
		}
		obs := l.Observe("GET", "/things/t1", 200, map[string]any{"id": "t1"}, ts)
		if !obs.Ready {
			t.Fatal("family must be frozen")
		}
		return Diff("prov", obs, classOf(obs))
	}
	atFloor := run(49) // 49/50 = 0.98 exactly
	if k := kinds(atFloor); k[FieldRemoved] != 1 {
		t.Fatalf("presence exactly at the floor must alert on removal: %+v", atFloor)
	}
	below := run(48) // 0.96
	if k := kinds(below); k[FieldRemoved] != 0 {
		t.Fatalf("below the floor the field is optional — no removal alert: %+v", below)
	}
}

// An empty frozen reference (204-style bodies) treats every observed field
// as added and never fabricates removals.
func TestEmptyReferenceDiff(t *testing.T) {
	l := baseline.NewLearner("prov", t.TempDir(), baseline.Warmup{MinSamples: 2, MinAge: 0})
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		l.Observe("DELETE", "/things/t1", 204, nil, ts)
	}
	obs := l.Observe("DELETE", "/things/t1", 204, map[string]any{"warning": "gone"}, ts)
	fs := Diff("prov", obs, classOf(obs))
	k := kinds(fs)
	if k[FieldAdded] != 1 || len(fs) != 1 {
		t.Fatalf("empty reference: want exactly one field_added, got %+v", fs)
	}
}

// A high-cardinality-latched field stops claiming enum drift but still
// claims TYPE drift — shape stays compared after value tracking stops.
func TestHighCardinalityLatchInDiff(t *testing.T) {
	l := baseline.NewLearner("prov", t.TempDir(), baseline.Warmup{MinSamples: 30, MinAge: 0})
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		l.Observe("GET", "/things/t1", 200, map[string]any{"ref": fmt.Sprintf("r_%03d", i)}, ts)
	}
	obs := l.Observe("GET", "/things/t1", 200, map[string]any{"ref": "r_never_seen"}, ts)
	if !obs.Ready {
		t.Fatal("family must be frozen")
	}
	if fs := Diff("prov", obs, classOf(obs)); len(fs) != 0 {
		t.Fatalf("latched field must not claim enum drift on a fresh value: %+v", fs)
	}
	obs2 := l.Observe("GET", "/things/t1", 200, map[string]any{"ref": float64(7)}, ts)
	fs2 := Diff("prov", obs2, classOf(obs2))
	if k := kinds(fs2); k[TypeChanged] != 1 {
		t.Fatalf("type drift must survive the value latch: %+v", fs2)
	}
}
