package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func journalEngine(t *testing.T) *Engine {
	t.Helper()
	return newEngine(t, loadWidgets(t), Config{ID: "sbx_journal", Seed: "journal-seed"})
}

func TestJournalRecordsHeadersQueryAndVirtualTime(t *testing.T) {
	e := journalEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/widgets?limit=5&cursor=abc", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Trace", "trace-1")
	e.ServeHTTP(httptest.NewRecorder(), req)

	entries, _ := e.JournalEntries(0)
	if len(entries) != 1 {
		t.Fatalf("want one entry, got %d", len(entries))
	}
	got := entries[0]
	if got.AtMs != e.VirtualClockMs() {
		t.Errorf("AtMs = %d, want the virtual clock %d", got.AtMs, e.VirtualClockMs())
	}
	if got.Headers["accept"] != "application/json" {
		t.Errorf("accept header = %q, want it journaled", got.Headers["accept"])
	}

	if len(got.Query["cursor"]) == 0 || got.Query["cursor"][0] != "abc" {
		t.Errorf("query cursor = %v, want [abc]", got.Query["cursor"])
	}
	if len(got.Query["limit"]) != 0 {
		t.Errorf("query limit = %v, want it dropped, never invented", got.Query["limit"])
	}
	if got.HeadersTruncated || got.QueryTruncated {
		t.Errorf("small request must not be truncated: %+v", got)
	}
}

func TestJournalRedactsCredentialsBeforeStoring(t *testing.T) {
	e := journalEngine(t)
	const secret = "SUPERSECRETVALUE0123456789"
	req := httptest.NewRequest(http.MethodGet, "/widgets?api_key="+secret, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set("Cookie", "session="+secret)
	e.ServeHTTP(httptest.NewRecorder(), req)

	entries, _ := e.JournalEntries(0)
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the journal kept a credential verbatim:\n%s", raw)
	}
	if len(entries[0].Headers) == 0 {
		t.Fatal("redaction must substitute, not delete the header set")
	}
}

func TestJournalFailsClosedOnTooManyHeaders(t *testing.T) {
	e := journalEngine(t)
	req := httptest.NewRequest(http.MethodGet, "/widgets", nil)
	for i := 0; i < maxJournalHeaders+5; i++ {
		req.Header.Set("X-Pad-"+strings.Repeat("a", i%20)+string(rune('a'+i%26))+strconv.Itoa(i), "v")
	}
	e.ServeHTTP(httptest.NewRecorder(), req)

	entries, _ := e.JournalEntries(0)
	got := entries[0]
	if !got.HeadersTruncated {
		t.Fatalf("over-cap headers must set HeadersTruncated (%d journaled)", len(got.Headers))
	}
	if got.Headers != nil {
		t.Fatal("a truncated header set must carry no content")
	}
}
