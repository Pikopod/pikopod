package sandbox

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/importer"
)

// wallclockEngine: widgets engine served over a REAL TCP listener, so an
// actual net/http client (with its real timeout machinery) is on the wire.
func wallclockServer(t *testing.T) (*Engine, *httptest.Server) {
	t.Helper()
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_wall", Seed: "wall-1"})
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return e, srv
}

// THE claim under test: with a wallclock latency fault armed, a real HTTP
// client with a short timeout ACTUALLY times out — and with the default
// virtualized mode, the same fault costs nothing.
func TestWallclockLatencyTripsRealClientTimeout(t *testing.T) {
	e, srv := wallclockServer(t)
	e.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: "latency", DelayMs: 3000, Probability: 1, Wallclock: true})

	client := &http.Client{Timeout: 250 * time.Millisecond}
	start := time.Now()
	_, err := client.Get(srv.URL + "/widgets")
	if err == nil {
		t.Fatal("a 250ms-timeout client against a 3s wallclock latency fault MUST time out")
	}
	if !strings.Contains(err.Error(), "Client.Timeout") && !strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("expected a client timeout, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("client should give up at ITS timeout, not the fault's: %v", elapsed)
	}

	// Same fault, VIRTUALIZED (default): instant, annotated, deterministic.
	e2 := newEngine(t, loadWidgets(t), Config{ID: "sbx_virt", Seed: "wall-1"})
	srv2 := httptest.NewServer(e2)
	defer srv2.Close()
	e2.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: "latency", DelayMs: 3000, Probability: 1})
	start = time.Now()
	resp, err := client.Get(srv2.URL + "/widgets")
	if err != nil {
		t.Fatalf("virtualized fault must not delay: %v", err)
	}
	defer resp.Body.Close()
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("virtualized latency slept — the default must stay instant")
	}
	if resp.Header.Get(FaultDelayHeader) != "3000" {
		t.Fatalf("virtualized annotation missing: %q", resp.Header.Get(FaultDelayHeader))
	}
}

// hang: hold, then drop the connection with no response.
func TestWallclockHangDropsConnection(t *testing.T) {
	e, srv := wallclockServer(t)
	e.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: "hang", DelayMs: 5000, Probability: 1, Wallclock: true})
	client := &http.Client{Timeout: 250 * time.Millisecond}
	if _, err := client.Get(srv.URL + "/widgets"); err == nil {
		t.Fatal("hang must leave the client without a response")
	}
}

// slow_body: headers arrive immediately; the body dribbles — a client whose
// timeout covers the whole exchange dies mid-read.
func TestWallclockSlowBodyStallsBodyRead(t *testing.T) {
	e, srv := wallclockServer(t)
	e.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: "slow_body", DelayMs: 2000, Probability: 1, Wallclock: true})

	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(srv.URL + "/widgets")
	if err != nil {
		t.Fatalf("headers must arrive promptly (the half-alive shape): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("body read must fail against the dribble under a 300ms budget")
	}
}

// delay-then-error: an error fault with DelayMs and wallclock sleeps first,
// then serves the error — the slow-500 shape.
func TestWallclockDelayThenError(t *testing.T) {
	e, srv := wallclockServer(t)
	e.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: "error", Status: 500, DelayMs: 400, Probability: 1, Wallclock: true})
	start := time.Now()
	resp, err := http.Get(srv.URL + "/widgets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 380*time.Millisecond {
		t.Fatalf("the 500 must arrive AFTER the delay, took %v", elapsed)
	}
}

// The global engine switch: rules WITHOUT the per-rule flag act on the wire
// when Config.WallclockFaults is on (`up --wallclock-faults`).
func TestWallclockGlobalConfig(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_gwall", Seed: "wall-1", WallclockFaults: true})
	srv := httptest.NewServer(e)
	defer srv.Close()
	e.ArmFault(FaultRule{Method: "GET", Path: "/widgets", Kind: "latency", DelayMs: 3000, Probability: 1}) // no per-rule flag
	client := &http.Client{Timeout: 250 * time.Millisecond}
	if _, err := client.Get(srv.URL + "/widgets"); err == nil {
		t.Fatal("global wallclock mode must apply to every fault")
	}
}

// A DRAFT/LLM_EXTRACTED contract still ENFORCES its auth — honesty about
// provenance must not weaken the simulation (the IsGuess/IsUncertain split).
func TestLLMExtractedContractStillEnforces(t *testing.T) {
	def, err := importer.NormalizeLLMExtracted([]byte(runnerLikeLLMSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_llm", Seed: "llm-1"})
	srv := httptest.NewServer(e)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/things")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("LLM-extracted bearer auth must still be enforced: got %d", resp.StatusCode)
	}
}

const runnerLikeLLMSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "x", "version": "1"},
  "security": [{"bearerAuth": []}],
  "paths": {"/things": {"get": {"responses": {"200": {"description": "ok"}}}}},
  "components": {"securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"}}}
}`
