package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
)

// incidentAgent fronts a scripted upstream. Sampling is pinned to 0 so every
// test also proves incidents survive the sampling gate.
func incidentAgent(t *testing.T, target string, tune func(*config.Upstream)) (*Agent, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	zero := 0.0
	up := config.Upstream{Listen: "/examplepay", Target: target}
	if tune != nil {
		tune(&up)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1", AgentPort: 0, DataDir: dir,
		Upstreams: map[string]config.Upstream{"examplepay": up},
		Warmup:    config.Warmup{MinSamples: 15, MinHours: new(int)},
		Sampling:  config.Sampling{Rate: &zero},
	}
	a, err := New(cfg, alert.Options{MinOccurrences: 1, Window: time.Minute}, newCaptureSink())
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(a.Proxy)
	t.Cleanup(front.Close)
	startPipeline(t, a)
	return a, front, dir
}

func scriptedUpstream(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func readEvents(t *testing.T, dir string) []alert.DriftEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		return nil
	}
	var out []alert.DriftEvent
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev alert.DriftEvent
		if json.Unmarshal([]byte(line), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

// drained blocks until the recorder has finished observing want records.
// Report, and the synchronous event-log append inside it, both complete
// before either counter moves, so the log is readable once this returns.
func drained(t *testing.T, a *Agent, want int64) {
	t.Helper()
	waitFor(t, func() bool {
		return a.Metrics.RecordingsWritten.Load()+a.Metrics.RecordingsSampledOut.Load() >= want
	})
}

// eventsOfKind waits until at least one event of a kind is on disk. It must
// NOT call Alerter.Flush: that waits on the delivery WaitGroup while the
// recorder goroutine can still Add to it, which is a race, and the event log
// is written synchronously anyway.
func eventsOfKind(t *testing.T, a *Agent, kind string) []alert.DriftEvent {
	t.Helper()
	var got []alert.DriftEvent
	waitFor(t, func() bool {
		got = nil
		for _, ev := range readEvents(t, a.Cfg.DataDir) {
			if string(ev.Kind) == kind {
				got = append(got, ev)
			}
		}
		return len(got) > 0
	})
	return got
}

func post(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// A 503 must become an incident on the FIRST request, with no warmup and no
// frozen family. Gating incidents on obs.Ready would reintroduce the 48-hour
// blind window on the one path that must never have it.
func TestIncidentFiresOnFirstRequestWithoutWarmup(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"upstream down"}`)
	})
	a, front, _ := incidentAgent(t, target, nil)

	post(t, front.URL+"/examplepay/charges", `{"amount":5000}`)

	evs := eventsOfKind(t, a, "upstream_error")
	if evs[0].After != "503" {
		t.Fatalf("After = %q, want %q — a 500 and a 503 must not share a fingerprint", evs[0].After, "503")
	}
	if evs[0].Level != "ERR" {
		t.Fatalf("Level = %q, want ERR", evs[0].Level)
	}
	if evs[0].SchemaVersion != alert.SchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", evs[0].SchemaVersion, alert.SchemaVersion)
	}
}

// The recording IS the reproduction. An incident must force the record past the
// sampling gate, or `scenario from-recording` has nothing to read.
func TestIncidentRecordSurvivesZeroSampling(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"upstream down"}`)
	})
	a, front, dir := incidentAgent(t, target, nil)

	post(t, front.URL+"/examplepay/charges", `{"amount":5000}`)
	eventsOfKind(t, a, "upstream_error")

	var raw []byte
	waitFor(t, func() bool {
		raw, _ = os.ReadFile(filepath.Join(dir, "recordings", "examplepay.ndjson"))
		return len(raw) > 0
	})
	if !strings.Contains(string(raw), `"status":503`) {
		t.Fatalf("incident record did not reach disk at sampling rate 0:\n%s", raw)
	}
}

