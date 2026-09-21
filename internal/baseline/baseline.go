// Package baseline learns per-endpoint-family "normal" and freezes a reference
// at warmup: LEARN → FREEZE → DIFF, so new shapes never silently become normal.
package baseline

import (
	"encoding/json"
	"fmt"
	"github.com/pikopod/pikopod/internal/store"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/pathtmpl"
)

// Under the cap a field is enum-ish and value drift is detectable; past it
// only shape is compared (high-cardinality: ids, tokens).
const valueTrackCap = 24

// Past the latch, only known paths keep updating.
const fieldTrackCap = 2000

type FieldStats struct {
	Count  int            `json:"count"`
	Types  map[string]int `json:"types"`
	Values map[string]int `json:"values,omitempty"`
	// HighCardinality latches once distinct values exceed the cap.
	HighCardinality bool `json:"high_cardinality,omitempty"`
	// Churned latches when a value-suppressed field showed two distinct
	// values: the evidence that the suppression is silencing something real.
	Churned bool `json:"churned,omitempty"`
	// lastValue is in-memory only: enough to notice churn, never persisted.
	lastValue string
	seenValue bool
}

type Family struct {
	Method      string                 `json:"method"`
	Template    string                 `json:"template"`
	StatusClass string                 `json:"status_class"`
	Samples     int                    `json:"samples"`
	FirstSeen   time.Time              `json:"first_seen"`
	LastSeen    time.Time              `json:"last_seen"`
	Fields      map[string]*FieldStats `json:"fields"`
	// Exact codes (the class alone hides 200 vs 201). RefStatusCodes is nil on
	// older baselines, where exact-status drift is not claimed, never guessed.
	StatusCodes    map[string]int `json:"status_codes,omitempty"`
	RefStatusCodes map[string]int `json:"ref_status_codes,omitempty"`
	// Reference holds the frozen copy diffs run against while Fields keeps
	// accumulating live stats.
	Frozen    bool                   `json:"frozen"`
	Reference map[string]*FieldStats `json:"reference,omitempty"`
	FrozenAt  time.Time              `json:"frozen_at,omitempty"`
	// Samples at freeze time: the denominator every Reference count is a ratio
	// of. Zero on older baselines, where presence is not claimed, never guessed.
	FrozenSamples int `json:"frozen_samples,omitempty"`
}

// PresenceRatio of a field in the reference (0 when unknown).
func (f *Family) PresenceRatio(field string) float64 {
	ref := f.Reference
	if ref == nil {
		ref = f.Fields
	}
	st, ok := ref[field]
	if !ok || f.Samples == 0 {
		return 0
	}
	if f.Frozen {
		// Denominator must be samples at freeze time: using the max count would
		// make an all-optional family read ~1.0 and manufacture FieldRemoved.
		if f.FrozenSamples == 0 {
			return 0 // frozen by an older pikopod: not claimed, never guessed
		}
		return float64(st.Count) / float64(f.FrozenSamples)
	}
	return float64(st.Count) / float64(f.Samples)
}

type Warmup struct {
	MinSamples int
	MinAge     time.Duration
}

// Learner owns families for one upstream, plus the cardinality guard.
type Learner struct {
	mu       sync.Mutex
	upstream string
	guard    *pathtmpl.Guard
	warmup   Warmup
	families map[string]*Family
	path     string // persistence file
	now      func() time.Time
	// matcher is the user's volatile_fields, injected by the agent; a match
	// suppresses VALUE tracking only, so presence and type stay asserted.
	matcher VolatileMatcher
	// Value tracking suppressed while presence and type stay asserted:
	// silencing these wholesale would delete coverage.
	valueVolatile map[string]bool
}

// VolatileMatcher decides whether a flattened field path is user-configured
// volatile. Injected by the agent; baseline never imports internal/volatile.
type VolatileMatcher interface {
	Match(path string) (entry string, ok bool)
	RecordDrop(path string)
}

func key(method, template, statusClass string) string {
	return method + " " + template + " " + statusClass
}

func StatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

func NewLearner(upstream, dataDir string, warmup Warmup) *Learner {
	l := &Learner{
		upstream: upstream,
		guard:    pathtmpl.NewGuard(0),
		warmup:   warmup,
		families: map[string]*Family{},
		path:     filepath.Join(dataDir, "baselines", upstream+".json"),
		now:      time.Now,
	}
	l.load()
	return l
}

// Observation is what the differ needs about one record, computed while the
// learner holds its lock.
type Observation struct {
	Family   *Family
	Ready    bool
	Fields   map[string]FieldValue
	Template string
	// The record's exact code, 0 when unknown; compared against RefStatusCodes.
	Status int
}

type FieldValue struct {
	Type  string
	Value string // string values only (post-sanitize); "" otherwise
}

