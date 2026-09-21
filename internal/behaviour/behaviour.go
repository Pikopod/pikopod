package behaviour

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/pathtmpl"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/store"
)

const (
	maxValuesPerField = 24
	maxEdgesPerField  = 64
	maxFieldsPerKey   = 200
	maxEndpoints      = 500
	maxRemembered     = 20000
)

type Edge struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	Count    int64     `json:"count"`
	LastSeen time.Time `json:"lastSeen"`
}

type Field struct {
	Samples     int64            `json:"samples"`
	Transitions int64            `json:"transitions"`
	Values      map[string]int64 `json:"values,omitempty"`
	Edges       map[string]*Edge `json:"edges,omitempty"`
	Latched     bool             `json:"latched,omitempty"`
	FirstSeen   time.Time        `json:"firstSeen"`
	LastSeen    time.Time        `json:"lastSeen"`
}

type Endpoint struct {
	Method    string            `json:"method"`
	Template  string            `json:"template"`
	Samples   int64             `json:"samples"`
	Fields    map[string]*Field `json:"fields"`
	FirstSeen time.Time         `json:"firstSeen"`
	LastSeen  time.Time         `json:"lastSeen"`
}

type Graph struct {
	Upstream  string               `json:"upstream"`
	Endpoints map[string]*Endpoint `json:"endpoints"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

type Matcher interface {
	Match(path string) (string, bool)
}

type Tracker struct {
	mu         sync.Mutex
	graph      *Graph
	guard      *pathtmpl.Guard
	path       string
	minSamples int
	minAge     time.Duration
	now        func() time.Time
	volatile   Matcher
	muted      map[string]bool
	last       map[string]string
	order      []string
}

func New(upstream, dataDir string, minSamples int, minAge time.Duration) *Tracker {
	t := &Tracker{
		graph:      &Graph{Upstream: upstream, Endpoints: map[string]*Endpoint{}},
		guard:      pathtmpl.NewGuard(0),
		path:       filePath(dataDir, upstream),
		minSamples: minSamples,
		minAge:     minAge,
		now:        time.Now,
		last:       map[string]string{},
	}
	if raw, err := os.ReadFile(t.path); err == nil {
		var g Graph
		if json.Unmarshal(raw, &g) == nil && g.Endpoints != nil {
			t.graph = &g
		}
	}
	return t
}

func filePath(dataDir, upstream string) string {
	return filepath.Join(dataDir, "apis", upstream+".behaviour.json")
}

func (t *Tracker) SetClock(now func() time.Time) { t.now = now }

func (t *Tracker) SetVolatile(m Matcher) { t.volatile = m }

func (t *Tracker) SetMuted(templates map[string]bool) { t.muted = templates }

func (t *Tracker) Observe(rec *proxy.Record) {
	if rec.RespKind != "json" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	pathOnly := rec.Path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	template, _ := t.guard.Apply(pathOnly)
	if t.muted[template] {
		return
	}
	resource, params := resourceKey(template, pathOnly)
	if resource == "" {
		return
	}
	now := t.now()
	k := strings.ToUpper(rec.Method) + "|" + template
	ep := t.graph.Endpoints[k]
	if ep == nil {
		if len(t.graph.Endpoints) >= maxEndpoints {
			return
		}
		ep = &Endpoint{Method: strings.ToUpper(rec.Method), Template: template, Fields: map[string]*Field{}, FirstSeen: now}
		t.graph.Endpoints[k] = ep
	}
	ep.Samples++
	ep.LastSeen = now
	redacted := map[string]bool{}
	for _, r := range rec.Redacted {
		if r.Section == "resp_body" {
			redacted[strings.ReplaceAll(strings.TrimPrefix(r.Pointer, "/"), "/", ".")] = true
		}
	}
	walk(rec.RespBody, "", func(path string, value any) {
		s, ok := value.(string)
		if !ok || redacted[path] || params[s] {
			return
		}
		if t.volatile != nil {
			if _, drop := t.volatile.Match(strings.ReplaceAll(path, ".", "/")); drop {
				return
			}
		}
		f := ep.Fields[path]
		if f == nil {
			if len(ep.Fields) >= maxFieldsPerKey {
				return
			}
			f = &Field{Values: map[string]int64{}, Edges: map[string]*Edge{}, FirstSeen: now}
			ep.Fields[path] = f
		}
		f.Samples++
		f.LastSeen = now
		if f.Latched {
			return
		}
		f.Values[s]++
		if len(f.Values) > maxValuesPerField {
			t.latch(f)
			return
		}
		cacheKey := resource + "|" + path
		prev, seen := t.last[cacheKey]
		t.remember(cacheKey, s)
		if !seen || prev == s {
			return
		}
		id := prev + "→" + s
		e := f.Edges[id]
		if e == nil {
			if len(f.Edges) >= maxEdgesPerField {
				t.latch(f)
				return
			}
			e = &Edge{From: prev, To: s}
			f.Edges[id] = e
		}
		e.Count++
		e.LastSeen = now
		f.Transitions++
	})
}

func (t *Tracker) latch(f *Field) {
	f.Latched = true
	f.Values, f.Edges, f.Transitions = nil, nil, 0
}

func (t *Tracker) remember(key, value string) {
	if _, ok := t.last[key]; !ok {
		t.order = append(t.order, key)
		if len(t.order) > maxRemembered {
			delete(t.last, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.last[key] = value
}

func resourceKey(template, path string) (string, map[string]bool) {
	ts, ps := strings.Split(template, "/"), strings.Split(path, "/")
	if len(ts) != len(ps) {
		return "", nil
	}
	var params []string
	set := map[string]bool{}
	for i, seg := range ts {
		if strings.Contains(seg, "{") {
			params = append(params, ps[i])
			set[ps[i]] = true
		}
	}
	if len(params) == 0 {
		return "", nil
	}
	return template + "|" + strings.Join(params, "|"), set
}

func walk(node any, path string, visit func(string, any)) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walk(v, p, visit)
		}
	case []any:
		for _, v := range n {
			walk(v, path+"[]", visit)
		}
	default:
		if path != "" {
			visit(path, node)
		}
	}
}

func (t *Tracker) Persist() error {
	t.mu.Lock()
	t.graph.UpdatedAt = t.now()
	raw, err := json.MarshalIndent(t.graph, "", " ")
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(t.path, raw); err != nil {
		return errfmt.Newf("cannot persist the behaviour graph", "check permissions on "+t.path, "docs/config-reference.md#behaviour", "%v", err)
	}
	return nil
}

func (t *Tracker) Snapshot() *Graph {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, _ := json.Marshal(t.graph)
	var out Graph
	json.Unmarshal(raw, &out)
	return &out
}

func Load(dataDir, upstream string) (*Graph, error) {
	raw, err := os.ReadFile(filePath(dataDir, upstream))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var g Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, errfmt.Newf("behaviour graph is corrupt", "fix or remove the file to relearn", "docs/config-reference.md#behaviour", "%v", err)
	}
	return &g, nil
}

type FieldReport struct {
	Method        string    `json:"method"`
	Template      string    `json:"template"`
	Field         string    `json:"field"`
	Transitions   int64     `json:"transitions"`
	Samples       int64     `json:"samples"`
	Since         time.Time `json:"since"`
	WarmedUp      bool      `json:"warmedUp"`
	Latched       bool      `json:"latched,omitempty"`
	Edges         []Edge    `json:"edges"`
	NeverObserved []string  `json:"neverObserved,omitempty"`
}

type Report struct {
	Upstream  string        `json:"upstream"`
	UpdatedAt time.Time     `json:"updatedAt"`
	Fields    []FieldReport `json:"fields"`
}

func BuildReport(g *Graph, minSamples int, minAge time.Duration, now time.Time) *Report {
	rep := &Report{Upstream: g.Upstream, UpdatedAt: g.UpdatedAt, Fields: []FieldReport{}}
	for _, ep := range g.Endpoints {
		for name, f := range ep.Fields {
			if f.Latched || len(f.Edges) == 0 {
				continue
			}
			fr := FieldReport{Method: ep.Method, Template: ep.Template, Field: name, Transitions: f.Transitions, Samples: f.Samples, Since: f.FirstSeen, Latched: f.Latched}
			fr.WarmedUp = f.Samples >= int64(minSamples) && now.Sub(f.FirstSeen) >= minAge
			for _, e := range f.Edges {
				fr.Edges = append(fr.Edges, *e)
			}
			sort.Slice(fr.Edges, func(i, j int) bool {
				if fr.Edges[i].Count != fr.Edges[j].Count {
					return fr.Edges[i].Count > fr.Edges[j].Count
				}
				return fr.Edges[i].From+fr.Edges[i].To < fr.Edges[j].From+fr.Edges[j].To
			})
			if fr.WarmedUp {
				fr.NeverObserved = neverObserved(f)
			}
			rep.Fields = append(rep.Fields, fr)
		}
	}
	sort.Slice(rep.Fields, func(i, j int) bool {
		a, b := rep.Fields[i], rep.Fields[j]
		return a.Method+a.Template+a.Field < b.Method+b.Template+b.Field
	})
	return rep
}

func neverObserved(f *Field) []string {
	states := map[string]bool{}
	for _, e := range f.Edges {
		states[e.From], states[e.To] = true, true
	}
	names := make([]string, 0, len(states))
	for s := range states {
		names = append(names, s)
	}
	sort.Strings(names)
	var out []string
	for _, a := range names {
		for _, b := range names {
			if a == b {
				continue
			}
			if _, seen := f.Edges[a+"→"+b]; !seen {
				out = append(out, a+" → "+b)
			}
		}
	}
	return out
}
