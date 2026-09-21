package behaviour

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/proxy"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type stubMatcher map[string]bool

func (m stubMatcher) Match(path string) (string, bool) {
	leaf := path[strings.LastIndex(path, "/")+1:]
	return leaf, m[leaf]
}

func rec(path, status string) *proxy.Record {
	return &proxy.Record{Method: "GET", Path: path, Status: 200, RespKind: "json", RespBody: map[string]any{"id": "tok", "status": status, "currency": "NGN"}}
}

func tracker(t *testing.T) *Tracker {
	t.Helper()
	tr := New("pay", t.TempDir(), 3, 0)
	tr.SetClock(func() time.Time { return t0 })
	return tr
}

func edges(tr *Tracker, method, template, field string) map[string]*Edge {
	ep := tr.Snapshot().Endpoints[method+"|"+template]
	if ep == nil || ep.Fields[field] == nil {
		return nil
	}
	return ep.Fields[field].Edges
}

func TestOneResourceChangingValueIsOneEdge(t *testing.T) {
	tr := tracker(t)
	tr.Observe(rec("/charges/ch_0000000001", "pending"))
	tr.Observe(rec("/charges/ch_0000000001", "succeeded"))
	e := edges(tr, "GET", "/charges/ch_{id}", "status")
	if len(e) != 1 || e["pending→succeeded"] == nil || e["pending→succeeded"].Count != 1 {
		t.Fatalf("want one pending→succeeded edge counted once: %+v", e)
	}
	if _, reverse := e["succeeded→pending"]; reverse {
		t.Fatal("the reverse edge was never observed")
	}
}

func TestSameValueTwiceIsNoEdge(t *testing.T) {
	tr := tracker(t)
	tr.Observe(rec("/charges/ch_0000000001", "pending"))
	tr.Observe(rec("/charges/ch_0000000001", "pending"))
	if e := edges(tr, "GET", "/charges/ch_{id}", "status"); len(e) != 0 {
		t.Fatalf("no change, no edge: %+v", e)
	}
}

func TestTwoResourcesSameTransitionCountTwo(t *testing.T) {
	tr := tracker(t)
	for _, id := range []string{"ch_0000000001", "ch_0000000002"} {
		tr.Observe(rec("/charges/"+id, "pending"))
		tr.Observe(rec("/charges/"+id, "succeeded"))
	}
	e := edges(tr, "GET", "/charges/ch_{id}", "status")
	if len(e) != 1 || e["pending→succeeded"].Count != 2 {
		t.Fatalf("want one edge with count 2: %+v", e)
	}
}

func TestDifferentResourcesNeverCrossCorrelate(t *testing.T) {
	tr := tracker(t)
	tr.Observe(rec("/charges/ch_0000000001", "pending"))
	tr.Observe(rec("/charges/ch_0000000002", "succeeded"))
	if e := edges(tr, "GET", "/charges/ch_{id}", "status"); len(e) != 0 {
		t.Fatalf("two resources with different values are not a transition: %+v", e)
	}
}

func TestCollectionEndpointsAreSkipped(t *testing.T) {
	tr := tracker(t)
	tr.Observe(rec("/charges", "pending"))
	tr.Observe(rec("/charges", "succeeded"))
	if len(tr.Snapshot().Endpoints) != 0 {
		t.Fatal("a collection identifies no resource and learns nothing")
	}
}

func TestHighCardinalityLatchesOff(t *testing.T) {
	tr := tracker(t)
	for i := 0; i < maxValuesPerField+5; i++ {
		tr.Observe(&proxy.Record{Method: "GET", Path: "/charges/ch_0000000001", Status: 200, RespKind: "json",
			RespBody: map[string]any{"updated_at": "2026-01-01T00:00:" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "Z"}})
	}
	f := tr.Snapshot().Endpoints["GET|/charges/ch_{id}"].Fields["updated_at"]
	if !f.Latched || len(f.Edges) != 0 || f.Values != nil {
		t.Fatalf("a churning field must latch off and drop its edges: %+v", f)
	}
	tr.Observe(&proxy.Record{Method: "GET", Path: "/charges/ch_0000000001", Status: 200, RespKind: "json", RespBody: map[string]any{"updated_at": "x"}})
	if f := tr.Snapshot().Endpoints["GET|/charges/ch_{id}"].Fields["updated_at"]; len(f.Edges) != 0 {
		t.Fatal("a latched field must not resume")
	}
}

