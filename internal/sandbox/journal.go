// Request journal: a bounded drop-oldest ring of what the CLIENT did. Once
// anything is evicted, upper-bound assertions must fail loudly, not pass silently.
package sandbox

import (
	"encoding/json"
	"strings"
	"sync"
)

const maxJournalEntries = 1024

// A larger body is journaled without its content (BodyTruncated) and body
// assertions against it fail closed.
const maxJournalBodyBytes = 16 * 1024

// JournalEntry is one request the sandbox received, as the client sent it.
type JournalEntry struct {
	Seq      int64  `json:"seq"`
	Method   string `json:"method"`
	Path     string `json:"path"`               // concrete request path
	Template string `json:"template,omitempty"` // matched spec template ("" = unrouted)
	Status   int    `json:"status"`
	// Body is the parsed JSON request body (nil when absent/non-JSON/too
	// large — see BodyTruncated).
	Body          any  `json:"body,omitempty"`
	BodyTruncated bool `json:"bodyTruncated,omitempty"`
}

type journal struct {
	mu      sync.Mutex
	entries []JournalEntry
	seq     int64
	evicted int64
}

// plainJSON normalizes the ingress parser's internal value types into plain Go
// JSON values, so journal consumers see ordinary maps and numbers.
func plainJSON(v any) any {
	raw, err := marshalJSValue(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func (j *journal) record(entry JournalEntry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	entry.Seq = j.seq
	j.entries = append(j.entries, entry)
	if len(j.entries) > maxJournalEntries {
		drop := len(j.entries) - maxJournalEntries
		j.entries = append(j.entries[:0:0], j.entries[drop:]...)
		j.evicted += int64(drop)
	}
}

// matches compares STRUCTURALLY, so authors write `/widgets/{id}` without
// knowing the spec's parameter name, and unrouted concrete paths stay countable.
func (entry *JournalEntry) matches(method, template string) bool {
	if method != "" && !strings.EqualFold(entry.Method, method) {
		return false
	}
	if template == "" {
		return true
	}
	return templateSegmentsMatch(template, entry.Template) || templateSegmentsMatch(template, entry.Path)
}

func templateSegmentsMatch(query, target string) bool {
	if target == "" {
		return false
	}
	q := strings.Split(strings.Trim(query, "/"), "/")
	tg := strings.Split(strings.Trim(target, "/"), "/")
	if len(q) != len(tg) {
		return false
	}
	for i := range q {
		queryTemplated := strings.Contains(q[i], "{")
		targetTemplated := strings.Contains(tg[i], "{")
		if queryTemplated || targetTemplated {
			continue // a parameter position matches any segment
		}
		if q[i] != tg[i] {
			return false
		}
	}
	return true
}

// JournalCount reports whether the journal has ever evicted; the caller decides
// whether its assertion is still provable.
func (e *Engine) JournalCount(method, template string) (count int, evicted bool) {
	e.journal.mu.Lock()
	defer e.journal.mu.Unlock()
	for i := range e.journal.entries {
		if e.journal.entries[i].matches(method, template) {
			count++
		}
	}
	return count, e.journal.evicted > 0
}

// JournalLast returns the most recent matching entry.
func (e *Engine) JournalLast(method, template string) (entry *JournalEntry, found, evicted bool) {
	e.journal.mu.Lock()
	defer e.journal.mu.Unlock()
	for i := len(e.journal.entries) - 1; i >= 0; i-- {
		if e.journal.entries[i].matches(method, template) {
			cp := e.journal.entries[i]
			return &cp, true, e.journal.evicted > 0
		}
	}
	return nil, false, e.journal.evicted > 0
}

// JournalEntries snapshots the newest entries (newest LAST), up to limit
// (0 = all retained), plus how many older entries were evicted.
func (e *Engine) JournalEntries(limit int) (entries []JournalEntry, evicted int64) {
	e.journal.mu.Lock()
	defer e.journal.mu.Unlock()
	all := e.journal.entries
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]JournalEntry, len(all))
	copy(out, all)
	return out, e.journal.evicted
}

// ResetJournal drops all entries AND the eviction taint — an explicit
// reset declares history irrelevant (a filtered clear would not).
func (e *Engine) ResetJournal() {
	e.journal.mu.Lock()
	defer e.journal.mu.Unlock()
	e.journal.entries = nil
	e.journal.evicted = 0
}
