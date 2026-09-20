package sandbox

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Times windows: fail the first N matching requests, then RECOVER —
// deterministic (no roll), per-key isolated, identical across engines.
func TestFaultTimesWindow(t *testing.T) {
	arm := func(e *Engine, per string) {
		e.ArmFault(FaultRule{Method: "POST", Path: "/widgets", Kind: "error", Status: 503,
			Probability: 1, Times: 2, Per: per})
	}
	statuses := func(e *Engine, headers map[string]string, n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = do(t, e, "POST", "/widgets", `{"name":"g"}`, headers).status
		}
		return out
	}

	// Global window: exactly the first 2 requests fail, ever.
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_tw", Seed: "tw-1"})
	arm(e, "global")
	got := statuses(e, nil, 4)
	want := []int{503, 503, 201, 201}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("global window wrong at %d: got %v want %v", i, got, want)
		}
	}

	// Per idempotency key: each key gets its own fail-twice window.
	e2 := newEngine(t, loadWidgets(t), Config{ID: "sbx_tw2", Seed: "tw-1"})
	arm(e2, "idempotency-key")
	a := map[string]string{"Idempotency-Key": "key-a"}
	b := map[string]string{"Idempotency-Key": "key-b"}
	if s := statuses(e2, a, 3); s[0] != 503 || s[1] != 503 || s[2] != 201 {
		t.Fatalf("key-a window wrong: %v", s)
	}
	// key-b's window is untouched by key-a's consumption.
	if s := statuses(e2, b, 3); s[0] != 503 || s[1] != 503 || s[2] != 201 {
		t.Fatalf("key-b must have its OWN window: %v", s)
	}

	// Determinism: a fresh engine with the same seed replays the same window.
	e3 := newEngine(t, loadWidgets(t), Config{ID: "sbx_tw3", Seed: "tw-1"})
	arm(e3, "global")
	if s := statuses(e3, nil, 4); s[0] != 503 || s[2] != 201 {
		t.Fatalf("windows must replay identically across engines: %v", s)
	}
}

// Transport faults act on the REAL wire in wallclock mode: a genuine
// net/http client sees connection errors and truncated bodies, not clean
// HTTP failures.
func TestTransportFaultsOnTheWire(t *testing.T) {
	serve := func(kind string) (*http.Response, error) {
		e := newEngine(t, loadWidgets(t), Config{ID: "sbx_" + kind, Seed: "wire-1", WallclockFaults: true})
		e.ArmFault(FaultRule{Method: "POST", Path: "/widgets", Kind: kind, Probability: 1})
		srv := httptest.NewServer(e)
		t.Cleanup(srv.Close)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(srv.URL+"/widgets", "application/json", strings.NewReader(`{"name":"g"}`))
		if resp != nil {
			t.Cleanup(func() { resp.Body.Close() })
		}
		return resp, err
	}

	if _, err := serve(FaultConnectionReset); err == nil {
		t.Fatal("connection_reset must surface as a client-side connection error")
	}

	if resp, err := serve(FaultMalformedResponse); err == nil {
		// Some transports parse the valid status line then choke on the body.
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil && !strings.Contains(string(body), "lskdu") {
			t.Fatalf("malformed_response must not produce a clean response: %q", body)
		}
	}

	resp, err := serve(FaultWrongContentLength)
	if err != nil {
		t.Fatalf("wrong_content_length must deliver headers: %v", err)
	}
	if _, readErr := io.ReadAll(resp.Body); !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("declared-longer body must end in unexpected EOF, got %v", readErr)
	}

	serveTimed := func(kind string, delayMs int64) (time.Duration, error) {
		e := newEngine(t, loadWidgets(t), Config{ID: "sbx_timed_" + kind, Seed: "wire-1", WallclockFaults: true})
		e.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: kind, DelayMs: delayMs, Probability: 1})
		srv := httptest.NewServer(e)
		t.Cleanup(srv.Close)
		start := time.Now()
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(srv.URL + "/widgets")
		if resp != nil {
			resp.Body.Close()
		}
		return time.Since(start), err
	}

	hangElapsed, hangErr := serveTimed("hang", 300)
	if hangErr == nil || hangElapsed < 250*time.Millisecond {
		t.Fatalf("hang must hold the connection before closing: elapsed=%v err=%v", hangElapsed, hangErr)
	}
	for _, kind := range []string{FaultEmptyResponse, FaultRandomDataThenClose} {
		elapsed, err := serveTimed(kind, 300)
		if err == nil {
			t.Fatalf("%s must fail before an HTTP response is parsed", kind)
		}
		if elapsed >= 250*time.Millisecond {
			t.Fatalf("%s must close immediately rather than behave like hang: elapsed=%v", kind, elapsed)
		}
	}
}

