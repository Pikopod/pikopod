package drift

import "github.com/pikopod/pikopod/internal/baseline"

type OfflineFinding struct {
	Kind   string
	Field  string
	Detail string
}

func DiffRecord(fam *baseline.Family, status int, body any) []OfflineFinding {
	if fam == nil || !fam.Frozen {
		return nil
	}
	obs := baseline.Observation{Family: fam, Ready: true, Fields: baseline.Flatten(body), Template: fam.Template, Status: status}
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
