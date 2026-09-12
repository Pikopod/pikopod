package sandbox

import (
	"strings"
	"testing"
)

// The journal remembers what the client sent — counts by spec template,
// last-matching body access, and an eviction taint that never silently
// clears.
func TestJournalRecordsClientTraffic(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_j", Seed: "j-1"})
	do(t, e, "POST", "/widgets", `{"name":"g","amount":900}`, nil)
	do(t, e, "GET", "/widgets/widgets_1", "", nil)
	do(t, e, "GET", "/widgets/widgets_1", "", nil)
	do(t, e, "GET", "/nowhere", "", nil) // unrouted still journals

	if n, evicted := e.JournalCount("POST", "/widgets"); n != 1 || evicted {
		t.Fatalf("create count wrong: %d evicted=%v", n, evicted)
	}
	// Template-space matching: one template counts every concrete id.
	if n, _ := e.JournalCount("GET", "/widgets/{id}"); n != 2 {
		t.Fatalf("template count wrong: %d", n)
	}
	// Unrouted requests are countable by concrete path.
	if n, _ := e.JournalCount("GET", "/nowhere"); n != 1 {
		t.Fatalf("unrouted count wrong: %d", n)
	}
	// Empty method = any method for the template.
	if n, _ := e.JournalCount("", "/widgets/{id}"); n != 2 {
		t.Fatalf("any-method count wrong: %d", n)
	}
	last, found, _ := e.JournalLast("POST", "/widgets")
	if !found || last.Body == nil {
		t.Fatalf("last matching entry must carry the parsed body: %+v", last)
	}
	if body, ok := last.Body.(map[string]any); !ok || body["amount"] != float64(900) {
		t.Fatalf("journaled body wrong: %#v", last.Body)
	}
}

// Ring semantics: drop-oldest past the cap, taint set and countable;
// ResetJournal clears entries AND the taint (an explicit reset declares
// history irrelevant).
func TestJournalEvictionTaint(t *testing.T) {
	e := &Engine{}
	j := &e.journal
	for i := 0; i < maxJournalEntries+7; i++ {
		j.record(JournalEntry{Method: "GET", Path: "/x"})
	}
	if len(j.entries) != maxJournalEntries || j.evicted != 7 {
		t.Fatalf("ring wrong: %d entries, %d evicted", len(j.entries), j.evicted)
	}
	// Newest survive: the highest seq is present, the lowest is gone.
	if j.entries[len(j.entries)-1].Seq != int64(maxJournalEntries+7) || j.entries[0].Seq != 8 {
		t.Fatalf("drop-oldest violated: first=%d last=%d", j.entries[0].Seq, j.entries[len(j.entries)-1].Seq)
	}
	if _, evicted := e.JournalCount("GET", "/x"); !evicted {
		t.Fatal("eviction taint must be reported")
	}
	e.ResetJournal()
	if n, evicted := e.JournalCount("GET", "/x"); n != 0 || evicted {
		t.Fatal("reset must clear entries and the taint")
	}
}

// Oversized bodies journal without content and say so — a body assertion
// downstream must be able to fail closed rather than assert on nothing.
func TestJournalBodyCap(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_jb", Seed: "jb-1"})
	big := `{"name":"g","blob":"` + strings.Repeat("x", maxJournalBodyBytes) + `"}`
	do(t, e, "POST", "/widgets", big, nil)
	last, found, _ := e.JournalLast("POST", "/widgets")
	if !found || !last.BodyTruncated || last.Body != nil {
		t.Fatalf("oversized body must journal as truncated: %+v", last)
	}
}
