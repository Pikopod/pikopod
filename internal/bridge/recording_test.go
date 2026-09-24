package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/proxy"
)

func writeRecordings(t *testing.T, dir, upstream string, recs ...proxy.Record) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range recs {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, "recordings", upstream+".ndjson")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func incidentEvent(kind drift.Kind, endpoint, after string) *alert.DriftEvent {
	return &alert.DriftEvent{
		SchemaVersion: alert.SchemaVersion, Fingerprint: "fp_abc123abc123",
		Upstream: "examplepay", Method: "POST", Endpoint: endpoint,
		StatusClass: "5xx", Kind: kind, After: after,
		FirstSeen: time.Now().Add(-time.Hour), LastSeen: time.Now(), Occurrences: 3,
	}
}

func steps(t *testing.T, pack map[string]any) []any {
	t.Helper()
	def, ok := pack["definition"].(map[string]any)
	if !ok {
		t.Fatalf("pack has no definition: %v", pack)
	}
	s, ok := def["steps"].([]any)
	if !ok {
		t.Fatalf("definition has no steps: %v", def)
	}
	return s
}

func stepOfType(t *testing.T, pack map[string]any, typ string) map[string]any {
	t.Helper()
	for _, s := range steps(t, pack) {
		m := s.(map[string]any)
		if m["type"] == typ {
			return m
		}
	}
	t.Fatalf("no %s step in pack", typ)
	return nil
}

func TestBuildFromRecordArmsTheRecordedFailure(t *testing.T) {
	rec := &proxy.Record{
		TS: time.Now(), Upstream: "examplepay", Method: "POST",
		Path: "/charges", Status: 503, ReqKind: "json",
		ReqBody: map[string]any{"amount": float64(5000)},
	}
	ev := incidentEvent(drift.UpstreamError, "/charges", "503")

	name, pack, err := BuildFromRecord(ev, rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "incident-") {
		t.Fatalf("pack name %q should be incident-scoped", name)
	}

	fault := stepOfType(t, pack, "INJECT_FAULT")["config"].(map[string]any)
	if fault["kind"] != "error" || fault["status"] != 503 {
		t.Fatalf("fault does not reproduce the recorded failure: %v", fault)
	}
	if fault["times"] != 1 {
		t.Fatalf("fault should fire once then recover, got times=%v", fault["times"])
	}

	req := stepOfType(t, pack, "REQUEST")
	cfg := req["config"].(map[string]any)
	if cfg["method"] != "POST" || cfg["path"] != "/charges" {
		t.Fatalf("replay does not carry the recorded request: %v", cfg)
	}
	if cfg["body"] == nil {
		t.Fatal("recorded JSON body was not replayed")
	}
	if len(req["assertions"].([]any)) == 0 {
		t.Fatal("replay asserts nothing — it cannot fail, so it proves nothing")
	}
}

func TestBuildFromRecordAlwaysDeclaresRedactedProvenance(t *testing.T) {
	rec := &proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 500}
	_, pack, err := BuildFromRecord(incidentEvent(drift.UpstreamError, "/charges", "500"), rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	note := stepOfType(t, pack, "NOTE")["config"].(map[string]any)["text"].(string)
	if !strings.Contains(strings.ToUpper(note), "RECONSTRUCTED") {
		t.Fatalf("NOTE does not declare redacted provenance: %q", note)
	}
}

func TestUnreachableReproducesAsConnectionReset(t *testing.T) {
	rec := &proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 502}
	ev := incidentEvent(drift.UpstreamUnreachable, "/charges", "502")

	_, pack, err := BuildFromRecord(ev, rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	fault := stepOfType(t, pack, "INJECT_FAULT")["config"].(map[string]any)
	if fault["kind"] != "connection_reset" {
		t.Fatalf("unreachable reproduced as %v, want connection_reset", fault["kind"])
	}
	if _, has := stepOfType(t, pack, "REQUEST")["assertions"]; has {
		t.Fatal("a reset transport has no status to assert")
	}
}

func TestRateLimitedReproducesAsRateLimitFault(t *testing.T) {
	rec := &proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 429}
	ev := incidentEvent(drift.RateLimited, "/charges", "429")
	_, pack, err := BuildFromRecord(ev, rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if k := stepOfType(t, pack, "INJECT_FAULT")["config"].(map[string]any)["kind"]; k != "rate_limit" {
		t.Fatalf("429 reproduced as %v, want rate_limit", k)
	}
}

