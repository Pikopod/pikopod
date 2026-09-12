package alert

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/drift"
)

type memSink struct {
	mu    sync.Mutex
	texts []string
}

func (m *memSink) Deliver(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.texts = append(m.texts, text)
	return nil
}
func (m *memSink) Name() string { return "mem" }
func (m *memSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.texts)
}

var testFinding = drift.Finding{
	Upstream: "pay", Method: "GET", Template: "/tx/{id}", StatusClass: "2xx",
	Kind: drift.FieldAdded, Field: "fee", After: "string",
}

func newAlerter(t *testing.T, dir string, sink Sink) *Alerter {
	t.Helper()
	a, err := New(dir, Options{MinOccurrences: 3, Window: 15 * time.Minute}, sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// N-occurrence emission then dedupe: no alert before the threshold, exactly
// one at it, none after — while occurrence counting continues.
func TestNOccurrenceThenDedupe(t *testing.T) {
	sink := &memSink{}
	a := newAlerter(t, t.TempDir(), sink)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a.SetClock(func() time.Time { return now })

	a.Report(testFinding)
	a.Report(testFinding)
	a.Flush()
	if sink.count() != 0 {
		a.Flush()
		t.Fatalf("2 occurrences must not page anyone: %v", sink.texts)
	}
	a.Report(testFinding)
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("the 3rd occurrence must alert exactly once: %v", sink.texts)
	}
	a.Report(testFinding)
	a.Report(testFinding)
	a.Flush()
	if sink.count() != 1 {
		t.Fatal("later occurrences must only bump counters")
	}
	ev, ok := a.EventFor(testFinding.Fingerprint())
	if !ok || ev.Occurrences != 5 {
		t.Fatalf("occurrences must keep counting after the alert: %+v", ev)
	}
}

// A slow drip below the threshold never alerts: the window resets and the
// count restarts — the 2am five-minute wobble stays silent forever.
func TestWindowResetKeepsSlowDripSilent(t *testing.T) {
	sink := &memSink{}
	a := newAlerter(t, t.TempDir(), sink)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a.SetClock(func() time.Time { return now })

	for i := 0; i < 6; i++ {
		a.Report(testFinding)
		a.Report(testFinding)
		now = now.Add(16 * time.Minute) // past the 15m window before the 3rd
	}
	a.Flush()
	if sink.count() != 0 {
		a.Flush()
		t.Fatalf("2-per-window forever must never alert: %v", sink.texts)
	}
	// Three inside one window still fires.
	a.Report(testFinding)
	a.Report(testFinding)
	a.Report(testFinding)
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("3 in one window must alert: %d", sink.count())
	}
}

// Dedupe survives restarts: an alerted fingerprint reported again by a
// fresh Alerter over the same data dir stays silent (redeploys must not
// re-storm the channel), and an acked fingerprint stays acked.
func TestDedupeAndAckSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	sink := &memSink{}
	a, err := New(dir, Options{MinOccurrences: 1}, sink)
	if err != nil {
		t.Fatal(err)
	}
	a.Report(testFinding) // MinOccurrences 1 → alerts immediately
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("setup: expected the alert, got %d", sink.count())
	}
	acked := drift.Finding{Upstream: "pay", Method: "GET", Template: "/tx/{id}", StatusClass: "2xx",
		Kind: drift.FieldRemoved, Field: "old"}
	a.Report(acked)
	if !a.Ack(acked.Fingerprint()) {
		t.Fatal("ack of a known fingerprint must succeed")
	}
	a.Close()

	sink2 := &memSink{}
	b, err := New(dir, Options{MinOccurrences: 1}, sink2)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Report(testFinding)
	b.Report(acked)
	b.Flush()
	if sink2.count() != 0 {
		t.Fatalf("a restart must not re-alert alerted or acked fingerprints: %v", sink2.texts)
	}
	// The persisted event keeps counting occurrences across the restart.
	ev, ok := b.EventFor(testFinding.Fingerprint())
	if !ok || ev.Occurrences != 2 {
		t.Fatalf("occurrences must continue from persisted state: %+v", ev)
	}
	if len(b.Active()) != 1 { // the acked one is not active
		t.Fatalf("active must show alerted-and-unacked only: %+v", b.Active())
	}
}