func TestTokenizedAndVolatileAndMutedAreExcluded(t *testing.T) {
	tr := tracker(t)
	tr.SetVolatile(stubMatcher{"currency": true})
	tr.SetMuted(map[string]bool{"/muted/m_{id}": true})
	first := rec("/charges/ch_0000000001", "pending")
	first.Redacted = []proxy.SectionRedaction{{Section: "resp_body", Pointer: "/id", Mode: "TOKENIZE"}}
	second := rec("/charges/ch_0000000001", "succeeded")
	second.RespBody.(map[string]any)["id"] = "tok2"
	second.RespBody.(map[string]any)["currency"] = "USD"
	second.Redacted = first.Redacted
	tr.Observe(first)
	tr.Observe(second)
	ep := tr.Snapshot().Endpoints["GET|/charges/ch_{id}"]
	if ep.Fields["id"] != nil || ep.Fields["currency"] != nil {
		t.Fatalf("tokenized and volatile fields must not be tracked: %v", ep.Fields)
	}
	if len(ep.Fields["status"].Edges) != 1 {
		t.Fatalf("the plain state field still learns: %+v", ep.Fields["status"])
	}
	tr.Observe(rec("/muted/m_0000000001", "a"))
	tr.Observe(rec("/muted/m_0000000001", "b"))
	if tr.Snapshot().Endpoints["GET|/muted/m_{id}"] != nil {
		t.Fatal("a muted template learns nothing")
	}
}

func TestNothingClaimedBeforeBothWarmupGates(t *testing.T) {
	tr := New("pay", t.TempDir(), 3, time.Hour)
	now := t0
	tr.SetClock(func() time.Time { return now })
	tr.Observe(rec("/charges/ch_0000000001", "pending"))
	tr.Observe(rec("/charges/ch_0000000001", "succeeded"))
	tr.Observe(rec("/charges/ch_0000000001", "refunded"))
	rep := BuildReport(tr.Snapshot(), 3, time.Hour, now)
	if len(rep.Fields) != 1 || rep.Fields[0].WarmedUp || rep.Fields[0].NeverObserved != nil {
		t.Fatalf("samples met but age not: nothing may be claimed: %+v", rep.Fields)
	}
	rep = BuildReport(tr.Snapshot(), 3, time.Hour, now.Add(2*time.Hour))
	f := rep.Fields[0]
	if !f.WarmedUp || len(f.NeverObserved) == 0 {
		t.Fatalf("both gates cleared: the absences may be listed: %+v", f)
	}
	for _, n := range f.NeverObserved {
		if n == "pending → succeeded" || n == "succeeded → refunded" {
			t.Fatalf("an observed edge listed as never observed: %v", f.NeverObserved)
		}
	}
	if !contains(f.NeverObserved, "succeeded → pending") || !contains(f.NeverObserved, "refunded → pending") {
		t.Fatalf("reverse edges are the absences: %v", f.NeverObserved)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestPersistedGraphCarriesNoResourceIdentifier(t *testing.T) {
	dir := t.TempDir()
	tr := New("pay", dir, 1, 0)
	tr.SetClock(func() time.Time { return t0 })
	const sentinel = "ch_CANARY7QX9ZK2M4"
	tr.Observe(rec("/charges/"+sentinel, "pending"))
	tr.Observe(rec("/charges/"+sentinel+"?expand=all", "succeeded"))
	tr.Observe(&proxy.Record{Method: "GET", Path: "/charges/" + sentinel, Status: 200, RespKind: "json", RespBody: map[string]any{"status": "refunded", "note": sentinel}})
	if err := tr.Persist(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filePath(dir, "pay"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(sentinel)) {
		t.Fatalf("CANARY LEAK: the resource identifier reached the persisted graph:\n%s", raw)
	}
	var g Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if g.Endpoints["GET|/charges/ch_{id}"].Fields["status"].Edges["pending→succeeded"] == nil {
		t.Fatal("the aggregate graph itself must persist")
	}
	again := New("pay", dir, 1, 0)
	again.SetClock(func() time.Time { return t0 })
	again.Observe(rec("/charges/"+sentinel, "failed"))
	if e := edges(again, "GET", "/charges/ch_{id}", "status"); e["refunded→failed"] != nil {
		t.Fatal("the previous-value cache is memory only; a restart must not continue a resource's history")
	}
}

func TestReportRendersEdgesAndAbsences(t *testing.T) {
	tr := tracker(t)
	for _, id := range []string{"ch_0000000001", "ch_0000000002", "ch_0000000003"} {
		tr.Observe(rec("/charges/"+id, "pending"))
		tr.Observe(rec("/charges/"+id, "succeeded"))
	}
	tr.Observe(rec("/charges/ch_0000000001", "refunded"))
	var b strings.Builder
	BuildReport(tr.Snapshot(), 3, 0, t0).WriteText(&b, t0)
	out := b.String()
	for _, want := range []string{"GET /charges/ch_{id} · status", "pending      → succeeded        3", "succeeded    → refunded         1     ← seen once", "never observed:", "succeeded → pending", "nothing here is enforced"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "forbidden") {
		t.Fatal("absence is never worded as a rule")
	}
}