func TestBuildFromRecordRefusesShapeDrift(t *testing.T) {
	rec := &proxy.Record{TS: time.Now(), Method: "GET", Path: "/charges", Status: 200}
	ev := incidentEvent(drift.FieldRemoved, "/charges", "")
	ev.Kind = drift.FieldRemoved

	_, _, err := BuildFromRecord(ev, rec, 0)
	if err == nil {
		t.Fatal("a shape-change event must be refused, not turned into a fault scenario")
	}
	if !strings.Contains(err.Error(), "from-drift") {
		t.Fatalf("refusal should point at the right command, got: %v", err)
	}
}

func TestFindRecordingHandlesMultipleAndNonTrailingParameters(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "examplepay",
		proxy.Record{TS: time.Now().Add(-time.Minute), Method: "POST",
			Path: "/accounts/acct_9/charges/ch_4/refund", Status: 503},
	)
	ev := incidentEvent(drift.UpstreamError, "/accounts/{accountId}/charges/{chargeId}/refund", "503")

	rec, err := FindRecording(dir, ev)
	if err != nil {
		t.Fatalf("from-drift refuses this endpoint shape; from-recording must not: %v", err)
	}
	if rec.Path != "/accounts/acct_9/charges/ch_4/refund" {
		t.Fatalf("matched the wrong recording: %s", rec.Path)
	}
}

func TestFindRecordingPicksMostRecentMatch(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	writeRecordings(t, dir, "examplepay",
		proxy.Record{TS: old, Method: "POST", Path: "/charges", Status: 503},
		proxy.Record{TS: recent, Method: "POST", Path: "/charges", Status: 503},
		proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 200},
		proxy.Record{TS: time.Now(), Method: "GET", Path: "/charges", Status: 503},
		proxy.Record{TS: time.Now(), Method: "POST", Path: "/refunds", Status: 503},
	)
	rec, err := FindRecording(dir, incidentEvent(drift.UpstreamError, "/charges", "503"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.TS.Equal(recent) {
		t.Fatalf("picked TS %v, want the most recent %v", rec.TS, recent)
	}
}

func TestFindRecordingRefusesWhenAgedOut(t *testing.T) {
	dir := t.TempDir()
	writeRecordings(t, dir, "examplepay",
		proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 200},
	)
	_, err := FindRecording(dir, incidentEvent(drift.UpstreamError, "/charges", "503"))
	if err == nil {
		t.Fatal("no matching recording must refuse, not fabricate one")
	}
	if !strings.Contains(err.Error(), "retention") {
		t.Fatalf("refusal should name retention as the likely cause, got: %v", err)
	}
}

func TestPathFitsTemplate(t *testing.T) {
	cases := []struct {
		path, template string
		want           bool
	}{
		{"/charges", "/charges", true},
		{"/charges/ch_1", "/charges/{id}", true},
		{"/accounts/a_1/charges/c_2", "/accounts/{a}/charges/{c}", true},
		{"/charges/ch_1?expand=x", "/charges/{id}", true},
		{"/charges", "/charges/{id}", false},
		{"/refunds/r_1", "/charges/{id}", false},
		{"/charges/ch_1/refund", "/charges/{id}", false},
	}
	for _, c := range cases {
		if got := pathFitsTemplate(c.path, c.template); got != c.want {
			t.Errorf("pathFitsTemplate(%q, %q) = %v, want %v", c.path, c.template, got, c.want)
		}
	}
}

func TestClientErrorNoteWarnsThatTheBodyIsRedacted(t *testing.T) {
	rec := &proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 422,
		ReqKind: "json", ReqBody: map[string]any{"amount": float64(5000)}}
	ev := incidentEvent(drift.ClientError, "/charges", "422")
	ev.StatusClass = "4xx"

	_, pack, err := BuildFromRecord(ev, rec, 0)
	if err != nil {
		t.Fatal(err)
	}
	note := stepOfType(t, pack, "NOTE")["config"].(map[string]any)["text"].(string)
	if !strings.Contains(note, "THIS MATTERS HERE") {
		t.Fatalf("client_error note does not warn that the body is redacted: %q", note)
	}
}

func TestClientErrorFloorCannotBeClearedByOneError(t *testing.T) {
	floor := config.ClientErrorFloor()
	const defaultRate = 0.05
	if got := 1.0 / float64(floor); got > defaultRate {
		t.Fatalf("one 4xx in %d requests is %.0f%%, which clears the %.0f%% default — the floor does not do its job",
			floor, got*100, defaultRate*100)
	}
}
