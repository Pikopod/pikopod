// Package contract is the traffic overlay layered beside the spec-derived IR.
// Admissions are journaled and versioned, so ResolveAt reproduces any version.
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

// Caps mirror the baseline discipline: bounded under hostile upstreams.
const (
	maxEndpoints      = 500
	maxFieldsPerKey   = 2000
	maxValuesPerField = 24
	maxContradictions = 200
	maxAdmissions     = 5000
)

// ObservedField is one field's accumulated traffic evidence.
type ObservedField struct {
	Seen    int64            `json:"seen"`             // records containing the field
	Samples int64            `json:"samples"`          // records for the endpoint while tracked
	Types   map[string]int64 `json:"types,omitempty"`  // ALLOW-mode only
	Values  map[string]int64 `json:"values,omitempty"` // ALLOW-mode strings only, capped
	// HighCardinality latches when Values exceeds the cap; values reset.
	HighCardinality bool `json:"highCardinality,omitempty"`
	// Redactions counts non-ALLOW sightings per mode — evidence the field
	// exists even though its content never reached disk.
	Redactions map[string]int64 `json:"redactions,omitempty"`
}

func (f *ObservedField) PresenceRate() float64 {
	if f.Samples == 0 {
		return 0
	}
	return float64(f.Seen) / float64(f.Samples)
}

// ObservedEndpoint accumulates per (method, template, statusClass).
type ObservedEndpoint struct {
	Method      string                    `json:"method"`
	Template    string                    `json:"template"`
	StatusClass string                    `json:"statusClass"`
	Samples     int64                     `json:"samples"`
	FirstSeen   time.Time                 `json:"firstSeen"`
	LastSeen    time.Time                 `json:"lastSeen"`
	Fields      map[string]*ObservedField `json:"fields,omitempty"`
	StatusCodes map[string]int64          `json:"statusCodes,omitempty"`
	FieldsLatch bool                      `json:"fieldsLatch,omitempty"` // maxFieldsPerKey hit
}

// AdmissionKind enumerates the ways traffic changes the effective contract.
type AdmissionKind string

const (
	AdmitField    AdmissionKind = "field"    // undeclared field joins the contract
	AdmitValue    AdmissionKind = "value"    // observed enum value joins a field
	AdmitType     AdmissionKind = "type"     // traffic-wins type override (gated)
	AdmitStatus   AdmissionKind = "status"   // undeclared status code
	AdmitEndpoint AdmissionKind = "endpoint" // undeclared endpoint template
)

// Admission is one journaled change to the effective contract. The journal
// is append-only; Version is monotonic across the whole overlay.
type Admission struct {
	Version     int           `json:"version"`
	Kind        AdmissionKind `json:"kind"`
	Method      string        `json:"method"`
	Template    string        `json:"template"`
	StatusClass string        `json:"statusClass"`
	Field       string        `json:"field,omitempty"`
	Value       string        `json:"value,omitempty"`
	Type        string        `json:"type,omitempty"`
	// Presence at admission time (confidence for the resolved Prov).
	Presence float64   `json:"presence,omitempty"`
	At       time.Time `json:"at"`
	// SourceKey is the traffic-form overlay key the admission came from — how
	// ResolveAt finds observed stats when Template is the spec's spelling.
	SourceKey string `json:"sourceKey,omitempty"`
}

// Contradiction records spec-vs-traffic disagreement. Never auto-erased:
// `pikopod contract` shows both sides.
type Contradiction struct {
	Method      string    `json:"method"`
	Template    string    `json:"template"`
	StatusClass string    `json:"statusClass"`
	Field       string    `json:"field"`
	SpecClaim   string    `json:"specClaim"` // e.g. declared type
	Observed    string    `json:"observed"`  // dominant observed type
	Rate        float64   `json:"rate"`      // fraction of ALLOW samples agreeing with Observed
	FirstSeen   time.Time `json:"firstSeen"`
	// Winner: "traffic" (gates cleared, admitted) or "spec" (still gated).
	Winner string `json:"winner"`
}

// Overlay is the persisted traffic half of the behavioral model.
type Overlay struct {
	Upstream       string                       `json:"upstream"`
	Version        int                          `json:"version"`   // last admitted version
	Endpoints      map[string]*ObservedEndpoint `json:"endpoints"` // key: METHOD|template|class
	Admissions     []Admission                  `json:"admissions"`
	Contradictions []Contradiction              `json:"contradictions,omitempty"`
	UpdatedAt      time.Time                    `json:"updatedAt"`
}

func endpointKey(method, template, statusClass string) string {
	return strings.ToUpper(method) + "|" + template + "|" + statusClass
}

// Refiner grows the overlay from the sanitized record stream — a SIBLING of the
// drift learner, never a reader of its state. Admission gates run on Persist.
type Refiner struct {
	mu      sync.Mutex
	overlay *Overlay
	guard   *pathtmpl.Guard
	path    string
	// Gates (config): an endpoint may admit only after warmup, and a field
	// only at sufficient presence.
	minSamples    int
	minAge        time.Duration
	presenceFloor float64
	now           func() time.Time
}

// NewRefiner loads or starts the overlay for one upstream.
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

// LoadOverlay reads a persisted overlay (nil, nil when none exists) — the
// renderer's entry point; it never needs a live Refiner.
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

// Observe folds one sanitized record into the overlay stats. It NEVER
// admits — admission is a separate, journaled act (Admit).
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

	// Sanitizer-aware: surviving fields teach presence (ALLOW also type+value) and
	// redaction pointers teach it for content that never reached disk.
	seen := map[string]bool{}
	walkFields(rec.RespBody, "", func(path string, value any) {
		f := r.fieldFor(ep, path)
		if f == nil {
			return
		}
		seen[path] = true
		f.Seen++
		if !isAllowedVerbatim(path, rec) {
			return // tokenized/etc: presence only
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
	// Samples per field lag endpoint samples from when tracking began.
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

// Persist writes the overlay atomically (0600).
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
		return "0" // status codes only; anything else is a caller bug
	}
	return strconv.Itoa(n)
}

// walkFields visits every leaf with dotted paths; array elements collapse to
// "[]" like baseline.Flatten so lists of objects share field identity.
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

// isAllowedVerbatim: was the value stored as the provider sent it? True unless a
// redaction pointer covers it (pointers use "/", field paths "." — normalize).
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

// sortedKeys for deterministic iteration in admission passes.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