// Two different findings alert independently — dedupe is per fingerprint,
// not per endpoint.
func TestDistinctFindingsAlertIndependently(t *testing.T) {
	sink := &memSink{}
	a := newAlerter(t, t.TempDir(), sink)
	other := testFinding
	other.Field = "tax"
	for i := 0; i < 3; i++ {
		a.Report(testFinding)
		a.Report(other)
	}
	a.Flush()
	if sink.count() != 2 {
		a.Flush()
		t.Fatalf("two divergences must page twice: %v", sink.texts)
	}
	a.Flush()
	for _, txt := range sink.texts {
		if !strings.Contains(txt, "pikopod drift") || !strings.Contains(txt, "from-drift fp_") {
			t.Fatalf("alert text must carry the product loop (diff + replay hint): %q", txt)
		}
	}
}

// ------------------------------------------------------- declared findings

func declaredEvent() DriftEvent {
	return DriftEvent{
		Method: "GET", Endpoint: "/tx/{id}",
		Kind: drift.Kind("declared:endpoint-removed"), Level: "ERR",
		Detail: "endpoint removed from the spec",
	}
}

func TestDeclaredAlertsOnFirstOccurrence(t *testing.T) {
	sink := &memSink{}
	a := newAlerter(t, t.TempDir(), sink)
	a.ReportDeclared("pay", "fp_decl_test1", declaredEvent())
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("declared drift is deterministic — must alert on first occurrence, got %d", sink.count())
	}
	// Re-running the watcher over the same standing diff never re-alerts.
	a.ReportDeclared("pay", "fp_decl_test1", declaredEvent())
	a.ReportDeclared("pay", "fp_decl_test1", declaredEvent())
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("dedupe by fingerprint: got %d deliveries", sink.count())
	}
	ev, ok := a.EventFor("fp_decl_test1")
	if !ok || ev.Source != "declared" || ev.Occurrences != 3 || ev.Level != "ERR" {
		t.Fatalf("event state: %+v ok=%v", ev, ok)
	}
}

func TestDeclaredDedupeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	sink := &memSink{}
	a := newAlerter(t, dir, sink)
	a.ReportDeclared("pay", "fp_decl_test2", declaredEvent())
	a.Close()

	sink2 := &memSink{}
	a2 := newAlerter(t, dir, sink2)
	defer a2.Close()
	a2.ReportDeclared("pay", "fp_decl_test2", declaredEvent())
	a2.Flush()
	if sink2.count() != 0 {
		t.Fatal("a restart must not re-alert a persisted declared fingerprint")
	}
}

func TestDeclaredAckSuppresses(t *testing.T) {
	sink := &memSink{}
	a := newAlerter(t, t.TempDir(), sink)
	defer a.Close()
	a.ReportDeclared("pay", "fp_decl_test3", declaredEvent())
	a.Ack("fp_decl_test3")
	a.ReportDeclared("pay", "fp_decl_test3", declaredEvent())
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("acked fingerprint re-alerted: %d", sink.count())
	}
}

func TestRenderDeclared(t *testing.T) {
	ev := declaredEvent()
	ev.Fingerprint, ev.Upstream, ev.Source = "fp_x", "pay", "declared"
	msg := Render(&ev)
	for _, want := range []string{"declared drift", "[ERR]", "GET /tx/{id}", "endpoint removed from the spec", "SPEC", "fp_x"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("declared render missing %q:\n%s", want, msg)
		}
	}
}

// --------------------------------------------------------- severity floor

