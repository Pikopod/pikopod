package baseline

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newTestLearner(t *testing.T, minSamples int) *Learner {
	t.Helper()
	return NewLearner("prov", t.TempDir(), Warmup{MinSamples: minSamples, MinAge: 0})
}

func TestFreezeBeforeAbsorbOrdering(t *testing.T) {
	l := newTestLearner(t, 3)
	warm := map[string]any{"id": "t1", "state": "active"}
	for i := 0; i < 3; i++ {
		if obs := l.Observe("GET", "/things/t1", 200, warm, t0); obs.Ready {
			t.Fatalf("observation %d must be pre-warmup", i)
		}
	}
	crossing := map[string]any{"id": "t1", "state": "active", "sneaky": "new"}
	obs := l.Observe("GET", "/things/t1", 201, crossing, t0.Add(time.Second))
	if !obs.Ready {
		t.Fatal("the crossing record must be diffed (Ready)")
	}
	if _, leaked := obs.Family.Reference["sneaky"]; leaked {
		t.Fatal("the crossing record's own fields leaked into the reference it is diffed against")
	}
	if _, ok := obs.Family.Reference["state"]; !ok {
		t.Fatal("warmup fields missing from the reference")
	}

	if _, leaked := obs.Family.RefStatusCodes["201"]; leaked {
		t.Fatalf("the crossing record's status leaked into RefStatusCodes: %v", obs.Family.RefStatusCodes)
	}
	if obs.Family.RefStatusCodes["200"] != 3 {
		t.Fatalf("frozen code counts wrong: %v", obs.Family.RefStatusCodes)
	}
}

func TestSingleSampleWarmup(t *testing.T) {
	l := newTestLearner(t, 1)
	if obs := l.Observe("GET", "/a", 200, map[string]any{"x": "1"}, t0); obs.Ready {
		t.Fatal("the very first record has nothing to diff against")
	}
	obs := l.Observe("GET", "/a", 200, map[string]any{"x": "1"}, t0)
	if !obs.Ready || obs.Family.Reference["x"] == nil || obs.Family.Reference["x"].Count != 1 {
		t.Fatalf("second record must diff against a one-sample reference: %+v", obs.Family.Reference)
	}
}

func TestWarmupMinAge(t *testing.T) {
	l := NewLearner("prov", t.TempDir(), Warmup{MinSamples: 1, MinAge: time.Hour})
	body := map[string]any{"x": "1"}
	l.Observe("GET", "/a", 200, body, t0)
	if obs := l.Observe("GET", "/a", 200, body, t0.Add(time.Minute)); obs.Ready {
		t.Fatal("samples inside the age window must not freeze")
	}
	if obs := l.Observe("GET", "/a", 200, body, t0.Add(2*time.Hour)); !obs.Ready {
		t.Fatal("past the age window the family must freeze")
	}
}

func TestValueTrackCapLatch(t *testing.T) {
	dir := t.TempDir()
	l := NewLearner("prov", dir, Warmup{MinSamples: 1000, MinAge: 0})
	for i := 0; i < valueTrackCap+1; i++ {
		l.Observe("GET", "/a", 200, map[string]any{"ref": fmt.Sprintf("r_%03d", i)}, t0)
	}
	st := l.Families()[0].Fields["ref"]
	if !st.HighCardinality || st.Values != nil {
		t.Fatalf("cap+1 distinct values must latch and drop the value map: %+v", st)
	}
	if err := l.Persist(); err != nil {
		t.Fatal(err)
	}
	l2 := NewLearner("prov", dir, Warmup{MinSamples: 1000, MinAge: 0})
	l2.Observe("GET", "/a", 200, map[string]any{"ref": "r_fresh"}, t0)
	st2 := l2.Families()[0].Fields["ref"]
	if !st2.HighCardinality || st2.Values != nil {
		t.Fatalf("the latch must survive restart: %+v", st2)
	}

	l3 := newTestLearner(t, 1000)
	for i := 0; i < valueTrackCap; i++ {
		l3.Observe("GET", "/a", 200, map[string]any{"ref": fmt.Sprintf("r_%03d", i)}, t0)
	}
	if st := l3.Families()[0].Fields["ref"]; st.HighCardinality || len(st.Values) != valueTrackCap {
		t.Fatalf("exactly-at-cap must keep tracking values: %+v", st)
	}
}

