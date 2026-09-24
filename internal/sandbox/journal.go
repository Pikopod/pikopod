package sandbox

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/pikopod/pikopod/internal/sanitize"
)

const maxJournalEntries = 1024

const maxJournalBodyBytes = 16 * 1024

const (
	maxJournalHeaders     = 64
	maxJournalQueryValues = 64
)

type JournalEntry struct {
	Seq      int64  `json:"seq"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Template string `json:"template,omitempty"`
	Status   int    `json:"status"`

	Body             any               `json:"body,omitempty"`
	BodyTruncated    bool              `json:"bodyTruncated,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	HeadersTruncated bool              `json:"headersTruncated,omitempty"`

	Query          map[string][]string `json:"query,omitempty"`
	QueryTruncated bool                `json:"queryTruncated,omitempty"`

	AtMs int64 `json:"atMs"`
}

type journal struct {
	mu      sync.Mutex
	entries []JournalEntry
	seq     int64
	evicted int64
}

func (e *Engine) journalHeaders(h map[string]string) (map[string]string, bool) {
	if len(h) == 0 {
		return nil, false
	}
	if len(h) > maxJournalHeaders {
		return nil, true
	}
	flat := make(map[string]any, len(h))
	for k, v := range h {
		flat[k] = v
	}
	res := sanitize.Sanitize(flat, e.journalTok, nil, true)
	obj, _ := res.Sanitized.(map[string]any)
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, false
}

func (e *Engine) journalQuery(q map[string][]string) (map[string][]string, bool) {
	if len(q) == 0 {
		return nil, false
	}
	total := 0
	for _, vs := range q {
		total += len(vs)
	}
	if total > maxJournalQueryValues {
		return nil, true
	}

	flat := make(map[string]any, len(q))
	for k, vs := range q {
		vals := make([]any, len(vs))
		for i, v := range vs {
			vals[i] = v
		}
		flat[k] = vals
	}
	res := sanitize.Sanitize(flat, e.journalTok, nil, false)
	obj, _ := res.Sanitized.(map[string]any)
	out := make(map[string][]string, len(obj))
	for k, v := range obj {
		vals, ok := v.([]any)
		if !ok {
			continue
		}
		kept := make([]string, 0, len(vals))
		for _, item := range vals {
			if s, ok := item.(string); ok {
				kept = append(kept, s)
			}
		}
		out[k] = kept
	}
	return out, false
}

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
			continue
		}
		if q[i] != tg[i] {
			return false
		}
	}
	return true
}

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

func (e *Engine) ResetJournal() {
	e.journal.mu.Lock()
	defer e.journal.mu.Unlock()
	e.journal.entries = nil
	e.journal.evicted = 0
}

type SequenceFields struct {
	Method  string
	Headers map[string]string
	Query   map[string]string
}

func (entry *JournalEntry) MatchesSequence(f SequenceFields, path string) bool {
	if !entry.matches(f.Method, path) {
		return false
	}
	for k, want := range f.Headers {
		if entry.Headers[strings.ToLower(k)] != want {
			return false
		}
	}
	for k, want := range f.Query {
		vals := entry.Query[k]
		hit := false
		for _, v := range vals {
			if v == want {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}
