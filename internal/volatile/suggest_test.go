package volatile

import (
	"fmt"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/proxy"
)

func rec(path string, body map[string]any) *proxy.Record {
	return &proxy.Record{
		TS: time.Now(), Upstream: "pay", Method: "GET", Path: path,
		Status: 200, RespKind: "json", RespBody: body,
	}
}

func churnRecords(n int) []*proxy.Record {
	var out []*proxy.Record
	for i := 0; i < n; i++ {
		out = append(out, rec("/tx/1", map[string]any{
			"session_ref": fmt.Sprintf("ref_%d", i),
			"currency":    "NGN",
		}))
	}
	return out
}

func TestSuggestPureChurn(t *testing.T) {
	an := Analyze(churnRecords(20), nil)
	if len(an.Suggestions) != 1 || an.Suggestions[0].Name != "session_ref" {
		t.Fatalf("suggestions: %+v (refusals %+v)", an.Suggestions, an.Refusals)
	}

	for _, r := range an.Refusals {
		if r.Name == "currency" {
			t.Fatalf("stable field misreported: %+v", r)
		}
	}
}

func TestRefuseOverBroad(t *testing.T) {

	var records []*proxy.Record
	for i := 0; i < 20; i++ {
		records = append(records, rec("/tx/1", map[string]any{
			"code": fmt.Sprintf("c_%d", i),
		}))
		records = append(records, rec("/banks", map[string]any{
			"data": map[string]any{"code": "044"},
		}))
	}
	an := Analyze(records, nil)
	if len(an.Suggestions) != 0 {
		t.Fatalf("over-broad name suggested: %+v", an.Suggestions)
	}
	found := false
	for _, r := range an.Refusals {
		if r.Name == "code" && r.Reason == OverBroad {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected OVER_BROAD refusal: %+v", an.Refusals)
	}
}

func TestRefuseStructural(t *testing.T) {
	var records []*proxy.Record
	for i := 0; i < 20; i++ {
		var v any = fmt.Sprintf("s_%d", i)
		if i%2 == 0 {
			v = map[string]any{"nested": i}
		}
		records = append(records, rec("/tx/1", map[string]any{"payload": v}))
	}
	an := Analyze(records, nil)
	for _, r := range an.Refusals {
		if r.Name == "payload" && r.Reason == Structural {
			return
		}
	}
	t.Fatalf("expected STRUCTURAL refusal: %+v suggestions=%+v", an.Refusals, an.Suggestions)
}

func TestRefuseTypeUnstable(t *testing.T) {
	var records []*proxy.Record
	for i := 0; i < 20; i++ {
		var v any = fmt.Sprintf("s_%d", i)
		if i%2 == 0 {
			v = float64(i)
		}
		records = append(records, rec("/tx/1", map[string]any{"amount_raw": v}))
	}
	an := Analyze(records, nil)
	for _, r := range an.Refusals {
		if r.Name == "amount_raw" && r.Reason == TypeUnstable {
			return
		}
	}
	t.Fatalf("expected TYPE_UNSTABLE refusal: %+v", an.Refusals)
}

func TestRefuseInsufficientSamples(t *testing.T) {
	an := Analyze(churnRecords(3), nil)
	if len(an.Suggestions) != 0 {
		t.Fatalf("3 samples must not prove churn: %+v", an.Suggestions)
	}
	for _, r := range an.Refusals {
		if r.Name == "session_ref" && r.Reason == InsufficientSamples {
			return
		}
	}
	t.Fatalf("expected INSUFFICIENT_SAMPLES: %+v", an.Refusals)
}

func TestRefuseRedacted(t *testing.T) {
	records := churnRecords(20)
	for _, r := range records {
		r.Redacted = []proxy.SectionRedaction{{Section: "resp_body", Pointer: "/session_ref", Mode: "TOKENIZE"}}
	}
	an := Analyze(records, nil)
	if len(an.Suggestions) != 0 {
		t.Fatalf("redacted evidence must not suggest: %+v", an.Suggestions)
	}
	for _, r := range an.Refusals {
		if r.Reason == Redacted {
			return
		}
	}
	t.Fatalf("expected REDACTED refusal: %+v", an.Refusals)
}

func TestAlreadyConfiguredSkippedAndLive(t *testing.T) {
	an := Analyze(churnRecords(20), []string{"session_ref"})
	if len(an.Suggestions) != 0 {
		t.Fatalf("configured name re-suggested: %+v", an.Suggestions)
	}
	if len(an.DeadEntries) != 0 {
		t.Fatalf("a matching entry is not dead: %+v", an.DeadEntries)
	}
}

func TestDeadEntryLint(t *testing.T) {
	an := Analyze(churnRecords(20), []string{"ghost_field"})
	if len(an.DeadEntries) != 1 || an.DeadEntries[0].Name != "ghost_field" {
		t.Fatalf("dead entry not flagged: %+v", an.DeadEntries)
	}
}

func TestCuratedListsExactMatchNeverSubstring(t *testing.T) {
	if !IsResponseHeader("date") {
		t.Fatal("date is volatile")
	}
	if IsResponseHeader("candidate-id") || IsResponseField("validated") || IsRequestField("timestamped_thing") {
		t.Fatal("substring leakage")
	}

	if len(responseFields) >= len(requestFields) {
		t.Fatal("response list must stay smaller than the request list — suppressing response coverage deletes assertions")
	}
}
