package alert

import (
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/drift"
)

func TestIncidentAlertNamesTheDeadlineAndTheExport(t *testing.T) {
	ev := &DriftEvent{Fingerprint: "fp_deadline01", Upstream: "pay", Method: "POST", Endpoint: "/charges", Kind: drift.UpstreamError, After: "503",
		FirstSeen: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), LastSeen: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Occurrences: 3}
	text := RenderWithRetention(ev, 48*time.Hour)
	if !strings.Contains(text, "reproducible until 2026-01-04T00:00:00Z") || !strings.Contains(text, "export: `pikopod incidents export fp_deadline01`") {
		t.Fatalf("incident alert:\n%s", text)
	}
	if strings.Contains(Render(ev), "reproducible until") {
		t.Fatal("without retention there is no deadline to name")
	}
	shape := &DriftEvent{Fingerprint: "fp_shape01", Upstream: "pay", Method: "GET", Endpoint: "/x", Kind: drift.FieldRemoved, Field: "a", Before: "string"}
	if strings.Contains(RenderWithRetention(shape, 48*time.Hour), "export:") {
		t.Fatal("shape drift is pinned with from-drift, not exported")
	}
}