// Observe folds one sanitized record in and returns the observation for
// diffing. Diff-then-absorb: the returned Family reflects the FROZEN state.
func (l *Learner) Observe(method, path string, status int, body any, ts time.Time) Observation {
	l.mu.Lock()
	defer l.mu.Unlock()

	template, promo := l.guard.Apply(path)
	if promo != nil {
		l.mergeFamiliesLocked()
	}
	k := key(method, template, StatusClass(status))
	fam, ok := l.families[k]
	if !ok {
		fam = &Family{Method: method, Template: template, StatusClass: StatusClass(status), Fields: map[string]*FieldStats{}, FirstSeen: ts}
		l.families[k] = fam
	}

	fields := Flatten(body)

	// Freeze check BEFORE absorbing this record: the record that crosses the
	// warmup threshold is the first one diffed against the frozen reference.
	if !fam.Frozen && fam.Samples >= l.warmup.MinSamples && ts.Sub(fam.FirstSeen) >= l.warmup.MinAge {
		fam.freeze(l.now())
	}

	obs := Observation{Family: fam, Ready: fam.Frozen, Fields: fields, Template: template, Status: status}

	fam.Samples++
	fam.LastSeen = ts
	if fam.StatusCodes == nil {
		fam.StatusCodes = map[string]int{}
	}
	fam.StatusCodes[strconv.Itoa(status)]++
	for path, fv := range fields {
		st, ok := fam.Fields[path]
		if !ok {
			// A hostile upstream minting fresh keys per response must not grow
			// memory/disk without bound.
			if len(fam.Fields) >= fieldTrackCap {
				continue
			}
			st = &FieldStats{Types: map[string]int{}}
			fam.Fields[path] = st
		}
		st.Count++
		st.Types[fv.Type]++
		if !st.HighCardinality && l.valueVolatile[strings.ToLower(lastFieldSegment(path))] {
			// Curated value-volatile field: never track values (they churn by
			// nature), but keep counting presence and types above.
			st.Values, st.HighCardinality = nil, true
		}
		if l.matcher != nil {
			if _, volatile := l.matcher.Match(path); volatile {
				st.Values, st.HighCardinality = nil, true
				l.matcher.RecordDrop(path)
				if fv.Type == "string" {
					if st.seenValue && st.lastValue != fv.Value {
						st.Churned = true
					}
					st.lastValue, st.seenValue = fv.Value, true
				}
			}
		}
		if fv.Type == "string" && !st.HighCardinality {
			if st.Values == nil {
				st.Values = map[string]int{}
			}
			st.Values[fv.Value]++
			if len(st.Values) > valueTrackCap {
				st.Values, st.HighCardinality = nil, true
			}
		}
	}
	return obs
}

// SetVolatileMatcher installs the user's compiled volatile_fields. Call
// before observing; the matcher is shared with the agent for Drops().
func (l *Learner) SetVolatileMatcher(m VolatileMatcher) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.matcher = m
}

// SetValueVolatile installs the curated value-volatile names (the agent
// wires internal/volatile's response list). Call before observing.
func (l *Learner) SetValueVolatile(names []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.valueVolatile = map[string]bool{}
	for _, n := range names {
		l.valueVolatile[strings.ToLower(n)] = true
	}
}

// lastFieldSegment: "a/b[]/request_ref" → "request_ref".
func lastFieldSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(path, "[]")
}

// HasFrozen reports whether any family for (method, template) has frozen.
// Dedicated accessor: Families() copies and sorts, too costly per record.
func (l *Learner) HasFrozen(method, template string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, fam := range l.families {
		if fam.Frozen && fam.Method == method && fam.Template == template {
			return true
		}
	}
	return false
}

// Refreeze re-snapshots families from LIVE stats — the "accept this drift"
// primitive, so accepted changes stop alerting rather than relying on dedupe.
func (l *Learner) Refreeze(method, template string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, fam := range l.families {
		if fam.Method == method && fam.Template == template && fam.Frozen {
			fam.freeze(l.now())
			n++
		}
	}
	return n
}

func (f *Family) freeze(at time.Time) {
	ref := make(map[string]*FieldStats, len(f.Fields))
	for k, v := range f.Fields {
		cp := &FieldStats{Count: v.Count, Types: map[string]int{}, HighCardinality: v.HighCardinality}
		for t, c := range v.Types {
			cp.Types[t] = c
		}
		if v.Values != nil {
			cp.Values = map[string]int{}
			for val, c := range v.Values {
				cp.Values[val] = c
			}
		}
		ref[k] = cp
	}
	codes := make(map[string]int, len(f.StatusCodes))
	for c, n := range f.StatusCodes {
		codes[c] = n
	}
	f.RefStatusCodes = codes
	f.Reference, f.Frozen, f.FrozenAt = ref, true, at
	f.FrozenSamples = f.Samples
}

