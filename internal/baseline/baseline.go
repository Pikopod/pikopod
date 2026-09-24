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

const valueTrackCap = 24

const fieldTrackCap = 2000

type FieldStats struct {
	Count  int            `json:"count"`
	Types  map[string]int `json:"types"`
	Values map[string]int `json:"values,omitempty"`

	HighCardinality bool `json:"high_cardinality,omitempty"`

	Churned bool `json:"churned,omitempty"`

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

	StatusCodes    map[string]int `json:"status_codes,omitempty"`
	RefStatusCodes map[string]int `json:"ref_status_codes,omitempty"`

	Frozen    bool                   `json:"frozen"`
	Reference map[string]*FieldStats `json:"reference,omitempty"`
	FrozenAt  time.Time              `json:"frozen_at,omitempty"`

	FrozenSamples int `json:"frozen_samples,omitempty"`
}

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

		if f.FrozenSamples == 0 {
			return 0
		}
		return float64(st.Count) / float64(f.FrozenSamples)
	}
	return float64(st.Count) / float64(f.Samples)
}

type Warmup struct {
	MinSamples int
	MinAge     time.Duration
}

type Learner struct {
	mu       sync.Mutex
	upstream string
	guard    *pathtmpl.Guard
	warmup   Warmup
	families map[string]*Family
	path     string
	now      func() time.Time

	matcher VolatileMatcher

	valueVolatile map[string]bool
}

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

type Observation struct {
	Family   *Family
	Ready    bool
	Fields   map[string]FieldValue
	Template string

	Status int
}

type FieldValue struct {
	Type  string
	Value string
}

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

			if len(fam.Fields) >= fieldTrackCap {
				continue
			}
			st = &FieldStats{Types: map[string]int{}}
			fam.Fields[path] = st
		}
		st.Count++
		st.Types[fv.Type]++
		if !st.HighCardinality && l.valueVolatile[strings.ToLower(lastFieldSegment(path))] {

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

func (l *Learner) SetVolatileMatcher(m VolatileMatcher) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.matcher = m
}

func (l *Learner) SetValueVolatile(names []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.valueVolatile = map[string]bool{}
	for _, n := range names {
		l.valueVolatile[strings.ToLower(n)] = true
	}
}

func lastFieldSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(path, "[]")
}

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

	f.Frozen, f.Reference = false, nil
}

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

func (l *Learner) Persist() error {

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
