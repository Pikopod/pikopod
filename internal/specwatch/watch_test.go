package specwatch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/specdiff"
)

var t0Watch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const specV1 = `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /widgets:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
                required: [id]
                properties:
                  id: {type: string}
`

const specV2 = `openapi: 3.0.0
info: {title: T, version: "2"}
paths:
  /widgets:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
                properties:
                  id: {type: string}
                  note: {type: string}
`

type reports struct {
	upstreams []string
	findings  []specdiff.Finding
}

func (r *reports) report(upstream string, f specdiff.Finding) {
	r.upstreams = append(r.upstreams, upstream)
	r.findings = append(r.findings, f)
}

func stubFetch(body *string, etag *string, notModified *bool) FetchFunc {
	return func(source, gotETag string) ([]byte, string, bool, error) {
		if *notModified {
			return nil, gotETag, true, nil
		}
		e := ""
		if etag != nil {
			e = *etag
		}
		return []byte(*body), e, false, nil
	}
}

func newTestWatcher(t *testing.T, rep *reports, pinned bool) (*Watcher, *string, *bool) {
	t.Helper()
	dir := t.TempDir()
	body, notMod := specV1, false
	src := Source{Upstream: "pay", SpecSource: "https://example.invalid/spec.yaml"}
	if pinned {
		def, err := importer.NormalizeOpenAPI([]byte(specV1))
		if err != nil {
			t.Fatal(err)
		}
		src.Pinned = def
	}
	w := New(dir, []Source{src}, time.Hour, rep.report)
	w.SetFetch(stubFetch(&body, nil, &notMod))
	return w, &body, &notMod
}

func TestFirstFetchSeedsPinWithoutFindings(t *testing.T) {
	rep := &reports{}
	w, body, _ := newTestWatcher(t, rep, false)

	res := w.Check()
	if !res[0].Checked || res[0].Changed || len(rep.findings) != 0 {
		t.Fatalf("first sight must seed silently: %+v findings=%d", res[0], len(rep.findings))
	}

	// Advance past the interval, change the spec: the seeded pin catches it.
	w.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	*body = specV2
	res = w.Check()
	if !res[0].Changed || res[0].Findings == 0 {
		t.Fatalf("changed spec vs seeded pin: %+v", res[0])
	}
	assertHasFinding(t, rep.findings, "response-property-became-optional")
	assertHasFinding(t, rep.findings, "response-property-added")
}

func TestPinnedIRDiffAndHashGate(t *testing.T) {
	rep := &reports{}
	w, body, _ := newTestWatcher(t, rep, true)
	*body = specV2

	res := w.Check()
	if !res[0].Changed || res[0].Findings == 0 {
		t.Fatalf("pinned diff: %+v", res[0])
	}
	n := len(rep.findings)

	// Same bytes, past the interval: hash gate skips diff — nothing re-reported.
	w.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	res = w.Check()
	if !res[0].Checked || res[0].Changed {
		t.Fatalf("identical bytes must not re-diff: %+v", res[0])
	}
	if len(rep.findings) != n {
		t.Fatalf("hash gate leaked %d duplicate reports", len(rep.findings)-n)
	}
}

func TestIntervalGating(t *testing.T) {
	rep := &reports{}
	w, _, _ := newTestWatcher(t, rep, true)
	w.Check()
	res := w.Check() // immediately again — not due
	if res[0].Checked {
		t.Fatal("second immediate check must be interval-gated")
	}
}

func TestNotModifiedSkipsEverything(t *testing.T) {
	rep := &reports{}
	w, _, notMod := newTestWatcher(t, rep, true)
	w.Check()
	*notMod = true
	w.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	res := w.Check()
	if !res[0].Checked || res[0].Changed || res[0].Err != nil {
		t.Fatalf("304 must be a cheap no-op: %+v", res[0])
	}
}

func TestUnparseableSpecIsAnErrorNotAPanic(t *testing.T) {
	rep := &reports{}
	w, body, _ := newTestWatcher(t, rep, true)
	*body = "<!doctype html><html>maintenance page</html>"
	res := w.Check()
	if res[0].Err == nil {
		t.Fatal("an HTML maintenance page must surface as an error")
	}
	if len(rep.findings) != 0 {
		t.Fatal("no findings from garbage")
	}
}

