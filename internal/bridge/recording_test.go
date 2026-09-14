package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
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

// The whole point: a recorded failure becomes a scenario that arms the same
// failure and replays the same request.
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

// Every pack must say the request was reconstructed from redacted data.
// Silence here invites someone to treat a token as the original value.
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

// An unreachable upstream is a dead socket, not a status. Reproducing it as a
// 502 would exercise the wrong branch of the caller's error handling.
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

// A shape change is not reproducible this way, and saying so beats producing a
// scenario that arms a fault for something that never failed.
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

// The reason from-recording is simpler than from-drift: the recording has the
// concrete path, so multiple and non-trailing parameters are not a limitation.
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

// Most recent wins, and only genuinely matching recordings are candidates.
func TestFindRecordingPicksMostRecentMatch(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	writeRecordings(t, dir, "examplepay",
		proxy.Record{TS: old, Method: "POST", Path: "/charges", Status: 503},
		proxy.Record{TS: recent, Method: "POST", Path: "/charges", Status: 503},
		proxy.Record{TS: time.Now(), Method: "POST", Path: "/charges", Status: 200}, // wrong status
		proxy.Record{TS: time.Now(), Method: "GET", Path: "/charges", Status: 503},  // wrong method
		proxy.Record{TS: time.Now(), Method: "POST", Path: "/refunds", Status: 503}, // wrong path
	)
	rec, err := FindRecording(dir, incidentEvent(drift.UpstreamError, "/charges", "503"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.TS.Equal(recent) {
		t.Fatalf("picked TS %v, want the most recent %v", rec.TS, recent)
	}
}

// An aged-out recording is a typed refusal naming retention, never a
// synthesised request. Fabricating one would produce a scenario that passes and
// means nothing.
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

// A 4xx is usually caused by the body, and the body we have is the redacted
// one. The scenario must say so, or someone debugs a rejection against a
// payload their code never sent.
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