// Concrete identifiers must never egress; the event carries a path template.
func TestIncidentEventCarriesTemplateNotConcretePath(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	a, front, _ := incidentAgent(t, target, nil)

	for i := 0; i < 3; i++ {
		resp, err := http.Get(fmt.Sprintf("%s/examplepay/charges/ch_live_%d", front.URL, i))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	for _, ev := range eventsOfKind(t, a, "upstream_error") {
		if strings.Contains(ev.Endpoint, "ch_live_") {
			t.Fatalf("concrete identifier egressed in endpoint %q", ev.Endpoint)
		}
	}
}

// A storm on one endpoint is one fingerprint with a rising count, not N events.
func TestIncidentStormIsOneFingerprint(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	})
	a, front, _ := incidentAgent(t, target, nil)

	for i := 0; i < 25; i++ {
		post(t, front.URL+"/examplepay/charges", `{}`)
	}
	fps := map[string]bool{}
	for _, ev := range eventsOfKind(t, a, "upstream_error") {
		fps[ev.Fingerprint] = true
	}
	if len(fps) != 1 {
		t.Fatalf("25 identical failures produced %d fingerprints, want 1", len(fps))
	}
}

// 4xx is opt-in: usually our own bug, and routine wherever a 401 is normal.
func TestClientErrorsOffByDefault(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	})
	a, front, dir := incidentAgent(t, target, nil)

	for i := 0; i < config.ClientErrorFloor()+10; i++ {
		post(t, front.URL+"/examplepay/charges", `{}`)
	}
	drained(t, a, int64(config.ClientErrorFloor()+10))
	for _, ev := range readEvents(t, dir) {
		if ev.Kind == "client_error" {
			t.Fatal("4xx produced a client_error with incidents.client_errors unset")
		}
	}
}

// Below the sample floor no rate is claimed: the first 4xx is a rate of 1.0.
func TestClientErrorNotClaimedBelowSampleFloor(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	})
	a, front, dir := incidentAgent(t, target, func(u *config.Upstream) {
		u.Incidents.ClientErrors = true
	})

	for i := 0; i < config.ClientErrorFloor()-1; i++ {
		post(t, front.URL+"/examplepay/charges", `{}`)
	}
	drained(t, a, int64(config.ClientErrorFloor()-1))
	for _, ev := range readEvents(t, dir) {
		if ev.Kind == "client_error" {
			t.Fatalf("client_error claimed below the %d-request floor", config.ClientErrorFloor())
		}
	}
}

func TestClientErrorFiresAboveFloorAndRate(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
	})
	a, front, _ := incidentAgent(t, target, func(u *config.Upstream) {
		u.Incidents.ClientErrors = true
	})

	for i := 0; i < config.ClientErrorFloor()+5; i++ {
		post(t, front.URL+"/examplepay/charges", `{}`)
	}
	evs := eventsOfKind(t, a, "client_error")
	if evs[0].Level != "WARN" {
		t.Fatalf("client_error Level = %q, want WARN", evs[0].Level)
	}
}

// 429 is its own kind: being throttled is a different fix from the upstream
// failing, and a different fix from our payload being rejected.
func TestRateLimitedIsItsOwnKind(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	})
	a, front, _ := incidentAgent(t, target, nil)

	post(t, front.URL+"/examplepay/charges", `{}`)
	evs := eventsOfKind(t, a, "rate_limited")
	if evs[0].Level != "WARN" {
		t.Fatalf("rate_limited Level = %q, want WARN", evs[0].Level)
	}
}

// An unreachable upstream is pikopod's own 502, a different problem from the
// upstream answering 502 itself. Different kind, different fix.
func TestUnreachableUpstreamIsItsOwnKind(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	a, front, _ := incidentAgent(t, dead.URL, nil)

	post(t, front.URL+"/examplepay/charges", `{}`)
	eventsOfKind(t, a, "upstream_unreachable")
}

// A healthy upstream produces no incidents at all, or the signal is noise.
func TestHealthyUpstreamProducesNoIncidents(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"ch_1","status":"success"}`)
	})
	a, front, dir := incidentAgent(t, target, func(u *config.Upstream) {
		u.Incidents.ClientErrors = true
	})

	for i := 0; i < 30; i++ {
		post(t, front.URL+"/examplepay/charges", `{}`)
	}
	drained(t, a, 30)
	for _, ev := range readEvents(t, dir) {
		if ev.Kind.IsIncident() {
			t.Fatalf("healthy upstream produced incident %s", ev.Kind)
		}
	}
}
