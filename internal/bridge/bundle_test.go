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

func incidentFixture(t *testing.T) (string, alert.DriftEvent) {
	t.Helper()
	dir := t.TempDir()
	ev := alert.DriftEvent{SchemaVersion: "1", Fingerprint: "fp_bundle0001", Upstream: "pay", Method: "POST", Endpoint: "/charges",
		StatusClass: "5xx", Kind: drift.UpstreamError, After: "503", FirstSeen: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), LastSeen: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Occurrences: 3}
	raw, _ := json.Marshal(ev)
	os.WriteFile(filepath.Join(dir, "events.ndjson"), append(raw, '\n'), 0o600)
	os.MkdirAll(filepath.Join(dir, "recordings"), 0o700)
	rec := proxy.Record{TS: ev.LastSeen, Upstream: "pay", Method: "POST", Path: "/charges", Status: 503, RespKind: "json", ReqBody: map[string]any{"amount": 100.0}, RespBody: map[string]any{"message": "upstream down"}}
	rraw, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(dir, "recordings", "pay.ndjson"), append(rraw, '\n'), 0o600)
	return dir, ev
}

func TestExportRoundTripsThroughLoad(t *testing.T) {
	dir, ev := incidentFixture(t)
	b, err := Export(dir, &ev, 48*time.Hour, "agent-7", time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != BundleSchemaVersion || b.Source.Host != "agent-7" || b.ExpiresAt == nil || !b.ExpiresAt.Equal(ev.LastSeen.Add(48*time.Hour)) {
		t.Fatalf("bundle header: %+v", b)
	}
	raw, _ := json.Marshal(b)
	path := filepath.Join(t.TempDir(), "incident.json")
	os.WriteFile(path, raw, 0o600)
	back, err := LoadBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Event.Fingerprint != ev.Fingerprint || back.Recording.Status != 503 || back.ContractVersion != 0 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if b2, _ := Export(dir, &ev, 0, "h", time.Now()); b2.ExpiresAt != nil {
		t.Fatal("no retention, no expiry")
	}
}

func TestLoadBundleRefusesWhatItCannotTrust(t *testing.T) {
	dir, ev := incidentFixture(t)
	b, _ := Export(dir, &ev, 0, "h", time.Now())
	good, _ := json.Marshal(b)
	write := func(body []byte) string {
		p := filepath.Join(t.TempDir(), "b.json")
		os.WriteFile(p, body, 0o600)
		return p
	}
	cases := map[string][]byte{
		"not json":      []byte("nope"),
		"wrong version": []byte(strings.Replace(string(good), `"schema_version":1`, `"schema_version":2`, 1)),
		"unknown field": []byte(strings.Replace(string(good), `"schema_version":1`, `"schema_version":1,"salt":"x"`, 1)),
		"missing event": []byte(`{"schema_version":1,"recording":{"method":"GET"}}`),
		"oversized":     append(append([]byte(`{"schema_version":1,"pad":"`), make([]byte, maxBundleBytes)...), []byte(`"}`)...),
	}
	for name, body := range cases {
		if _, err := LoadBundle(write(body)); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if _, err := LoadBundle(write(good)); err != nil {
		t.Fatal(err)
	}
}

func TestIsBundleArg(t *testing.T) {
	if IsBundleArg("fp_14835fa32dfb") || !IsBundleArg("./incident.json") || !IsBundleArg("out/incident.json") || !IsBundleArg("x.json") {
		t.Fatal("fingerprints are never paths; .json and slashed arguments are")
	}
}