func TestFieldTrackCap(t *testing.T) {
	l := newTestLearner(t, 100000)
	big := map[string]any{}
	for i := 0; i < fieldTrackCap; i++ {
		big[fmt.Sprintf("k%04d", i)] = "v"
	}
	l.Observe("GET", "/a", 200, big, t0)
	fam := l.Families()[0]
	if len(fam.Fields) != fieldTrackCap {
		t.Fatalf("expected exactly %d tracked fields, got %d", fieldTrackCap, len(fam.Fields))
	}
	big["k_extra"] = "v"
	l.Observe("GET", "/a", 200, big, t0)
	if _, tracked := fam.Fields["k_extra"]; tracked {
		t.Fatal("a fresh path past the cap must be ignored")
	}
	if fam.Fields["k0000"].Count != 2 {
		t.Fatalf("known paths must keep updating past the latch: %+v", fam.Fields["k0000"])
	}
}

func TestPersistLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := NewLearner("prov", dir, Warmup{MinSamples: 2, MinAge: 0})
	body := map[string]any{"id": "t1", "state": "active"}
	for i := 0; i < 3; i++ {
		l.Observe("GET", "/things/t1", 200, body, t0)
	}
	if !l.Families()[0].Frozen {
		t.Fatal("family must be frozen before persisting")
	}
	if err := l.Persist(); err != nil {
		t.Fatal(err)
	}

	l2 := NewLearner("prov", dir, Warmup{MinSamples: 2, MinAge: 0})
	obs := l2.Observe("GET", "/things/t1", 200, body, t0.Add(time.Minute))
	if !obs.Ready {
		t.Fatal("a persisted frozen family must be Ready immediately after load")
	}
	if st := obs.Family.Reference["state"]; st == nil || st.Values["active"] == 0 {
		t.Fatalf("reference values lost across restart: %+v", obs.Family.Reference)
	}
	if obs.Family.RefStatusCodes["200"] == 0 {
		t.Fatalf("frozen status codes lost across restart: %v", obs.Family.RefStatusCodes)
	}
}

func TestEmptyBodyBaseline(t *testing.T) {
	l := newTestLearner(t, 2)
	for i := 0; i < 3; i++ {
		l.Observe("DELETE", "/things/t1", 204, nil, t0)
	}
	fam := l.Families()[0]
	if !fam.Frozen || len(fam.Reference) != 0 {
		t.Fatalf("empty-body family must freeze with an empty reference: frozen=%v ref=%v", fam.Frozen, fam.Reference)
	}
	if r := fam.PresenceRatio("anything"); r != 0 {
		t.Fatalf("presence of an unknown field in an empty reference must be 0, got %v", r)
	}
}

func TestGuardPromotionMergesFamilies(t *testing.T) {
	l := newTestLearner(t, 2)
	body := map[string]any{"name": "x"}

	for i := 0; i < 3; i++ {
		l.Observe("GET", "/users/alphaaa", 200, body, t0)
	}

	for i := 1; i <= 40; i++ {
		seg := fmt.Sprintf("alpha%c%c", 'a'+i/26, 'a'+i%26)
		l.Observe("GET", "/users/"+seg, 200, body, t0)
	}
	var pinned, merged *Family
	for _, f := range l.Families() {
		switch f.Template {
		case "/users/alphaaa":
			pinned = f
		case "/users/{id}":
			merged = f
		default:
			t.Fatalf("unexpected surviving family %q — one-offs must merge", f.Template)
		}
	}
	if pinned == nil || !pinned.Frozen {
		t.Fatal("the frequently-seen literal must survive as its own frozen route")
	}
	if merged == nil {
		t.Fatal("the sprayed one-offs must merge into /users/{id}")
	}
	if merged.Samples < 39 {
		t.Fatalf("merge must sum the one-off samples, got %d", merged.Samples)
	}

	st := merged.Reference["name"]
	if st == nil || st.Count < 30 {
		t.Fatalf("merged reference must be rebuilt from union stats: %+v", st)
	}
}

