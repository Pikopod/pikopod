package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/proxy"
)

func writeRecordings(t *testing.T, dir, upstream string, recs []proxy.Record) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "recordings"), 0o700)
	f, err := os.Create(filepath.Join(dir, "recordings", upstream+".ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range recs {
		enc.Encode(r)
	}
	f.Close()
}

func body(s string) any {
	var v any
	json.Unmarshal([]byte(s), &v)
	return v
}

func TestHierarchicalMatching(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "POST", Path: "/charge", Status: 200, ReqBody: body(`{"amount":100,"currency":"ngn"}`), RespBody: body(`{"id":"tok1","status":"success"}`), RespKind: "json", ReqKind: "json"},
		{Method: "POST", Path: "/charge", Status: 200, ReqBody: body(`{"amount":250,"currency":"ngn"}`), RespBody: body(`{"id":"tok2","status":"success"}`), RespKind: "json", ReqKind: "json"},
		{Method: "GET", Path: "/charge/tx_abc123def456", Status: 200, RespBody: body(`{"id":"tok1","status":"success"}`), RespKind: "json"},
	})
	s, err := Load(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, tier := s.Match("POST", "/charge", []byte(`{"amount":100,"currency":"ngn"}`)); tier != TierExact {
		t.Fatalf("identical fixture body should match exact, got %s", tier)
	}

	if rec, tier := s.Match("POST", "/charge", []byte(`{"amount":999,"currency":"usd"}`)); tier != TierShape || rec == nil {
		t.Fatalf("same-shape body should match shape, got %s", tier)
	}

	if _, tier := s.Match("GET", "/charge/tx_zzz999yyy888", nil); tier == TierMiss {
		t.Fatal("templated path with different id must not miss")
	}

	if _, tier := s.Match("DELETE", "/refunds/r1", nil); tier != TierMiss {
		t.Fatalf("unknown endpoint should miss, got %s", tier)
	}
	if s.Unmatched != 1 || len(s.Report) != 4 {
		t.Fatalf("report bookkeeping wrong: unmatched=%d report=%d", s.Unmatched, len(s.Report))
	}
}

func TestMatchValueServesParsedBodies(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "GET", Path: "/balance", Status: 200, RespBody: body(`{"available":5000}`), RespKind: "json"},
	})
	s, _ := Load(dir, "pay", nil)
	rec, tier := s.MatchValue("GET", "/balance", nil)
	if rec == nil || tier == TierMiss {
		t.Fatalf("recorded exchange must match: %v %s", rec, tier)
	}

	if rec.Status != 200 || fmt.Sprint(rec.RespBody.(map[string]any)["available"]) != "5000" {
		t.Fatalf("recorded response wrong: %+v", rec)
	}
	if miss, tier := s.MatchValue("GET", "/nope", nil); miss != nil || tier != TierMiss {
		t.Fatalf("unknown path must miss (the engine 404s), got %v %s", miss, tier)
	}
}

func TestGateFindsOfflineDrift(t *testing.T) {
	dir := t.TempDir()

	l := baseline.NewLearner("pay", dir, baseline.Warmup{MinSamples: 5, MinAge: 0})
	ts := time.Now()
	for i := 0; i < 8; i++ {
		l.Observe("GET", "/charge/tx_00000000000"+string(rune('a'+i)), 200, body(`{"id":"x1","status":"success","amount":100}`), ts)
	}
	if err := l.Persist(); err != nil {
		t.Fatal(err)
	}

	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "GET", Path: "/charge/tx_aaa111bbb222", Status: 200, RespBody: body(`{"id":"x1","status":"success","amount":100}`), RespKind: "json"},
		{Method: "GET", Path: "/charge/tx_ccc333ddd444", Status: 200, RespBody: body(`{"id":"x1","status":"succeeded","amount":100,"fee_bearer":"merchant"}`), RespKind: "json"},
	})

	res, err := Gate(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 2 {
		t.Fatalf("records=%d", res.Records)
	}
	kinds := map[string]bool{}
	for _, f := range res.Findings {
		kinds[f.Kind] = true
	}
	if !kinds["enum_value_new"] || !kinds["field_added"] {
		t.Fatalf("gate should find enum + field drift, got %+v", res.Findings)
	}

	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "GET", Path: "/charge/tx_eee555fff666", Status: 200, RespBody: body(`{"id":"x1","status":"success","amount":100}`), RespKind: "json"},
	})
	res2, err := Gate(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Findings) != 0 {
		t.Fatalf("clean recordings must gate clean, got %+v", res2.Findings)
	}
}

