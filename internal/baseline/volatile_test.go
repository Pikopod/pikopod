package baseline

import (
	"strings"
	"testing"
	"time"
)

func churnLearner(t *testing.T, m VolatileMatcher) *Learner {
	t.Helper()
	l := newTestLearner(t, 10)
	l.SetVolatileMatcher(m)
	for i := 0; i < 10; i++ {
		l.Observe("GET", "/payments/p1", 200, map[string]any{"id": "p1", "payment": map[string]any{"status": "s" + string(rune('a'+i))}}, t0)
	}
	return l
}

func TestVolatileSuppressesValuesNotPresence(t *testing.T) {
	l := churnLearner(t, stub("status"))
	obs := l.Observe("GET", "/payments/p1", 200, map[string]any{"id": "p1", "payment": map[string]any{}}, t0.Add(time.Second))
	if !obs.Ready {
		t.Fatal("must be frozen")
	}
	ref := obs.Family.Reference["payment/status"]
	if ref == nil || ref.Count != 10 {
		t.Fatalf("payment/status must be in the frozen reference with full presence: %+v", ref)
	}
	if _, present := obs.Fields["payment/status"]; present {
		t.Fatal("the absent field must be absent from the observation so the differ sees a removal")
	}
	if obs.Family.PresenceRatio("payment/status") < 0.9 {
		t.Fatalf("presence ratio must let FieldRemoved fire: %v", obs.Family.PresenceRatio("payment/status"))
	}
}

func TestVolatileSuppressesValuesNotType(t *testing.T) {
	l := churnLearner(t, stub("status"))
	obs := l.Observe("GET", "/payments/p1", 200, map[string]any{"id": "p1", "payment": map[string]any{"status": map[string]any{"code": "x"}}}, t0)
	ref := obs.Family.Reference["payment/status"]
	if ref == nil || ref.Types["string"] != 10 || obs.Fields["payment/status"].Type != "object" {
		t.Fatalf("type stays asserted: ref=%+v obs=%+v", ref, obs.Fields["payment/status"])
	}
}

func TestVolatileValueChurnSilent(t *testing.T) {
	l := churnLearner(t, stub("status"))
	obs := l.Observe("GET", "/payments/p1", 200, map[string]any{"id": "p1", "payment": map[string]any{"status": "brand-new"}}, t0)
	ref := obs.Family.Reference["payment/status"]
	if ref == nil || ref.Values != nil || !ref.HighCardinality {
		t.Fatalf("value tracking must be off so churn produces no enum finding: %+v", ref)
	}
	if !obs.Family.Fields["payment/status"].Churned {
		t.Fatal("churn is still noticed for the collateral check")
	}
}

func TestVolatileDropsRecorded(t *testing.T) {
	m := stub("status")
	churnLearner(t, m)
	if len(m.drops) != 10 {
		t.Fatalf("one drop per suppressed observation: %d", len(m.drops))
	}
	for _, d := range m.drops {
		if d != "payment/status" || strings.Contains(d, "sa") {
			t.Fatalf("drops carry paths only: %q", d)
		}
	}
}

func TestVolatileNoMatcherIsUnchanged(t *testing.T) {
	l := newTestLearner(t, 10)
	for i := 0; i < 10; i++ {
		l.Observe("GET", "/a", 200, map[string]any{"status": "ok"}, t0)
	}
	obs := l.Observe("GET", "/a", 200, map[string]any{"status": "ok"}, t0)
	if st := obs.Family.Reference["status"]; st == nil || st.Values["ok"] != 10 {
		t.Fatalf("without a matcher values are tracked as before: %+v", st)
	}
}

func BenchmarkObserve(b *testing.B) {
	l := NewLearner("prov", b.TempDir(), Warmup{MinSamples: 50})
	body := map[string]any{"id": "p1", "payment": map[string]any{"status": "ACTIVE", "amount": 100.0}, "meta": map[string]any{"ref": "r1", "ts": "x"}}
	for i := 0; i < b.N; i++ {
		l.Observe("GET", "/payments/p1", 200, body, t0)
	}
}

func BenchmarkObserveWithMatcher(b *testing.B) {
	l := NewLearner("prov", b.TempDir(), Warmup{MinSamples: 50})
	l.SetVolatileMatcher(stub("ref", "status"))
	body := map[string]any{"id": "p1", "payment": map[string]any{"status": "ACTIVE", "amount": 100.0}, "meta": map[string]any{"ref": "r1", "ts": "x"}}
	for i := 0; i < b.N; i++ {
		l.Observe("GET", "/payments/p1", 200, body, t0)
	}
}