func TestRefreezeAdoptsLiveBehavior(t *testing.T) {
	l := newTestLearner(t, 2)
	old := map[string]any{"id": "t1", "status": "success"}
	for i := 0; i < 3; i++ {
		l.Observe("GET", "/things/t1", 200, old, t0)
	}
	fam := l.Families()[0]
	if _, inRef := fam.Reference["fee_bearer"]; inRef {
		t.Fatal("setup: reference must predate the drift")
	}

	drifted := map[string]any{"id": "t1", "status": "success", "fee_bearer": "merchant"}
	l.Observe("GET", "/things/t1", 200, drifted, t0)
	if n := l.Refreeze("GET", fam.Template); n != 1 {
		t.Fatalf("refreeze must hit exactly the one frozen family, got %d", n)
	}
	if _, inRef := fam.Reference["fee_bearer"]; !inRef {
		t.Fatal("after refreeze the drifted field IS the baseline")
	}

	l.Observe("GET", "/things/t1", 201, drifted, t0)
	l.Refreeze("GET", fam.Template)
	if fam.RefStatusCodes["201"] == 0 {
		t.Fatal("refreeze must adopt live status codes too")
	}
}

func TestResetScope(t *testing.T) {
	l := newTestLearner(t, 100)
	l.Observe("GET", "/a", 200, map[string]any{"x": "1"}, t0)
	l.Observe("GET", "/b", 200, map[string]any{"x": "1"}, t0)
	if n := l.Reset("/a"); n != 1 {
		t.Fatalf("per-template reset must drop exactly one family, got %d", n)
	}
	if len(l.Families()) != 1 || l.Families()[0].Template != "/b" {
		t.Fatalf("the other family must survive: %+v", l.Families())
	}
	if n := l.Reset(""); n != 1 {
		t.Fatalf("upstream reset must drop the rest, got %d", n)
	}
	if len(l.Families()) != 0 {
		t.Fatal("nothing may survive an upstream reset")
	}
}

func TestFlattenShapes(t *testing.T) {
	fields := Flatten(map[string]any{
		"items": []any{
			map[string]any{"amount": float64(1)},
			map[string]any{"amount": "two"},
		},
		"a/b": "escaped",
	})
	if fields["items"].Type != "array" {
		t.Fatalf("array container missing: %+v", fields)
	}
	if _, shared := fields["items[]/amount"]; !shared {
		t.Fatalf("array elements must share one path: %+v", fields)
	}
	if _, forged := fields["a/b"]; forged {
		t.Fatalf("a slash inside a key must not forge a nested path: %+v", fields)
	}
	if fields["a~1b"].Value != "escaped" {
		t.Fatalf("escaped key lost: %+v", fields)
	}

	if len(Flatten(nil)) != 0 {
		t.Fatal("nil body must flatten to nothing")
	}
}

type leafMatcher struct {
	names map[string]bool
	drops []string
}

func (m *leafMatcher) Match(path string) (string, bool) {
	leaf := lastFieldSegment(path)
	if m.names[strings.ToLower(leaf)] {
		return strings.ToLower(leaf), true
	}
	return "", false
}
func (m *leafMatcher) RecordDrop(path string) { m.drops = append(m.drops, path) }

func stub(names ...string) *leafMatcher {
	m := &leafMatcher{names: map[string]bool{}}
	for _, n := range names {
		m.names[strings.ToLower(n)] = true
	}
	return m
}

func TestVolatileMatchesNestedSegments(t *testing.T) {
	l := newTestLearner(t, 100)
	l.SetVolatileMatcher(stub("Request_Ref"))
	obs := l.Observe("GET", "/a", 200, map[string]any{
		"meta": map[string]any{"request_ref": "r1"},
		"ok":   "yes",
	}, t0)
	if _, tracked := obs.Fields["meta/request_ref"]; !tracked {
		t.Fatalf("a volatile field stays observed: %+v", obs.Fields)
	}
	st := obs.Family.Fields["meta/request_ref"]
	if st == nil || st.Values != nil || !st.HighCardinality || st.Count != 1 {
		t.Fatalf("values suppressed, presence counted: %+v", st)
	}
}

