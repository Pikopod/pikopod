package sandbox

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/proxy"
)

// buildOverlay grows a matured overlay: fee_bearer (undeclared by the
// widgets spec) at ~2/3 presence with two observed values, plus an
// undeclared endpoint /widgets/limits.
func buildOverlay(t *testing.T) *contract.Overlay {
	t.Helper()
	r := contract.NewRefiner("prov", t.TempDir(), 10, 0)
	r.SetClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	for i := 0; i < 12; i++ {
		body := map[string]any{"id": "tok_x", "name": "gizmo", "createdAt": "2026-01-01T00:00:00Z"}
		// fee_bearer: EVERY sample (presence 1.0 → always rendered).
		val := "merchant"
		if i%2 == 0 {
			val = "customer"
		}
		body["fee_bearer"] = val
		// discount: 2/3 of samples (subject to presence-rate omission).
		if i%3 != 0 {
			body["discount"] = float64(50)
		}
		r.Observe(&proxy.Record{Method: "GET", Path: "/widgets/tok_x", Status: 200, RespKind: "json", RespBody: body})
		r.Observe(&proxy.Record{Method: "GET", Path: "/limits", Status: 200, RespKind: "json",
			RespBody: map[string]any{"daily_max": float64(500000), "currency": "ngn"}})
	}
	if r.Admit(loadWidgets(t), false) == 0 {
		t.Fatal("expected admissions")
	}
	return r.Snapshot()
}

// The acceptance property: with the overlay attached, the sandbox returns a
// field the docs NEVER declared — deterministically, with the contract
// version visible on the response.
func TestRenderTrafficAdmittedField(t *testing.T) {
	ov := buildOverlay(t)
	eff := contract.ResolveAt(ov, ov.Version)

	serve := func(id string) (map[string]any, string) {
		e := newEngine(t, loadWidgets(t), Config{ID: "sbx_" + id, Seed: "render-1", Effective: eff})
		srv := httptest.NewServer(e)
		defer srv.Close()
		do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
		got := do(t, e, "GET", "/widgets/widgets_1", "", nil)
		var body map[string]any
		json.Unmarshal([]byte(got.body), &body)
		return body, got.headers[ContractVersionHeader]
	}

	body, version := serve("a")
	fee, ok := body["fee_bearer"]
	if !ok {
		t.Fatalf("the traffic-admitted field must render (spec never declared it): %v", body)
	}
	// Observed VALUES preferred over synthesized ones.
	if fee != "merchant" && fee != "customer" {
		t.Fatalf("observed values must be preferred: %v", fee)
	}
	if version == "" {
		t.Fatalf("responses must carry %s", ContractVersionHeader)
	}
	// Read-your-write untouched.
	if body["name"] != "g" {
		t.Fatalf("stored attributes must never be rewritten: %v", body)
	}

	// Determinism: same seed, same rendering.
	body2, _ := serve("b")
	if body["fee_bearer"] != body2["fee_bearer"] {
		t.Fatalf("rendering must be seed-deterministic: %v vs %v", body["fee_bearer"], body2["fee_bearer"])
	}
}

// Presence-rate omission: a ~2/3-present field must be ABSENT for some
// resources and present for others — seeded, stable per resource.
func TestPresenceRateOmission(t *testing.T) {
	ov := buildOverlay(t)
	eff := contract.ResolveAt(ov, ov.Version)
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_omit", Seed: "omit-1", Effective: eff})

	present, absent := 0, 0
	first := map[string]bool{}
	for i := 0; i < 24; i++ {
		do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
	}
	for i := 1; i <= 24; i++ {
		key := "widgets_" + itoaInt(i)
		got := do(t, e, "GET", "/widgets/"+key, "", nil)
		var body map[string]any
		json.Unmarshal([]byte(got.body), &body)
		_, has := body["discount"]
		first[key] = has
		if has {
			present++
		} else {
			absent++
		}
	}
	if present == 0 || absent == 0 {
		t.Fatalf("a 2/3-present field must be omitted for SOME resources and present for others: present=%d absent=%d", present, absent)
	}
	// Stability: the same resource renders the same either way, every read.
	for i := 1; i <= 24; i++ {
		key := "widgets_" + itoaInt(i)
		got := do(t, e, "GET", "/widgets/"+key, "", nil)
		var body map[string]any
		json.Unmarshal([]byte(got.body), &body)
		if _, has := body["discount"]; has != first[key] {
			t.Fatalf("omission must be stable per resource: %s flipped", key)
		}
	}
}

// Undeclared-but-observed endpoints serve a synthesized body and say so.
func TestServeObservedEndpoint(t *testing.T) {
	ov := buildOverlay(t)
	eff := contract.ResolveAt(ov, ov.Version)
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_obs", Seed: "obs-1", Effective: eff})

	got := do(t, e, "GET", "/limits", "", nil)
	if got.status != 200 {
		t.Fatalf("observed endpoint must serve, got %d", got.status)
	}
	if got.headers["x-pikopod-contract"] != "observed-endpoint" {
		t.Fatalf("a wholly traffic-derived route must be marked: %v", got.headers)
	}
	var body map[string]any
	json.Unmarshal([]byte(got.body), &body)
	if body["currency"] != "ngn" {
		t.Fatalf("observed values must render: %v", body)
	}
	if _, ok := body["daily_max"]; !ok {
		t.Fatalf("observed numeric field missing: %v", body)
	}
	// Shadowing rule: spec routes match FIRST, so a concrete path that a
	// spec template covers (e.g. /widgets/limits under /widgets/{widgetId})
	// is served by the SPEC route, never the observed one — traffic can add
	// routes the spec lacks, but can never hijack declared ones.
	specSide := do(t, e, "GET", "/widgets/limits", "", nil)
	if specSide.headers["x-pikopod-contract"] == "observed-endpoint" {
		t.Fatal("observed endpoints must never shadow spec routes")
	}
}

// A pin resolved at an OLD version never sees later admissions.
func TestRenderRespectsPinnedVersion(t *testing.T) {
	ov := buildOverlay(t)
	effNow := contract.ResolveAt(ov, ov.Version)
	effV0 := contract.ResolveAt(ov, 0)

	eNow := newEngine(t, loadWidgets(t), Config{ID: "sbx_v1", Seed: "pin-1", Effective: effNow})
	eV0 := newEngine(t, loadWidgets(t), Config{ID: "sbx_v0", Seed: "pin-1", Effective: effV0})
	for _, e := range []*Engine{eNow, eV0} {
		do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
	}
	var now, v0 map[string]any
	json.Unmarshal([]byte(do(t, eNow, "GET", "/widgets/widgets_1", "", nil).body), &now)
	json.Unmarshal([]byte(do(t, eV0, "GET", "/widgets/widgets_1", "", nil).body), &v0)
	if _, ok := now["fee_bearer"]; !ok {
		t.Fatal("latest version must render the admission")
	}
	if _, ok := v0["fee_bearer"]; ok {
		t.Fatal("version 0 must NOT render later admissions — pins are immovable")
	}
}

func itoaInt(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