func TestMinLevelFloorsDeliveryNotRecord(t *testing.T) {
	sink := &memSink{}
	dir := t.TempDir()
	a, err := New(dir, Options{MinOccurrences: 1, Window: time.Minute, MinLevel: "WARN"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// INFO-grade observed finding (additive field): floored from the channel.
	info := drift.Finding{Upstream: "pay", Method: "GET", Template: "/tx", StatusClass: "2xx",
		Kind: drift.FieldAdded, Field: "fee", After: "string"}
	a.Report(info)
	a.Flush()
	if sink.count() != 0 {
		a.Flush()
		t.Fatalf("INFO must not reach the channel under min_level WARN: %d", sink.count())
	}
	// ...but the record survives: dedupe state + Active() still carry it.
	if evs := a.Active(); len(evs) != 1 || evs[0].Level != "INFO" {
		t.Fatalf("floored alert must stay on record: %+v", evs)
	}

	// ERR-grade delivers.
	a.Report(drift.Finding{Upstream: "pay", Method: "GET", Template: "/tx", StatusClass: "2xx",
		Kind: drift.FieldRemoved, Field: "amount", Before: "number"})
	a.Flush()
	if sink.count() != 1 {
		a.Flush()
		t.Fatalf("ERR must deliver: %d", sink.count())
	}

	// Declared WARN delivers at the WARN floor.
	a.ReportDeclared("pay", "fp_floor_warn", DriftEvent{Method: "GET", Endpoint: "/tx",
		Kind: drift.Kind("declared:response-enum-value-added"), Level: "WARN", Detail: "d"})
	a.Flush()
	if sink.count() != 2 {
		a.Flush()
		t.Fatalf("WARN at WARN floor must deliver: %d", sink.count())
	}
}

func TestDigest(t *testing.T) {
	sink := &memSink{}
	a, err := New(t.TempDir(), Options{MinOccurrences: 1, Window: time.Minute,
		MinLevel: "ERR", SummaryEvery: time.Hour}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	now := time.Now()
	a.SetClock(func() time.Time { return now })

	a.MaybeDigest() // arms the window
	a.Report(drift.Finding{Upstream: "pay", Method: "GET", Template: "/a", StatusClass: "2xx",
		Kind: drift.FieldAdded, Field: "x", After: "string"}) // INFO, floored
	a.Report(drift.Finding{Upstream: "pay", Method: "GET", Template: "/b", StatusClass: "2xx",
		Kind: drift.FieldRemoved, Field: "y", Before: "string"}) // ERR, delivered
	a.ReportDeclared("pay", "fp_digest_1", DriftEvent{Method: "GET", Endpoint: "/c",
		Kind: drift.Kind("declared:endpoint-removed"), Level: "ERR", Detail: "d"})

	a.Flush()
	before := sink.count()
	a.MaybeDigest() // window not elapsed
	a.Flush()
	if sink.count() != before {
		t.Fatal("digest fired early")
	}
	now = now.Add(2 * time.Hour)
	a.MaybeDigest()
	a.Flush()
	if sink.count() != before+1 {
		a.Flush()
		t.Fatalf("digest missing: %d", sink.count())
	}
	a.Flush()
	msg := sink.texts[len(sink.texts)-1]
	for _, want := range []string{"digest", "observed drift 2", "declared drift 1", "1 alert(s) below slack.min_level"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("digest missing %q:\n%s", want, msg)
		}
	}
	// The window reset: an immediate re-check posts nothing.
	a.MaybeDigest()
	a.Flush()
	if sink.count() != before+1 {
		t.Fatal("digest double-fired")
	}
}

// --------------------------------------------------------------- delivery

func TestSlackWebhookSinkDeliver(t *testing.T) {
	var got map[string]string
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := (SlackWebhookSink{URL: srv.URL}).Deliver("hello drift"); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "hello drift" || !strings.Contains(contentType, "application/json") {
		t.Fatalf("body=%v content-type=%s", got, contentType)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	err := (SlackWebhookSink{URL: bad.URL}).Deliver("x")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("non-2xx must surface with the status: %v", err)
	}
}

func TestDeliveryHealthReflectsSinkFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	a, err := New(t.TempDir(), Options{MinOccurrences: 1, Window: time.Minute},
		SlackWebhookSink{URL: bad.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Report(testFinding)
	a.Flush()
	sent, ok, lastErr := a.DeliveryHealth()
	if sent != 1 || ok || !strings.Contains(lastErr, "slack-webhook") {
		t.Fatalf("health after failed delivery: sent=%d ok=%v err=%q", sent, ok, lastErr)
	}
}

// Crash-safety ordering: the dedupe latch persists BEFORE network delivery,
// so a crash mid-delivery re-alerts nothing on restart.
func TestLatchPersistedBeforeDelivery(t *testing.T) {
	dir := t.TempDir()
	var persistedDuringDelivery bool
	probe := sinkFunc(func(text string) error {
		raw, err := os.ReadFile(filepath.Join(dir, "alerts", "state.json"))
		persistedDuringDelivery = err == nil && strings.Contains(string(raw), `"alerted": true`)
		return nil
	})
	a, err := New(dir, Options{MinOccurrences: 1, Window: time.Minute}, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Report(testFinding)
	a.Flush() // Wait/Done ordering makes the probe's write visible here
	if !persistedDuringDelivery {
		t.Fatal("the Alerted latch must be on disk before the sink is called")
	}
}

type sinkFunc func(string) error

func (f sinkFunc) Deliver(text string) error { return f(text) }
func (f sinkFunc) Name() string              { return "probe" }

// F10: the fingerprint cap must never become a silent permanent blackout.
func TestSaturationEvictsInsteadOfBlackout(t *testing.T) {
	sink := &memSink{}
	a, err := New(t.TempDir(), Options{MinOccurrences: 3, Window: 15 * time.Minute, MaxTracked: 50}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Fill to the (test-sized) cap with ALERTED states (evictable).
	for i := 0; i < 50; i++ {
		a.ReportDeclared("pay", fmt.Sprintf("fp_fill_%05d", i), declaredEvent())
	}
	a.Flush()
	filled := sink.count()
	if filled != 50 {
		t.Fatalf("setup: %d", filled)
	}

	// One more NEW fingerprint: an old alerted state is evicted, the new
	// drift still alerts — the product's one job keeps working at the cap.
	a.ReportDeclared("pay", "fp_the_new_one", declaredEvent())
	a.Flush()
	if sink.count() != filled+1 {
		t.Fatalf("new drift at the cap must still alert: %d", sink.count())
	}
	if _, ok := a.EventFor("fp_the_new_one"); !ok {
		t.Fatal("new fingerprint must be tracked after eviction")
	}
	a.mu.Lock()
	evictions := a.SaturationEvictions
	a.mu.Unlock()
	if evictions == 0 {
		t.Fatal("eviction must be counted")
	}
}

// Acked states are evicted before alerted ones.
func TestSaturationPrefersAckedVictims(t *testing.T) {
	sink := &memSink{}
	a, err := New(t.TempDir(), Options{MinOccurrences: 3, Window: 15 * time.Minute, MaxTracked: 50}, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 50; i++ {
		a.ReportDeclared("pay", fmt.Sprintf("fp_f2_%05d", i), declaredEvent())
	}
	a.Ack("fp_f2_00007")
	a.ReportDeclared("pay", "fp_f2_new", declaredEvent())
	if _, stillThere := a.EventFor("fp_f2_00007"); stillThere {
		t.Fatal("the acked state must be the eviction victim")
	}
	if _, ok := a.EventFor("fp_f2_00042"); !ok {
		t.Fatal("un-acked alerted states must survive while an acked victim exists")
	}
}

// A wedged sink must not stall the caller: Report returns immediately and
// overflow drops-and-counts.
func TestSlowSinkNeverBlocksReport(t *testing.T) {
	release := make(chan struct{})
	slow := sinkFunc(func(string) error { <-release; return nil })
	a, err := New(t.TempDir(), Options{MinOccurrences: 1, Window: time.Minute}, slow)
	if err != nil {
		t.Fatal(err)
	}
	// The 60/h delivery ceiling would cap deliver() calls below the queue
	// size — advance the clock across ceiling windows so >256 deliveries
	// are attempted while the first one wedges the loop.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var reports int
	a.SetClock(func() time.Time { return base.Add(time.Duration(reports/50) * 2 * time.Hour) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 400 distinct fingerprints: the first delivery wedges the loop, the
		// 256-slot queue fills, the rest drop — none of it blocks Report.
		for i := 0; i < 400; i++ {
			reports = i
			a.ReportDeclared("pay", fmt.Sprintf("fp_slow_%03d", i), declaredEvent())
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Report blocked on a wedged sink")
	}
	a.mu.Lock()
	dropped := a.DeliveriesDropped
	a.mu.Unlock()
	if dropped == 0 {
		t.Fatal("overflow must drop and count, not queue unboundedly")
	}
	close(release)
	a.Close()
}

// Alert bodies are authored for Slack. The stdout sink is a different medium:
// no :emoji: codes, no *asterisks*, no backticks — and field names carrying
// underscores must survive untouched.
func TestPlainTextRendersSlackMarkdownForATerminal(t *testing.T) {
	in := ":rotating_light: *pikopod drift — new field* on `GET /transaction/tx_{id}` (fakepay)\n" +
		"`fee_bearer` (string) appeared in responses\n" +
		"_documented in the provider's changelog_\n" +
		"fingerprint `fp_6d540d187d44`"
	got := PlainText(in)

	for _, bad := range []string{":rotating_light:", ":warning:", ":memo:", "*", "`"} {
		if strings.Contains(got, bad) {
			t.Fatalf("Slack markup %q leaked into terminal output:\n%s", bad, got)
		}
	}
	for _, want := range []string{
		"[ERR]",
		"pikopod drift — new field",
		"GET /transaction/tx_{id}",
		"fee_bearer (string) appeared in responses", // underscore field name intact
		"documented in the provider's changelog",    // whole-line italic unwrapped
		"fp_6d540d187d44",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// Two underscored field names on one line must not be read as an italic span.
func TestPlainTextDoesNotTreatFieldUnderscoresAsItalics(t *testing.T) {
	got := PlainText("`account_number` and `fee_bearer` both changed")
	if got != "account_number and fee_bearer both changed" {
		t.Fatalf("underscored field names were mangled: %q", got)
	}
}