// Virtualized mode (the default): transport kinds degrade to header
// annotation — deterministic, transcript-stable, no wire damage.
func TestTransportFaultsVirtualizedAnnotation(t *testing.T) {
	for _, kind := range []string{FaultConnectionReset, FaultEmptyResponse, FaultRandomDataThenClose} {
		e := newEngine(t, loadWidgets(t), Config{ID: "sbx_virt_" + kind, Seed: "virt-1"})
		e.ArmFault(FaultRule{Method: "POST", Path: "/widgets", Kind: kind, Probability: 1})
		got := do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
		if got.status != 201 {
			t.Fatalf("virtualized transport fault must not break the response: %s: %d", kind, got.status)
		}
		if !strings.Contains(got.headers[FaultAppliedHeader], kind) {
			t.Fatalf("the would-be fault must be annotated: %s: %v", kind, got.headers)
		}
	}
}

// Seeded delay distributions: reproducible jitter — same seed and request
// identity, same sample; bounds respected.
func TestDelayDistributionsDeterministic(t *testing.T) {
	sample := func(seed string, d *DelayDistribution) int64 {
		e := newEngine(t, loadWidgets(t), Config{ID: "sbx_dd_" + seed, Seed: seed})
		e.ArmFault(FaultRule{Method: "POST", Path: "/widgets", Kind: "latency", Probability: 1, Delay: d})
		got := do(t, e, "POST", "/widgets", `{"name":"g"}`, nil)
		var ms int64
		if v := got.headers[FaultDelayHeader]; v != "" {
			for _, c := range v {
				ms = ms*10 + int64(c-'0')
			}
		}
		return ms
	}
	logn := &DelayDistribution{Type: "lognormal", MedianMs: 200, Sigma: 0.5, MaxMs: 2000}
	a, b := sample("dd-1", logn), sample("dd-1", logn)
	if a != b {
		t.Fatalf("same seed must sample the same delay: %d vs %d", a, b)
	}
	if a <= 0 || a > 2000 {
		t.Fatalf("lognormal sample out of bounds: %d", a)
	}
	if c := sample("dd-2", logn); c == a {
		t.Logf("note: different seeds coincided (%d) — legal but unlikely", c)
	}

	uni := &DelayDistribution{Type: "uniform", LowerMs: 100, UpperMs: 110}
	if u := sample("dd-3", uni); u < 100 || u > 110 {
		t.Fatalf("uniform sample out of range: %d", u)
	}
	band := &DelayDistribution{Type: "band", BaseMs: 1000, Pct: 10}
	if v := sample("dd-4", band); v < 900 || v > 1100 {
		t.Fatalf("band sample outside ±10%%: %d", v)
	}
}

// Lognormal max: resample-then-clamp — a tiny max with a huge sigma always
// lands AT or under max, never above.
func TestLognormalMaxClamp(t *testing.T) {
	d := &DelayDistribution{Type: "lognormal", MedianMs: 1000, Sigma: 5, MaxMs: 50}
	for i := 0; i < 50; i++ {
		prng := NewPrng("clamp-" + string(rune('a'+i)))
		if v := d.sample(prng); v > 50 {
			t.Fatalf("sample %d exceeded max: %d", i, v)
		}
	}
}
