package volatile

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/proxy"
)

func TestCompileRefusesMalformedEntry(t *testing.T) {
	for _, bad := range []string{"", "/status", "status/", "a//b", "sta tus", "meta.status/{x}"} {
		m, refusals, err := Compile([]string{bad})
		var e *errfmt.E
		if err == nil || !errors.As(err, &e) || m != nil {
			t.Fatalf("%q: want an errfmt refusal, got m=%v err=%v", bad, m, err)
		}
		if len(refusals) != 1 || refusals[0].Reason != Malformed {
			t.Fatalf("%q: want one MALFORMED refusal, got %+v", bad, refusals)
		}
	}
}

func TestCompileAlreadyConfiguredEmitted(t *testing.T) {
	m, refusals, err := Compile([]string{"status", "Status", "meta/status"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 || refusals[0].Reason != AlreadyConfigured {
		t.Fatalf("a duplicate entry is ALREADY_CONFIGURED: %+v", refusals)
	}
	if got := m.Entries(); !reflect.DeepEqual(got, []string{"status", "meta/status"}) {
		t.Fatalf("entries: %v", got)
	}
}

func TestMatchBareAndScoped(t *testing.T) {
	m, _, _ := Compile([]string{"status", "meta/ref", "items[]/code"})
	cases := map[string]string{
		"status": "status", "payment/status": "status", "a/b[]/status": "status", "Payment/STATUS": "status",
		"meta/ref": "meta/ref", "data/meta/ref": "meta/ref", "ref": "", "xmeta/ref": "", "meta/ref/x": "",
		"items[]/code": "items[]/code", "data/items[]/code": "items[]/code", "code": "", "items/code": "",
	}
	for path, want := range cases {
		entry, ok := m.Match(path)
		if (want != "") != ok || entry != want {
			t.Errorf("Match(%q) = %q,%v; want %q", path, entry, ok, want)
		}
	}
	var nilM *Matcher
	if _, ok := nilM.Match("status"); ok {
		t.Fatal("a nil matcher matches nothing")
	}
}

func TestMatchAllocatesNothing(t *testing.T) {
	m, _, _ := Compile([]string{"status", "meta/ref"})
	if n := testing.AllocsPerRun(1000, func() { m.Match("payment/Meta/ref") }); n != 0 {
		t.Fatalf("Match allocated %.0f times per call", n)
	}
}

func TestDropsRecordPathsOnly(t *testing.T) {
	m, _, _ := CompileSets([]string{"status"}, []string{"request_id"})
	m.RecordDrop("payment/status")
	m.RecordDrop("payment/status")
	m.RecordDrop("meta/request_id")
	m.RecordDrop("unrelated")
	want := []Drop{{Entry: "curated:request_id", Path: "meta/request_id", Count: 1}, {Entry: "status", Path: "payment/status", Count: 2}}
	if got := m.Drops(); !reflect.DeepEqual(got, want) {
		t.Fatalf("drops:\n got %+v\nwant %+v", got, want)
	}
}

func learnerWith(t *testing.T, entries []string, records int, mutate func(i int, body map[string]any)) (*baseline.Learner, *Matcher) {
	t.Helper()
	m, _, err := Compile(entries)
	if err != nil {
		t.Fatal(err)
	}
	l := baseline.NewLearner("pay", t.TempDir(), baseline.Warmup{MinSamples: 10})
	l.SetVolatileMatcher(m)
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < records; i++ {
		body := map[string]any{"id": "p1", "payment": map[string]any{"status": "ACTIVE"}, "meta": map[string]any{"status": "s" + string(rune('a'+i%26))}}
		if mutate != nil {
			mutate(i, body)
		}
		l.Observe("GET", "/payments/p1", 200, body, ts)
	}
	return l, m
}

func TestVolatilePathScoped(t *testing.T) {
	l, _ := learnerWith(t, []string{"meta/status"}, 12, nil)
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	obs := l.Observe("GET", "/payments/p1", 200, map[string]any{"id": "p1", "payment": map[string]any{"status": "SUSPENDED"}, "meta": map[string]any{"status": "zz"}}, ts)
	if !obs.Ready {
		t.Fatal("must be frozen")
	}
	kinds := map[string]string{}
	for _, f := range drift.Diff("pay", obs, map[string]bool{"2xx": true}) {
		kinds[f.Field] = string(f.Kind)
	}
	if kinds["payment/status"] != string(drift.EnumValueNew) {
		t.Fatalf("payment/status must keep its value assertion: %v", kinds)
	}
	if _, silenced := kinds["meta/status"]; silenced {
		t.Fatalf("meta/status must be silenced by the scoped entry: %v", kinds)
	}
}

func TestCollateralOverBroad(t *testing.T) {
	l, m := learnerWith(t, []string{"status"}, 12, nil)
	got := Collateral(l, m)
	if len(got) != 1 || got[0].Reason != OverBroad || got[0].Path != "payment/status" || got[0].Name != "status" {
		t.Fatalf("a bare name covering a stable field beside a churning one is OVER_BROAD naming the stable path: %+v", got)
	}
}

func TestCollateralStableEmitted(t *testing.T) {
	l, m := learnerWith(t, []string{"payment/status"}, 12, nil)
	got := Collateral(l, m)
	if len(got) != 1 || got[0].Reason != Stable || got[0].Path != "payment/status" {
		t.Fatalf("an entry over a field that never changed is STABLE: %+v", got)
	}
}

func TestCollateralDeadEntry(t *testing.T) {
	l, m := learnerWith(t, []string{"nothing_here"}, 12, nil)
	got := Collateral(l, m)
	if len(got) != 1 || got[0].Reason != Dead || got[0].Name != "nothing_here" {
		t.Fatalf("an entry matching nothing is DEAD_ENTRY: %+v", got)
	}
}

func TestVolatileAcceptanceAgainstRealDiffer(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	train := func(entries []string) *baseline.Learner {
		m, _, err := Compile(entries)
		if err != nil {
			t.Fatal(err)
		}
		l := baseline.NewLearner("pay", t.TempDir(), baseline.Warmup{MinSamples: 5})
		l.SetVolatileMatcher(m)
		for i := 0; i < 6; i++ {
			l.Observe("GET", "/charges/c1", 200, map[string]any{"id": "c1", "status": "success", "meta": map[string]any{"ref": "r" + string(rune('a'+i))}, "amount": 100.0}, ts)
		}
		return l
	}
	records := []*proxy.Record{
		{Method: "GET", Path: "/charges/c1", Status: 200, RespKind: "json", RespBody: map[string]any{"id": "c1", "status": "succeeded", "meta": map[string]any{"ref": "rz"}, "amount": 100.0, "fee": 3.0}},
		{Method: "GET", Path: "/charges/c1", Status: 200, RespKind: "json", RespBody: map[string]any{"id": "c1", "status": "success", "meta": map[string]any{}, "amount": "100"}},
	}
	run := func(l *baseline.Learner, m *Matcher) []drift.OfflineFinding {
		var out []drift.OfflineFinding
		for _, rec := range records {
			fam := onlyFamily(t, l)
			for _, f := range drift.DiffRecord(fam, rec.Status, rec.RespBody) {
				if _, drop := m.Match(f.Field); drop && f.Kind == string(drift.EnumValueNew) {
					continue
				}
				out = append(out, f)
			}
		}
		return out
	}
	none, _, _ := Compile(nil)
	without := run(train(nil), none)
	entry, _, _ := Compile([]string{"meta/ref"})
	with := run(train([]string{"meta/ref"}), entry)

	var wantSilenced, kept []drift.OfflineFinding
	for _, f := range without {
		if f.Field == "meta/ref" && f.Kind == string(drift.EnumValueNew) {
			wantSilenced = append(wantSilenced, f)
		} else {
			kept = append(kept, f)
		}
	}
	if len(wantSilenced) == 0 {
		t.Fatalf("fixture must produce the finding the entry is meant to silence: %+v", without)
	}
	if !reflect.DeepEqual(with, kept) {
		t.Fatalf("only the silenced finding may differ:\n with   %+v\n expect %+v", with, kept)
	}
	found := map[string]bool{}
	for _, f := range with {
		found[f.Field+"|"+f.Kind] = true
	}
	if !found["meta/ref|field_removed"] || !found["status|enum_value_new"] || !found["fee|field_added"] || !found["amount|type_changed"] {
		t.Fatalf("presence, type and neighbouring findings must survive: %+v", with)
	}
}

func onlyFamily(t *testing.T, l *baseline.Learner) *baseline.Family {
	t.Helper()
	fams := l.Families()
	if len(fams) != 1 || !fams[0].Frozen {
		t.Fatalf("fixture must yield one frozen family: %d", len(fams))
	}
	return fams[0]
}

func TestSuggestReportsAlreadyConfigured(t *testing.T) {
	an := Analyze(churnRecords(20), []string{"session_ref"})
	seen := false
	for _, r := range an.Refusals {
		if r.Reason == AlreadyConfigured && r.Name == "session_ref" {
			seen = true
		}
	}
	if !seen || strings.Contains(strings.Join(names(an), ","), "session_ref") {
		t.Fatalf("a configured name is reported ALREADY_CONFIGURED and not re-suggested: %+v %+v", an.Refusals, an.Suggestions)
	}
}

func names(an *Analysis) []string {
	var out []string
	for _, s := range an.Suggestions {
		out = append(out, s.Name)
	}
	return out
}
