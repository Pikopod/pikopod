package drift

import "github.com/pikopod/pikopod/internal/baseline"

// OfflineFinding is the CI-gate view of a divergence (no upstream context —
// the caller owns that).
type OfflineFinding struct {
	Kind   string
	Field  string
	Detail string
}

// DiffRecord diffs one sanitized body against a FROZEN family's reference,
// read-only — the offline half of the differ. status 0 skips exact-status.
func DiffRecord(fam *baseline.Family, status int, body any) []OfflineFinding {
	if fam == nil || !fam.Frozen {
		return nil
	}
	obs := baseline.Observation{Family: fam, Ready: true, Fields: baseline.Flatten(body), Template: fam.Template, Status: status}
	// knownStatusClasses: the family exists, so its class is known — StatusNew
	// is a live-only signal (the gate diffs within known families).
	findings := Diff("", obs, map[string]bool{fam.StatusClass: true})
	out := make([]OfflineFinding, 0, len(findings))
	for _, f := range findings {
		detail := f.After
		if f.Kind == FieldRemoved {
			detail = f.Before
		}
		out = append(out, OfflineFinding{Kind: string(f.Kind), Field: f.Field, Detail: detail})
	}
	return out
}
