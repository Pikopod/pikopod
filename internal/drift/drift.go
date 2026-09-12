// Package drift diffs traffic against frozen baseline references. Alerts are
// STRUCTURE ONLY; latency is recorded but never alerted (no certain claim).
package drift

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/baseline"
)

type Kind string

const (
	FieldAdded   Kind = "field_added"
	FieldRemoved Kind = "field_removed"
	TypeChanged  Kind = "type_changed"
	EnumValueNew Kind = "enum_value_new"
	StatusNew    Kind = "status_new"
	// A never-null field arrived null. Its own kind, not TypeChanged: a
	// different break, a different fix, its own fingerprint under dedupe.
	FieldNullable Kind = "field_nullable"
	// The exact code moved within a known class (200→201 is invisible to
	// StatusNew, which compares classes).
	StatusCodeChanged Kind = "status_code_changed"
	// A 4xx/5xx body was restructured. Collapsed to one finding rather than a
	// flood of adds+removes: an error-format swap is one event, not N.
	ErrorShapeChanged Kind = "error_shape_changed"
)

// presenceFloor: a field only counts as "removed" if it was present in
// (nearly) every reference sample — optional fields are not removals.
const presenceFloor = 0.98

// Finding is one structural divergence in one record.
type Finding struct {
	Upstream    string `json:"upstream"`
	Method      string `json:"method"`
	Template    string `json:"template"`
	StatusClass string `json:"status_class"`
	Kind        Kind   `json:"kind"`
	Field       string `json:"field,omitempty"`
	Before      string `json:"before,omitempty"`
	After       string `json:"after,omitempty"`
	// Declared×observed join context. Presentation only — NEVER part of the
	// fingerprint, so annotating a finding cannot re-alert it.
	Note string `json:"note,omitempty"`
	// The provider's new spec version declares this change, so severity
	// routing treats it as informational: announced, not silent.
	Documented bool `json:"documented,omitempty"`
}

// Fingerprint identifies a drift stably across occurrences. Components are
// escaped so a "|" inside one cannot shift the component boundaries.
func (f Finding) Fingerprint() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s",
		fpEscape(f.Upstream), fpEscape(f.Method), fpEscape(f.Template), fpEscape(f.StatusClass),
		fpEscape(string(f.Kind)), fpEscape(f.Field), fpEscape(f.Before), fpEscape(f.After))))
	return "fp_" + hex.EncodeToString(h[:])[:12]
}

func fpEscape(s string) string {
	if !strings.ContainsAny(s, `\|`) {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "|", `\|`)
}

// Diff compares one observation against its frozen reference; caller
// guarantees obs.Ready. StatusNew fires on a class never seen for the endpoint.
func Diff(upstream string, obs baseline.Observation, knownStatusClasses map[string]bool) []Finding {
	fam := obs.Family
	ref := fam.Reference
	var out []Finding

	mk := func(kind Kind, field, before, after string) Finding {
		return Finding{Upstream: upstream, Method: fam.Method, Template: fam.Template, StatusClass: fam.StatusClass, Kind: kind, Field: field, Before: before, After: after}
	}

	if !knownStatusClasses[fam.StatusClass] {
		out = append(out, mk(StatusNew, "", "", fam.StatusClass))
	}

	// Claimed only when the frozen reference tracked codes: older baselines
	// and caller-less statuses skip rather than guess.
	if obs.Status != 0 && len(fam.RefStatusCodes) > 0 {
		code := strconv.Itoa(obs.Status)
		if _, seen := fam.RefStatusCodes[code]; !seen {
			out = append(out, mk(StatusCodeChanged, "", knownCodes(fam.RefStatusCodes), code))
		}
	}

	// Deterministic field order keeps emission order stable across runs.
	paths := make([]string, 0, len(obs.Fields))
	for p := range obs.Fields {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var added, fieldFindings []Finding
	for _, path := range paths {
		fv := obs.Fields[path]
		st, known := ref[path]
		if !known {
			added = append(added, mk(FieldAdded, path, "", fv.Type))
			continue
		}
		if _, typeKnown := st.Types[fv.Type]; !typeKnown {
			if fv.Type == "null" {
				// Never-null field arrived null: nullability, not a type swap.
				fieldFindings = append(fieldFindings, mk(FieldNullable, path, dominantType(st), "null"))
			} else {
				fieldFindings = append(fieldFindings, mk(TypeChanged, path, dominantType(st), fv.Type))
			}
			continue
		}
		if fv.Type == "string" && !st.HighCardinality && st.Values != nil {
			if _, seen := st.Values[fv.Value]; !seen {
				fieldFindings = append(fieldFindings, mk(EnumValueNew, path, knownValues(st), fv.Value))
			}
		}
	}

	// Removals: reference fields with ~always-there presence missing from
	// this record. Presence-frequency keeps optional fields quiet.
	var removed []Finding
	refPaths := make([]string, 0, len(ref))
	for p := range ref {
		refPaths = append(refPaths, p)
	}
	sort.Strings(refPaths)
	for _, path := range refPaths {
		if _, present := obs.Fields[path]; present {
			continue
		}
		if fam.PresenceRatio(path) >= presenceFloor && ref[path].Count > 1 {
			removed = append(removed, mk(FieldRemoved, path, dominantType(ref[path]), ""))
		}
	}

	// Adds AND removes together on an error family means a format swap: one
	// event, one fingerprint, subsuming the add/remove findings it replaces.
	if isErrorClass(fam.StatusClass) && len(added) > 0 && len(removed) > 0 {
		out = append(out, mk(ErrorShapeChanged, "", shapeOf(refPaths), shapeOf(paths)))
		return append(out, fieldFindings...)
	}
	out = append(out, added...)
	out = append(out, fieldFindings...)
	out = append(out, removed...)
	return out
}

func isErrorClass(class string) bool { return class == "4xx" || class == "5xx" }

// knownCodes renders the frozen exact-code set compactly (sorted — the
// fingerprint depends on this string being stable).
func knownCodes(codes map[string]int) string {
	out := make([]string, 0, len(codes))
	for c := range codes {
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// shapeOf renders a field-path set compactly and deterministically (sorted,
// capped): two records with the same restructured shape share a fingerprint.
func shapeOf(sortedPaths []string) string {
	if len(sortedPaths) > 8 {
		sortedPaths = append(sortedPaths[:8:8], "…")
	}
	return strings.Join(sortedPaths, ",")
}

func dominantType(st *baseline.FieldStats) string {
	best, bestN := "", -1
	for t, n := range st.Types {
		if n > bestN || (n == bestN && t < best) {
			best, bestN = t, n
		}
	}
	return best
}

// knownValues renders the reference value set compactly (sorted, capped).
func knownValues(st *baseline.FieldStats) string {
	vals := make([]string, 0, len(st.Values))
	for v := range st.Values {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	if len(vals) > 6 {
		vals = append(vals[:6], "…")
	}
	out := ""
	for i, v := range vals {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