// mergeFamiliesLocked re-keys families after a guard promotion so old
// concrete-path families collapse into the parameterized one.
func (l *Learner) mergeFamiliesLocked() {
	merged := map[string]*Family{}
	for _, fam := range l.families {
		newTemplate := l.guard.MergeKey(fam.Template)
		k := key(fam.Method, newTemplate, fam.StatusClass)
		if exist, ok := merged[k]; ok {
			exist.absorb(fam)
		} else {
			fam.Template = newTemplate
			merged[k] = fam
		}
	}
	l.families = merged
}

func (f *Family) absorb(other *Family) {
	f.Samples += other.Samples
	for c, n := range other.StatusCodes {
		if f.StatusCodes == nil {
			f.StatusCodes = map[string]int{}
		}
		f.StatusCodes[c] += n
	}
	if other.FirstSeen.Before(f.FirstSeen) || f.FirstSeen.IsZero() {
		f.FirstSeen = other.FirstSeen
	}
	if other.LastSeen.After(f.LastSeen) {
		f.LastSeen = other.LastSeen
	}
	for k, st := range other.Fields {
		mine, ok := f.Fields[k]
		if !ok {
			f.Fields[k] = st
			continue
		}
		mine.Count += st.Count
		for t, c := range st.Types {
			mine.Types[t] += c
		}
		if st.HighCardinality {
			mine.Values, mine.HighCardinality = nil, true
		} else if mine.Values != nil && st.Values != nil {
			for v, c := range st.Values {
				mine.Values[v] += c
			}
			if len(mine.Values) > valueTrackCap {
				mine.Values, mine.HighCardinality = nil, true
			}
		}
	}
	// Merged families re-learn their freeze: safer than pretending two
	// references were one.
	f.Frozen, f.Reference = false, nil
}

// Families returns a deterministic snapshot for status/report.
func (l *Learner) Families() []*Family {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Family, 0, len(l.families))
	for _, f := range l.families {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Template != out[j].Template {
			return out[i].Template < out[j].Template
		}
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].StatusClass < out[j].StatusClass
	})
	return out
}

// Reset drops learned state for one template ("" = whole upstream) — the
// atomic-swap re-learn.
func (l *Learner) Reset(template string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for k, fam := range l.families {
		if template == "" || fam.Template == template {
			delete(l.families, k)
			n++
		}
	}
	return n
}

// Persist writes atomically (tmp + rename).
func (l *Learner) Persist() error {
	// Snapshot under the lock, marshal + write OUTSIDE it: MarshalIndent over
	// thousands of families stalled Observe for the whole encode.
	l.mu.Lock()
	snap := make(map[string]*Family, len(l.families))
	for k, fam := range l.families {
		c := *fam
		c.Fields = copyFieldStats(fam.Fields)
		c.Reference = copyFieldStats(fam.Reference)
		c.StatusCodes = copyIntMap(fam.StatusCodes)
		c.RefStatusCodes = copyIntMap(fam.RefStatusCodes)
		snap[k] = &c
	}
	l.mu.Unlock()
	raw, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(l.path, raw)
}

func copyFieldStats(m map[string]*FieldStats) map[string]*FieldStats {
	if m == nil {
		return nil
	}
	out := make(map[string]*FieldStats, len(m))
	for k, st := range m {
		c := *st
		c.Types = copyIntMap(st.Types)
		c.Values = copyIntMap(st.Values)
		out[k] = &c
	}
	return out
}

func copyIntMap(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (l *Learner) load() {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var fams map[string]*Family
	if json.Unmarshal(raw, &fams) == nil && fams != nil {
		l.families = fams
	}
}

// Flatten maps a sanitized JSON tree to fieldPath → FieldValue. Array
// elements collapse to "[]" so lists of objects share field paths.
func Flatten(body any) map[string]FieldValue {
	out := map[string]FieldValue{}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch n := node.(type) {
		case map[string]any:
			if path != "" {
				out[path] = FieldValue{Type: "object"}
			}
			for k, v := range n {
				walk(v, joinField(path, k))
			}
		case []any:
			out[path] = FieldValue{Type: "array"}
			for _, v := range n {
				walk(v, path+"[]")
			}
		case string:
			out[path] = FieldValue{Type: "string", Value: n}
		case bool:
			out[path] = FieldValue{Type: "bool"}
		case nil:
			out[path] = FieldValue{Type: "null"}
		case float64:
			out[path] = FieldValue{Type: "number"}
		case json.Number:
			out[path] = FieldValue{Type: "number"}
		default:
			out[path] = FieldValue{Type: fmt.Sprintf("%T", n)}
		}
	}
	if body != nil {
		walk(body, "")
	}
	delete(out, "")
	return out
}

func joinField(base, k string) string {
	k = strings.ReplaceAll(k, "/", "~1")
	if base == "" {
		return k
	}
	return base + "/" + k
}