func TestMatchValueDiag(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "POST", Path: "/transfers", Status: 200, RespKind: "json",
			ReqBody:  map[string]any{"amount": float64(100), "currency": "NGN"},
			RespBody: map[string]any{"ok": true}},
	})
	s, err := Load(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}

	rec, diag := s.MatchValueDiag("POST", "/transfers", map[string]any{"amount": float64(100), "currency": "NGN"})
	if rec == nil || diag.Tier != TierExact || len(diag.MissedOn) != 0 {
		t.Fatalf("exact: %+v", diag)
	}

	s2, _ := Load(dir, "pay", nil)
	_, diag = s2.MatchValueDiag("POST", "/transfers", map[string]any{"amount": float64(250), "currency": "NGN"})
	if diag.Tier != TierShape || len(diag.MissedOn) != 1 || diag.MissedOn[0] != "amount" {
		t.Fatalf("shape gap: %+v", diag)
	}

	s3, _ := Load(dir, "pay", nil)
	_, diag = s3.MatchValueDiag("POST", "/transfers", map[string]any{"amount": float64(100), "reason": "gift"})
	if diag.Tier != TierSequence {
		t.Fatalf("tier: %+v", diag)
	}
	joined := strings.Join(diag.MissedOn, " ")
	if !strings.Contains(joined, "+reason") || !strings.Contains(joined, "-currency") {
		t.Fatalf("sequence gap: %+v", diag)
	}

	rec, diag = s.MatchValueDiag("POST", "/transfer", nil)
	if rec != nil || diag.Tier != TierMiss || diag.Closest != "POST /transfers" || diag.ClosestN != 1 {
		t.Fatalf("miss: %+v", diag)
	}
	closest, n := s.ExplainMiss("POST", "/transfer")
	if closest != "POST /transfers" || n != 1 {
		t.Fatalf("ExplainMiss: %s %d", closest, n)
	}
}

func TestExactTierSequenceVariants(t *testing.T) {
	dir := t.TempDir()
	req := map[string]any{"amount": float64(100)}
	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "POST", Path: "/charge", Status: 503, RespKind: "json", ReqBody: req,
			RespBody: map[string]any{"attempt": float64(1)}},
		{Method: "POST", Path: "/charge", Status: 503, RespKind: "json", ReqBody: req,
			RespBody: map[string]any{"attempt": float64(2)}},
		{Method: "POST", Path: "/charge", Status: 200, RespKind: "json", ReqBody: req,
			RespBody: map[string]any{"attempt": float64(3)}},
	})
	s, err := Load(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []int{503, 503, 200, 200, 200}
	for i, want := range wantStatuses {
		rec, diag := s.MatchValueDiag("POST", "/charge", req)
		if rec == nil || rec.Status != want {
			t.Fatalf("call %d: want %d, got %+v", i+1, want, rec)
		}
		if diag.Tier != TierExact || diag.SeqLen != 3 {
			t.Fatalf("call %d: diag %+v", i+1, diag)
		}
		if i < 3 && (diag.SeqPos != i+1 || diag.Held) {
			t.Fatalf("call %d: position %+v", i+1, diag)
		}
		if i >= 3 && (!diag.Held || diag.SeqPos != 3) {
			t.Fatalf("call %d: must hold the last: %+v", i+1, diag)
		}
	}

	other := map[string]any{"amount": float64(999)}
	if rec, _ := s.MatchValueDiag("POST", "/charge", other); rec == nil || rec.RespBody.(map[string]any)["attempt"] != float64(1) {

		if rec == nil {
			t.Fatal("different body on a recorded endpoint must still serve")
		}
	}
}

func TestExactTierSingleRecordingNoSequence(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "GET", Path: "/one", Status: 200, RespKind: "json", RespBody: map[string]any{"a": true}},
	})
	s, _ := Load(dir, "pay", nil)
	_, diag := s.MatchValueDiag("GET", "/one", nil)
	if diag.SeqLen != 1 || diag.Held {
		t.Fatalf("single recording: %+v", diag)
	}

	rec, diag := s.MatchValueDiag("GET", "/one", nil)
	if rec == nil || diag.Tier != TierExact || !diag.Held {
		t.Fatalf("re-request must hold: %+v", diag)
	}
}

func TestExactKeyNumberCanonicalization(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "pay", []proxy.Record{
		{Method: "POST", Path: "/charge", Status: 200, RespKind: "json",
			ReqBody:  body(`{"amount":500.00,"currency":"NGN"}`),
			RespBody: map[string]any{"ok": true}},
	})
	s, err := Load(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}

	rec, diag := s.MatchValueDiag("POST", "/charge", map[string]any{"amount": float64(500), "currency": "NGN"})
	if rec == nil || diag.Tier != TierExact {
		t.Fatalf("cosmetic number formatting must not break exact matching: %+v", diag)
	}

	a := canonicalJSON(map[string]any{"n": json.Number("90071992547409934")})
	b := canonicalJSON(map[string]any{"n": json.Number("90071992547409935")})
	if a == b {
		t.Fatal(">2^53 literals must not collapse through float64")
	}
}
