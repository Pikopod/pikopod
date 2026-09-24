package contract

import (
	"encoding/json"
	"github.com/pikopod/pikopod/internal/store"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/pathtmpl"
	"github.com/pikopod/pikopod/internal/proxy"
)

const (
	maxEndpoints      = 500
	maxFieldsPerKey   = 2000
	maxValuesPerField = 24
	maxContradictions = 200
	maxAdmissions     = 5000
)

type ObservedField struct {
	Seen    int64            `json:"seen"`
	Samples int64            `json:"samples"`
	Types   map[string]int64 `json:"types,omitempty"`
	Values  map[string]int64 `json:"values,omitempty"`

	HighCardinality bool `json:"highCardinality,omitempty"`

	Redactions map[string]int64 `json:"redactions,omitempty"`
}

func (f *ObservedField) PresenceRate() float64 {
	if f.Samples == 0 {
		return 0
	}
	return float64(f.Seen) / float64(f.Samples)
}

type ObservedEndpoint struct {
	Method      string                    `json:"method"`
	Template    string                    `json:"template"`
	StatusClass string                    `json:"statusClass"`
	Samples     int64                     `json:"samples"`
	FirstSeen   time.Time                 `json:"firstSeen"`
	LastSeen    time.Time                 `json:"lastSeen"`
	Fields      map[string]*ObservedField `json:"fields,omitempty"`
	StatusCodes map[string]int64          `json:"statusCodes,omitempty"`
	FieldsLatch bool                      `json:"fieldsLatch,omitempty"`
}

type AdmissionKind string

const (
	AdmitField    AdmissionKind = "field"
	AdmitValue    AdmissionKind = "value"
	AdmitType     AdmissionKind = "type"
	AdmitStatus   AdmissionKind = "status"
	AdmitEndpoint AdmissionKind = "endpoint"
)

type Admission struct {
	Version     int           `json:"version"`
	Kind        AdmissionKind `json:"kind"`
	Method      string        `json:"method"`
	Template    string        `json:"template"`
	StatusClass string        `json:"statusClass"`
	Field       string        `json:"field,omitempty"`
	Value       string        `json:"value,omitempty"`
	Type        string        `json:"type,omitempty"`

	Presence float64   `json:"presence,omitempty"`
	At       time.Time `json:"at"`

	SourceKey string `json:"sourceKey,omitempty"`
}

type Contradiction struct {
	Method      string    `json:"method"`
	Template    string    `json:"template"`
	StatusClass string    `json:"statusClass"`
	Field       string    `json:"field"`
	SpecClaim   string    `json:"specClaim"`
	Observed    string    `json:"observed"`
	Rate        float64   `json:"rate"`
	FirstSeen   time.Time `json:"firstSeen"`

	Winner string `json:"winner"`
}

type Overlay struct {
	Upstream       string                       `json:"upstream"`
	Version        int                          `json:"version"`
	Endpoints      map[string]*ObservedEndpoint `json:"endpoints"`
	Admissions     []Admission                  `json:"admissions"`
	Contradictions []Contradiction              `json:"contradictions,omitempty"`
	UpdatedAt      time.Time                    `json:"updatedAt"`
}

func endpointKey(method, template, statusClass string) string {
	return strings.ToUpper(method) + "|" + template + "|" + statusClass
}

type Refiner struct {
	mu      sync.Mutex
	overlay *Overlay
	guard   *pathtmpl.Guard
	path    string

	minSamples    int
	minAge        time.Duration
	presenceFloor float64
	now           func() time.Time
}

func NewRefiner(upstream, dataDir string, minSamples int, minAge time.Duration) *Refiner {
	r := &Refiner{
		overlay:       &Overlay{Upstream: upstream, Endpoints: map[string]*ObservedEndpoint{}},
		guard:         pathtmpl.NewGuard(0),
		path:          filepath.Join(dataDir, "apis", upstream+".observed.json"),
		minSamples:    minSamples,
		minAge:        minAge,
		presenceFloor: 0.20,
		now:           time.Now,
	}
	if raw, err := os.ReadFile(r.path); err == nil {
		var ov Overlay
		if json.Unmarshal(raw, &ov) == nil && ov.Endpoints != nil {
			r.overlay = &ov
		}
	}
	return r
}

