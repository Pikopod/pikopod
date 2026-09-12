package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/replay"
)

func loadRecordings(t *testing.T, recs []proxy.Record) *replay.Set {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "recordings", "pay.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	set, err := replay.Load(dir, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// The recordings tier answers what neither the spec nor admitted traffic
// can, names its tier on the response, and NEVER shadows a spec route.
func TestRecordingsTierServesUnroutablePaths(t *testing.T) {
	set := loadRecordings(t, []proxy.Record{
		{Method: "GET", Path: "/balance", Status: 200, RespKind: "json",
			RespBody: map[string]any{"available": float64(5000)}},
		{Method: "GET", Path: "/widgets/tok_1", Status: 200, RespKind: "json",
			RespBody: map[string]any{"from": "recording"}},
	})
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_rt", Seed: "rt-1", Recordings: set})

	// Unroutable path → served from recordings, tier named.
	got := do(t, e, "GET", "/balance", "", nil)
	if got.status != 200 {
		t.Fatalf("recorded exchange must serve: %d %s", got.status, got.body)
	}
	if got.headers[ReplayTierHeader] == "" {
		t.Fatalf("recordings-served responses must name their tier: %v", got.headers)
	}
	var body map[string]any
	json.Unmarshal([]byte(got.body), &body)
	if body["available"] != float64(5000) {
		t.Fatalf("recorded body must serve verbatim: %s", got.body)
	}

	// A path the SPEC declares (/widgets/{id}) is never answered from
	// recordings — the spec's own 404 for a missing resource stands.
	specSide := do(t, e, "GET", "/widgets/tok_1", "", nil)
	if specSide.headers[ReplayTierHeader] != "" {
		t.Fatal("recordings must never shadow spec routes")
	}
	if specSide.status == 200 {
		t.Fatalf("missing resource must stay a spec-side miss, got %d %s", specSide.status, specSide.body)
	}
}

// Admitted observed endpoints outrank raw recordings: an admission cleared
// warmup + presence gates; a recording is a single unvetted exchange.
func TestObservedEndpointOutranksRecording(t *testing.T) {
	ov := buildOverlay(t) // admits GET /limits as an observed endpoint
	eff := contract.ResolveAt(ov, ov.Version)
	set := loadRecordings(t, []proxy.Record{
		{Method: "GET", Path: "/limits", Status: 200, RespKind: "json",
			RespBody: map[string]any{"from": "recording"}},
	})
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_rank", Seed: "rank-1", Effective: eff, Recordings: set})

	got := do(t, e, "GET", "/limits", "", nil)
	if got.headers["x-pikopod-contract"] != "observed-endpoint" {
		t.Fatalf("the ADMITTED endpoint must win over the raw recording: %v", got.headers)
	}
	if got.headers[ReplayTierHeader] != "" {
		t.Fatalf("observed-endpoint responses must not claim a replay tier: %v", got.headers)
	}

	// A path only the recordings know still falls through to the tier —
	// with the contract version stamped alongside, one behavioral surface.
	set2 := loadRecordings(t, []proxy.Record{
		{Method: "GET", Path: "/balance", Status: 200, RespKind: "json",
			RespBody: map[string]any{"available": float64(1)}},
	})
	e2 := newEngine(t, loadWidgets(t), Config{ID: "sbx_rank2", Seed: "rank-1", Effective: eff, Recordings: set2})
	rec := do(t, e2, "GET", "/balance", "", nil)
	if rec.headers[ReplayTierHeader] == "" || rec.headers[ContractVersionHeader] == "" {
		t.Fatalf("recordings-tier response must carry both tier and contract version: %v", rec.headers)
	}
}

// A degraded recordings serve names WHY the higher tier missed, and a
// full miss names the closest RECORDED endpoint on the 404.
func TestRecordingsTierMissDiagnostics(t *testing.T) {
	set := loadRecordings(t, []proxy.Record{
		{Method: "POST", Path: "/transfers", Status: 200, RespKind: "json",
			ReqBody:  map[string]any{"amount": float64(100), "currency": "NGN"},
			RespBody: map[string]any{"ok": true}},
	})
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_md", Seed: "md-1", Recordings: set})

	// Same shape, different value → shape tier, missed-on names the field.
	got := do(t, e, "POST", "/transfers", `{"amount":250,"currency":"NGN"}`, map[string]string{"content-type": "application/json"})
	if got.headers[ReplayTierHeader] != string(replay.TierShape) {
		t.Fatalf("tier: %v", got.headers)
	}
	if missed := got.headers[ReplayMissedOnHeader]; !strings.Contains(missed, "amount") || !strings.Contains(missed, "exact missed") {
		t.Fatalf("shape serve must say why exact missed: %q", missed)
	}

	// Different field set → sequence tier, +/- field names.
	got = do(t, e, "POST", "/transfers", `{"amount":100,"reason":"gift"}`, map[string]string{"content-type": "application/json"})
	if got.headers[ReplayTierHeader] != string(replay.TierSequence) {
		t.Fatalf("tier: %v", got.headers)
	}
	if missed := got.headers[ReplayMissedOnHeader]; !strings.Contains(missed, "+reason") || !strings.Contains(missed, "-currency") {
		t.Fatalf("sequence serve must name the field-set gap: %q", missed)
	}

	// Nothing recorded for the endpoint at all → 404 carries the closest
	// RECORDED endpoint.
	got = do(t, e, "POST", "/transfer", "", nil)
	if got.status != 404 {
		t.Fatalf("expected miss, got %d", got.status)
	}
	if closest := got.headers[ReplayClosestHeader]; !strings.Contains(closest, "POST /transfers") || !strings.Contains(closest, "1 recording") {
		t.Fatalf("404 must name the nearest recorded endpoint: %q (headers %v)", closest, got.headers)
	}
}