func TestLoadFamiliesFind(t *testing.T) {
	dir := t.TempDir()
	l := NewLearner("prov", dir, Warmup{MinSamples: 1, MinAge: 0})
	for i := 0; i < 2; i++ {
		l.Observe("GET", "/things/t1", 200, map[string]any{"x": "1"}, t0)
	}
	if err := l.Persist(); err != nil {
		t.Fatal(err)
	}
	fams, err := LoadFamilies(dir, "prov")
	if err != nil {
		t.Fatal(err)
	}
	fam := fams.Find("GET", "/things/t1", "2xx")
	if fam == nil || !fam.Frozen {
		t.Fatalf("persisted frozen family must be findable: %+v", fam)
	}
	if fams.Find("GET", "/things/t1", "5xx") != nil {
		t.Fatal("a class never seen must not be found")
	}
}

func TestValueVolatileSuppressesValuesKeepsPresence(t *testing.T) {
	l := NewLearner("pay", t.TempDir(), Warmup{MinSamples: 2, MinAge: 0})
	l.SetValueVolatile([]string{"request_id"})
	ts := time.Now()
	for i := 0; i < 5; i++ {
		l.Observe("GET", "/tx/1", 200, map[string]any{
			"request_id": "rid_" + string(rune('a'+i)),
			"status":     "active",
		}, ts.Add(time.Duration(i)*time.Second))
	}
	fams := l.Families()
	if len(fams) != 1 {
		t.Fatalf("families: %d", len(fams))
	}
	st := fams[0].Fields["request_id"]
	if st == nil {
		t.Fatal("value-volatile field must STILL be learned (presence/type) — dropping it deletes coverage")
	}
	if st.Count != 5 || st.Types["string"] != 5 {
		t.Fatalf("presence/type tracking broken: %+v", st)
	}
	if st.Values != nil || !st.HighCardinality {
		t.Fatalf("values must not be tracked for a curated volatile field: %+v", st)
	}

	if sib := fams[0].Fields["status"]; sib == nil || sib.Values["active"] != 5 {
		t.Fatalf("sibling value tracking broken: %+v", fams[0].Fields["status"])
	}
}

func TestPresenceRatioAllOptionalFamilyDoesNotReadAsAlwaysPresent(t *testing.T) {
	l := newTestLearner(t, 10)
	for i := 0; i < 10; i++ {
		body := map[string]any{"a": "x"}
		if i%2 == 1 {
			body = map[string]any{"b": "y"}
		}
		if obs := l.Observe("GET", "/things", 200, body, t0); obs.Ready {
			t.Fatalf("observation %d must be pre-warmup", i)
		}
	}

	obs := l.Observe("GET", "/things", 200, map[string]any{"a": "x"}, t0.Add(time.Second))
	if !obs.Ready {
		t.Fatal("the crossing record must be diffed (Ready)")
	}
	fam := obs.Family
	if fam.FrozenSamples != 10 {
		t.Fatalf("FrozenSamples = %d, want 10 (samples at freeze time)", fam.FrozenSamples)
	}

	if got := fam.PresenceRatio("a"); got != 0.5 {
		t.Fatalf("PresenceRatio(a) = %v, want 0.5 — a field seen in 5 of 10 samples is not always-present", got)
	}
	if got := fam.PresenceRatio("b"); got != 0.5 {
		t.Fatalf("PresenceRatio(b) = %v, want 0.5", got)
	}
}

func TestPresenceRatioLegacyFrozenBaselineIsNotClaimed(t *testing.T) {
	fam := &Family{
		Method: "GET", Template: "/things", StatusClass: "2xx",
		Samples: 10, Frozen: true, FrozenSamples: 0,
		Fields:    map[string]*FieldStats{"a": {Count: 10}},
		Reference: map[string]*FieldStats{"a": {Count: 10}},
	}
	if got := fam.PresenceRatio("a"); got != 0 {
		t.Fatalf("PresenceRatio on a legacy frozen baseline = %v, want 0 (not claimed, never guessed)", got)
	}
	fam.freeze(t0)
	if fam.FrozenSamples != 10 {
		t.Fatalf("refreeze must restore FrozenSamples, got %d", fam.FrozenSamples)
	}
	if got := fam.PresenceRatio("a"); got != 1 {
		t.Fatalf("after refreeze PresenceRatio(a) = %v, want 1", got)
	}
}