func TestResetPinReseeds(t *testing.T) {
	rep := &reports{}
	dir := t.TempDir()
	body, notMod := specV1, false
	w := New(dir, []Source{{Upstream: "pay", SpecSource: "x"}}, time.Hour, rep.report)
	w.SetFetch(stubFetch(&body, nil, &notMod))
	w.Check() // seeds pin at v1

	if err := ResetPin(dir, "pay"); err != nil {
		t.Fatal(err)
	}
	// Fresh watcher (state cleared for pay): v2 becomes the NEW pin silently.
	w2 := New(dir, []Source{{Upstream: "pay", SpecSource: "x"}}, time.Hour, rep.report)
	body = specV2
	w2.SetFetch(stubFetch(&body, nil, &notMod))
	res := w2.Check()
	if res[0].Changed || len(rep.findings) != 0 {
		t.Fatalf("after ResetPin the next fetch re-seeds silently: %+v findings=%d", res[0], len(rep.findings))
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	rep := &reports{}
	dir := t.TempDir()
	body, notMod := specV2, false
	def, _ := importer.NormalizeOpenAPI([]byte(specV1))
	src := Source{Upstream: "pay", SpecSource: "x", Pinned: def}
	w := New(dir, []Source{src}, time.Hour, rep.report)
	w.SetFetch(stubFetch(&body, nil, &notMod))
	w.Check()
	n := len(rep.findings)
	if n == 0 {
		t.Fatal("setup: expected findings")
	}

	// A restarted watcher sees the persisted hash: same bytes → no re-diff.
	w2 := New(dir, []Source{src}, time.Hour, rep.report)
	w2.SetFetch(stubFetch(&body, nil, &notMod))
	w2.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	res := w2.Check()
	if res[0].Changed || len(rep.findings) != n {
		t.Fatalf("restart must not re-diff unchanged bytes: %+v", res[0])
	}
}

func TestHTTPFetchETagFlow(t *testing.T) {
	hits, notModHits := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModHits++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		hits++
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(specV1))
	}))
	defer srv.Close()

	raw, etag, notMod, err := Fetch(srv.URL, "")
	if err != nil || notMod || etag != `"v1"` || len(raw) == 0 {
		t.Fatalf("first fetch: %v etag=%q notMod=%v", err, etag, notMod)
	}
	_, _, notMod, err = Fetch(srv.URL, etag)
	if err != nil || !notMod {
		t.Fatalf("etag fetch: %v notMod=%v", err, notMod)
	}
	if hits != 1 || notModHits != 1 {
		t.Fatalf("server saw hits=%d notMod=%d", hits, notModHits)
	}
}

func assertHasFinding(t *testing.T, fs []specdiff.Finding, id string) {
	t.Helper()
	for _, f := range fs {
		if f.ID == id {
			return
		}
	}
	got := make([]string, len(fs))
	for i, f := range fs {
		got[i] = f.ID
	}
	t.Fatalf("missing finding %q in %v", id, got)
}

func TestDocumentedJournalRoundTrip(t *testing.T) {
	rep := &reports{}
	dir := t.TempDir()
	body, notMod := specV2, false
	def, _ := importer.NormalizeOpenAPI([]byte(specV1))
	w := New(dir, []Source{{Upstream: "pay", SpecSource: "x", Pinned: def}}, time.Hour, rep.report)
	w.SetFetch(stubFetch(&body, nil, &notMod))
	w.Check() // v1→v2 adds the "note" property

	doc := LoadDocumented(dir, "pay")
	if !doc.HasFieldAdded("GET", "/widgets", "note") {
		t.Fatalf("journal must record the documented field add: %+v", doc.Changes)
	}
	if doc.HasFieldAdded("GET", "/widgets", "other") {
		t.Fatal("journal must not over-claim")
	}
	// Canonical-template tolerance: traffic spelling with a different param
	// name still matches.
	if !doc.HasFieldAdded("GET", "/widgets", "note") {
		t.Fatal("canonical match")
	}
}

func TestCanonicalTemplate(t *testing.T) {
	if CanonicalTemplate("/w/{widgetId}") != CanonicalTemplate("/w/tok_{id}") {
		t.Fatal("param spellings must share an identity")
	}
	if CanonicalTemplate("/w/{id}") == CanonicalTemplate("/x/{id}") {
		t.Fatal("different routes must differ")
	}
}

// Fetch negative paths — non-200, redirect cap, size cap, and the
// error-path interval stamp (a broken source must not be hammered).
func TestFetchNegativePaths(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, _, _, err := Fetch(bad.URL, ""); err == nil || !strings.Contains(err.Error(), "watched spec fetch failed") {
		t.Fatalf("500 must error: %v", err)
	}

	hops := 0
	var loop *httptest.Server
	loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, loop.URL+"/again", http.StatusFound)
	}))
	defer loop.Close()
	if _, _, _, err := Fetch(loop.URL, ""); err == nil {
		t.Fatal("redirect chain must be capped")
	}

	if _, _, _, err := Fetch("/does/not/exist.yaml", ""); err == nil || !strings.Contains(err.Error(), "cannot read watched spec file") {
		t.Fatalf("missing file: %v", err)
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		var written int64
		for written <= maxSpecBytes { // one chunk past the cap
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += int64(n)
		}
	}))
	defer huge.Close()
	if _, _, _, err := Fetch(huge.URL, ""); err == nil || !strings.Contains(err.Error(), "size cap") {
		t.Fatalf("oversized spec must be a hard error, never a truncation: %v", err)
	}
}