func LoadOverlay(dataDir, upstream string) (*Overlay, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "apis", upstream+".observed.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ov Overlay
	if err := json.Unmarshal(raw, &ov); err != nil {
		return nil, errfmt.Newf("traffic overlay is corrupt", "fix or remove the file to relearn", "docs/config-reference.md#refine", "%v", err)
	}
	return &ov, nil
}

func (r *Refiner) SetClock(now func() time.Time) { r.now = now }

func (r *Refiner) Observe(rec *proxy.Record) {
	if rec.RespKind != "json" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	pathOnly := rec.Path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	template, _ := r.guard.Apply(pathOnly)
	class := statusClass(rec.Status)
	k := endpointKey(rec.Method, template, class)
	ep := r.overlay.Endpoints[k]
	if ep == nil {
		if len(r.overlay.Endpoints) >= maxEndpoints {
			return
		}
		ep = &ObservedEndpoint{
			Method: strings.ToUpper(rec.Method), Template: template, StatusClass: class,
			Fields: map[string]*ObservedField{}, StatusCodes: map[string]int64{},
			FirstSeen: r.now(),
		}
		r.overlay.Endpoints[k] = ep
	}
	ep.Samples++
	ep.LastSeen = r.now()
	ep.StatusCodes[itoa(rec.Status)]++

	seen := map[string]bool{}
	walkFields(rec.RespBody, "", func(path string, value any) {
		f := r.fieldFor(ep, path)
		if f == nil {
			return
		}
		seen[path] = true
		f.Seen++
		if !isAllowedVerbatim(path, rec) {
			return
		}
		t := jsonTypeOf(value)
		if f.Types == nil {
			f.Types = map[string]int64{}
		}
		f.Types[t]++
		if s, ok := value.(string); ok && !f.HighCardinality {
			if f.Values == nil {
				f.Values = map[string]int64{}
			}
			f.Values[s]++
			if len(f.Values) > maxValuesPerField {
				f.Values, f.HighCardinality = nil, true
			}
		}
	})
	for _, red := range rec.Redacted {
		if red.Section != "resp_body" {
			continue
		}
		path := strings.TrimPrefix(red.Pointer, "/")
		path = strings.ReplaceAll(path, "/", ".")
		if seen[path] {
			continue
		}
		f := r.fieldFor(ep, path)
		if f == nil {
			continue
		}
		f.Seen++
		if f.Redactions == nil {
			f.Redactions = map[string]int64{}
		}
		f.Redactions[red.Mode]++
	}

	for _, f := range ep.Fields {
		f.Samples++
	}
}

func (r *Refiner) fieldFor(ep *ObservedEndpoint, path string) *ObservedField {
	f, ok := ep.Fields[path]
	if !ok {
		if ep.FieldsLatch || len(ep.Fields) >= maxFieldsPerKey {
			ep.FieldsLatch = true
			return nil
		}
		f = &ObservedField{}
		ep.Fields[path] = f
	}
	return f
}

func (r *Refiner) Persist() error {
	r.mu.Lock()
	r.overlay.UpdatedAt = r.now()
	raw, err := json.MarshalIndent(r.overlay, "", " ")
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(r.path, raw); err != nil {
		return errfmt.Newf("cannot persist the traffic overlay", "check permissions on "+r.path, "docs/config-reference.md#refine", "%v", err)
	}
	return nil
}

func (r *Refiner) Snapshot() *Overlay {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := json.Marshal(r.overlay)
	var out Overlay
	json.Unmarshal(raw, &out)
	return &out
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

func itoa(n int) string {
	if n < 100 || n > 999 {
		return "0"
	}
	return strconv.Itoa(n)
}

func walkFields(node any, path string, visit func(path string, value any)) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walkFields(v, p, visit)
		}
	case []any:
		for _, v := range n {
			walkFields(v, path+"[]", visit)
		}
	default:
		if path != "" {
			visit(path, node)
		}
	}
}

func isAllowedVerbatim(fieldPath string, rec *proxy.Record) bool {
	needle := "/" + strings.ReplaceAll(fieldPath, ".", "/")
	for _, red := range rec.Redacted {
		if red.Section == "resp_body" && red.Pointer == needle {
			return false
		}
	}
	return true
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "object"
	}
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
