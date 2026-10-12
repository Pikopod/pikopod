package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/drift"
)

func TestAFailureOnAnExcludedPathStillFiresWithoutARecording(t *testing.T) {
	target := scriptedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		if strings.HasPrefix(r.URL.Path, "/health") {
			w.Write([]byte(`{"status":"HEALTHCANARY"}`))
			return
		}
		w.Write([]byte(`{"error":"down"}`))
	})
	a, front, dir := incidentAgent(t, target, func(up *config.Upstream) {
		up.Record = config.RecordFilter{Exclude: []string{"^/health"}}
	})
	for _, path := range []string{"/examplepay/health", "/examplepay/charges"} {
		resp, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	var evs []alert.DriftEvent
	waitFor(t, func() bool {
		evs = eventsOfKind(t, a, string(drift.UpstreamError))
		return len(evs) == 2
	})
	endpoints := map[string]bool{}
	for _, ev := range evs {
		endpoints[ev.Endpoint] = true
	}
	if !endpoints["/health"] || !endpoints["/charges"] {
		t.Fatalf("the 5xx on the excluded path is still an incident, like the recorded one: %+v", evs)
	}
	var raw []byte
	waitFor(t, func() bool {
		raw, _ = os.ReadFile(filepath.Join(dir, "recordings", "examplepay.ndjson"))
		return strings.Contains(string(raw), "/charges")
	})
	if strings.Contains(string(raw), "health") || strings.Contains(string(raw), "HEALTHCANARY") {
		t.Fatalf("the excluded exchange never reached disk:\n%s", raw)
	}
	rec := httptest.NewRecorder()
	a.healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health map[string]any
	json.Unmarshal(rec.Body.Bytes(), &health)
	if health["recordings_excluded"] != float64(1) {
		t.Fatalf("healthz counts the exclusion: %v", health["recordings_excluded"])
	}
}