func TestFetchErrorStampsIntervalAndLastError(t *testing.T) {
	rep := &reports{}
	dir := t.TempDir()
	w := New(dir, []Source{{Upstream: "pay", SpecSource: "/missing.yaml"}}, time.Hour, rep.report)

	res := w.Check()
	if res[0].Err == nil {
		t.Fatal("expected fetch error")
	}
	// Immediately again: interval-gated even after a FAILURE — a broken
	// source must not be re-fetched every agent tick.
	res = w.Check()
	if res[0].Checked {
		t.Fatal("failed check must still consume its interval slot")
	}
	// The failure is visible in persisted state.
	raw, err := os.ReadFile(filepath.Join(dir, "specwatch", "state.json"))
	if err != nil || !strings.Contains(string(raw), "last_error") {
		t.Fatalf("state must record the failure: %v %s", err, raw)
	}
}

func TestDocumentedJournalCapAndDedupe(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, nil, time.Hour, func(string, specdiff.Finding) {})
	find := func(path string) specdiff.Finding {
		return specdiff.Finding{ID: "response-property-added", Method: "GET", Template: "/w",
			Args: []string{"200 application/json", path}}
	}
	// Duplicate appends collapse.
	w.journalDocumented("pay", []specdiff.Finding{find("data.a"), find("data.a")}, time.Now())
	w.journalDocumented("pay", []specdiff.Finding{find("data.a")}, time.Now())
	if n := len(LoadDocumented(dir, "pay").Changes); n != 1 {
		t.Fatalf("dedupe: %d entries", n)
	}
	// A churny spec cannot grow the journal past the cap; newest survive.
	var storm []specdiff.Finding
	for i := 0; i < documentedCap+50; i++ {
		storm = append(storm, find(fmt.Sprintf("data.f%05d", i)))
	}
	w.journalDocumented("pay", storm, time.Now())
	d := LoadDocumented(dir, "pay")
	if len(d.Changes) != documentedCap {
		t.Fatalf("cap: %d", len(d.Changes))
	}
	if !d.HasFieldAdded("GET", "/w", fmt.Sprintf("data.f%05d", documentedCap+49)) {
		t.Fatal("newest entries must survive the cap")
	}
}

// A spec that fetches cleanly but fails to NORMALIZE must not silently
// retire the source. ETag and Hash are processed-successfully markers: if
// either advances past an unparseable document, the next check short-circuits
// (304 via ETag, sameBytes via Hash) and the source goes dark permanently
// while /healthz reports it healthy.
func TestUnparseableSpecDoesNotRetireTheSource(t *testing.T) {
	rep := &reports{}
	dir := t.TempDir()
	clock := t0Watch
	w := New(dir, []Source{{Upstream: "pay", SpecSource: "https://example.test/spec.yaml"}}, time.Hour, rep.report)
	w.SetClock(func() time.Time { return clock })

	var fetches int
	body := []byte("this is not a spec at all")
	w.SetFetch(func(src, etag string) ([]byte, string, bool, error) {
		fetches++
		// A real server 304s only when the caller's ETag is current. Ours
		// must not have advanced past the document that failed to parse.
		if etag == "etag-broken" {
			return nil, etag, true, nil
		}
		return body, "etag-broken", false, nil
	})

	res := w.Check()
	if res[0].Err == nil {
		t.Fatal("an unparseable spec must surface an error")
	}
	if h := w.Health()[0]; h.LastError == "" {
		t.Fatal("LastError must be recorded, or /healthz reports a dark source as healthy")
	}

	// Next interval: the source must be re-fetched AND re-parsed, not skipped.
	clock = clock.Add(2 * time.Hour)
	res = w.Check()
	if !res[0].Checked {
		t.Fatal("source went dark: the failed parse consumed the source permanently")
	}
	if res[0].Err == nil {
		t.Fatal("still-broken spec must still report an error, not pass silently")
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want 2 — the second check short-circuited instead of retrying", fetches)
	}

	// The provider fixes their document: the watcher recovers on its own.
	body = []byte(specV1)
	clock = clock.Add(2 * time.Hour)
	res = w.Check()
	if res[0].Err != nil {
		t.Fatalf("watcher must recover once the spec parses again: %v", res[0].Err)
	}
	if h := w.Health()[0]; h.LastError != "" {
		t.Fatalf("LastError must clear on recovery, got %q", h.LastError)
	}
}
