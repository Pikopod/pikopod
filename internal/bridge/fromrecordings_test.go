package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/scenario"
)

func trec(method, path string, status int, reqBody, respBody map[string]any) *proxy.Record {
	r := &proxy.Record{TS: time.Now(), Upstream: "pay", Method: method, Path: path,
		Status: status, RespKind: "json", RespBody: anyify(respBody)}
	if reqBody != nil {
		r.ReqKind, r.ReqBody = "json", anyify(reqBody)
	}
	return r
}

// anyify keeps literal map[string]any (records round-trip through JSON in
// production; tests hand plain maps).
func anyify(m map[string]any) any {
	if m == nil {
		return nil
	}
	return map[string]any(m)
}

func TestFromRecordingsProducerConsumerChain(t *testing.T) {
	records := []*proxy.Record{
		trec("POST", "/charges", 201, map[string]any{"amount": float64(500), "currency": "NGN"},
			map[string]any{"data": map[string]any{"id": "ch_a1b2c3d4e5", "status": "pending"}}),
		trec("GET", "/charges/ch_a1b2c3d4e5", 200, nil,
			map[string]any{"data": map[string]any{"id": "ch_a1b2c3d4e5", "status": "success"}}),
		trec("POST", "/refunds", 201, map[string]any{"charge": "ch_a1b2c3d4e5"},
			map[string]any{"data": map[string]any{"id": "rf_z9y8x7w6v5"}}),
	}
	name, pack, err := BuildFromRecordings("pay", records, FromRecordingsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if name != "traffic-pay" {
		t.Fatalf("name: %s", name)
	}

	// The generated definition must pass the REAL parser — a pack that does
	// not validate is a generator bug.
	def, errs := scenario.ParseDefinition(pack["definition"])
	if len(errs) > 0 {
		t.Fatalf("generated pack invalid: %+v", errs)
	}
	if len(def.Steps) != 3 {
		t.Fatalf("steps: %d", len(def.Steps))
	}

	// Step 1 produced ch_…; it must carry the capture.
	s1 := def.Steps[0]
	if len(s1.Capture) != 1 {
		t.Fatalf("producer capture missing: %+v", s1.Capture)
	}
	var varName, expr string
	for k, v := range s1.Capture {
		varName, expr = k, v
	}
	if expr != "response.body$.data.id" {
		t.Fatalf("capture expr: %s", expr)
	}

	// Step 2 consumes it in the PATH; step 3 in the BODY.
	s2cfg := def.Steps[1].Config.(*scenario.RequestConfig)
	if s2cfg.Path != "/charges/{{"+varName+"}}" {
		t.Fatalf("path interpolation: %s", s2cfg.Path)
	}
	s3cfg := def.Steps[2].Config.(*scenario.RequestConfig)
	if body, ok := s3cfg.Body.(map[string]any); !ok || body["charge"] != "{{"+varName+"}}" {
		t.Fatalf("body interpolation: %+v", s3cfg.Body)
	}

	// Status assertions in recorded order.
	for i, want := range []int{201, 200, 201} {
		a := def.Steps[i].Assertions[0]
		if a.Target != "response.status" || a.Op != "equals" {
			t.Fatalf("step %d assertion: %+v", i, a)
		}
		if int(asFloat(t, a.Expected)) != want {
			t.Fatalf("step %d expected %d, got %v", i, want, a.Expected)
		}
	}
}

func asFloat(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	t.Fatalf("not numeric: %T", v)
	return 0
}

// The anti-over-chaining rule: short literals, enum words, amounts and
// booleans NEVER chain, even when repeated verbatim (Keploy's global
// value-equality is the anti-pattern).
func TestFromRecordingsNeverChainsShortLiterals(t *testing.T) {
	records := []*proxy.Record{
		trec("POST", "/charges", 201, map[string]any{"currency": "NGN"},
			map[string]any{"currency": "NGN", "status": "pending", "amount": float64(500)}),
		trec("POST", "/charges", 201, map[string]any{"currency": "NGN", "status": "pending"},
			map[string]any{"ok": true}),
	}
	_, pack, err := BuildFromRecordings("pay", records, FromRecordingsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	def, errs := scenario.ParseDefinition(pack["definition"])
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	for _, s := range def.Steps {
		if len(s.Capture) != 0 {
			t.Fatalf("no captures expected: %+v", s.Capture)
		}
		cfg := s.Config.(*scenario.RequestConfig)
		if strings.Contains(fmtBody(cfg.Body), "{{") {
			t.Fatalf("short literal chained: %+v", cfg.Body)
		}
	}
}

func fmtBody(v any) string {
	if v == nil {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(
		strings.Join(strings.Fields(strings.ReplaceAll(
			fmtAny(v), "\n", " ")), " ")), "  ", " "), "\t", " ")
}

func fmtAny(v any) string {
	switch n := v.(type) {
	case map[string]any:
		out := "{"
		for k, vv := range n {
			out += k + ":" + fmtAny(vv) + " "
		}
		return out + "}"
	case string:
		return n
	default:
		return ""
	}
}

// A value seen in a request BEFORE any response produced it never chains —
// temporal order is part of the proof.
func TestFromRecordingsTemporalOrderRequired(t *testing.T) {
	records := []*proxy.Record{
		// The consumer comes FIRST — the id has no producer yet.
		trec("GET", "/charges/ch_a1b2c3d4e5", 200, nil,
			map[string]any{"data": map[string]any{"id": "ch_a1b2c3d4e5"}}),
	}
	_, pack, _ := BuildFromRecordings("pay", records, FromRecordingsOptions{})
	def, errs := scenario.ParseDefinition(pack["definition"])
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	cfg := def.Steps[0].Config.(*scenario.RequestConfig)
	if strings.Contains(cfg.Path, "{{") {
		t.Fatalf("self-chaining: %s", cfg.Path)
	}
}

func TestFromRecordingsWindowCap(t *testing.T) {
	var records []*proxy.Record
	for i := 0; i < 30; i++ {
		records = append(records, trec("GET", "/health", 200, nil, map[string]any{"ok": true}))
	}
	_, pack, _ := BuildFromRecordings("pay", records, FromRecordingsOptions{Last: 5})
	def, errs := scenario.ParseDefinition(pack["definition"])
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(def.Steps) != 5 {
		t.Fatalf("window cap: %d", len(def.Steps))
	}
}

func TestFromRecordingsChainableGate(t *testing.T) {
	yes := []string{
		"ch_a1b2c3d4e5", "tok_ABC123xyz", "0123456789abcdef",
		"550e8400-e29b-41d4-a716-446655440000", "12345678901",
		"AXJ39dkKD93jdAq29", // long mixed
	}
	no := []string{
		"NGN", "pending", "true", "500", "active", "on_hold",
		"short1", "has space in it", "customer", "12345", // digits too short
	}
	for _, v := range yes {
		if !chainable(v) {
			t.Errorf("should chain: %q", v)
		}
	}
	for _, v := range no {
		if chainable(v) {
			t.Errorf("must NOT chain: %q", v)
		}
	}
}
