// The declared×observed join: spec-diff knows what the provider DECLARED, the
// learner what traffic does. Presentation only — fingerprints never change.
package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/pikopod/pikopod/internal/specwatch"
)

// AnnotateDocumented downgrades observed findings whose change the
// provider's new spec version declares (per the specwatch journal).
func AnnotateDocumented(findings []drift.Finding, doc *specwatch.Documented) {
	if doc == nil {
		return
	}
	for i := range findings {
		f := &findings[i]
		documented := false
		switch f.Kind {
		case drift.FieldAdded:
			documented = doc.HasFieldAdded(f.Method, f.Template, f.Field)
		case drift.EnumValueNew:
			documented = doc.HasEnumValueAdded(f.Method, f.Template, f.Field, f.After)
		case drift.StatusNew, drift.StatusCodeChanged:
			documented = doc.HasStatusAdded(f.Method, f.Template, f.After)
		}
		if documented {
			f.Documented = true
			f.Note = "documented: the provider's new spec version declares this change"
		}
	}
}

// EnrichDeclaredFinding joins observed-traffic evidence onto a declared finding.
// Response-side only — the join never guesses, and Level only ever RISES.
func EnrichDeclaredFinding(f *specdiff.Finding, fams []*baseline.Family) {
	matching := func(statusClass string) []*baseline.Family {
		var out []*baseline.Family
		for _, fam := range fams {
			if fam.Method != f.Method {
				continue
			}
			if specwatch.CanonicalTemplate(fam.Template) != specwatch.CanonicalTemplate(f.Template) {
				continue
			}
			if statusClass != "" && fam.StatusClass != statusClass {
				continue
			}
			out = append(out, fam)
		}
		return out
	}

	switch f.ID {
	case "endpoint-removed":
		total, last := 0, time.Time{}
		for _, fam := range matching("") {
			total += fam.Samples
			if fam.LastSeen.After(last) {
				last = fam.LastSeen
			}
		}
		if total > 0 {
			f.Detail += fmt.Sprintf(" — STILL RECEIVING TRAFFIC (%d samples, last seen %s)", total, last.UTC().Format(time.RFC3339))
			raiseTo(f, specdiff.Err)
		}

	case "response-required-property-removed", "response-property-removed":
		fam, path := famAndPath(f, matching)
		if fam == nil {
			return
		}
		if _, live := fam.Fields[path]; !live {
			return
		}
		ratio := fam.PresenceRatio(path)
		f.Detail += fmt.Sprintf(" — consumers receive this field today (presence %.0f%%, %d samples)", ratio*100, fam.Samples)
		// Same floor as drift's presenceFloor: ~always-present is the bar for
		// "consumers depend on it".
		if ratio >= presenceEvidenceFloor {
			raiseTo(f, specdiff.Err)
		}

	case "response-enum-value-removed":
		fam, path := famAndPath(f, matching)
		if fam == nil || len(f.Args) < 3 {
			return
		}
		if st, ok := fam.Fields[path]; ok && st.Values != nil {
			if _, carried := st.Values[f.Args[2]]; carried {
				f.Detail += " — traffic STILL carries this value; the spec now disagrees with the wire"
				raiseTo(f, specdiff.Warn)
			}
		}

	case "response-status-removed":
		if len(f.Args) < 1 {
			return
		}
		code := f.Args[0]
		for _, fam := range matching("") {
			if fam.StatusCodes[code] > 0 {
				f.Detail += fmt.Sprintf(" — traffic STILL returns %s (%d times); the spec now disagrees with the wire", code, fam.StatusCodes[code])
				raiseTo(f, specdiff.Err)
				return
			}
		}

	case "response-property-added":
		fam, path := famAndPath(f, matching)
		if fam == nil {
			return
		}
		if _, live := fam.Fields[path]; live {
			f.Detail += " — already appearing in observed traffic"
		}

	case "response-enum-value-added":
		fam, path := famAndPath(f, matching)
		if fam == nil || len(f.Args) < 3 {
			return
		}
		if st, ok := fam.Fields[path]; ok && st.Values != nil {
			if _, seen := st.Values[f.Args[2]]; seen {
				f.Detail += " — already observed in traffic"
			}
		}
	}
}

// famAndPath resolves a response finding's family (status class from args[0],
// e.g. "200 application/json") and field path (args[1]).
func famAndPath(f *specdiff.Finding, matching func(string) []*baseline.Family) (*baseline.Family, string) {
	if len(f.Args) < 2 {
		return nil, ""
	}
	status, _, _ := strings.Cut(f.Args[0], " ")
	class := classOf(status)
	if class == "" {
		return nil, ""
	}
	fams := matching(class)
	if len(fams) == 0 {
		return nil, ""
	}
	// Declared paths are dot-joined; the learner's field map slash-joins.
	return fams[0], specwatch.CanonicalFieldPath(f.Args[1])
}

func classOf(status string) string {
	if len(status) == 0 || status[0] < '1' || status[0] > '5' {
		return ""
	}
	return status[:1] + "xx"
}

// presenceEvidenceFloor mirrors drift.presenceFloor (0.98): the presence
// ratio at which a field counts as a guarantee consumers rely on.
const presenceEvidenceFloor = 0.98

func raiseTo(f *specdiff.Finding, floor specdiff.Level) {
	if f.Level.Rank() < floor.Rank() {
		f.Level = floor
	}
}

// EnrichDeclared is the agent-side entry: joins the upstream's learned
// families onto the finding.
func (a *Agent) EnrichDeclared(upstream string, f *specdiff.Finding) {
	a.mu.Lock()
	l, ok := a.learners[upstream]
	a.mu.Unlock()
	if !ok {
		return // no observed traffic for this upstream yet — nothing to join
	}
	EnrichDeclaredFinding(f, l.Families())
}

// documentedFor returns the upstream's declared-changes journal, cached and
// reloaded at most once a minute (the observe path is hot).
func (a *Agent) documentedFor(upstream string) *specwatch.Documented {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.documented == nil {
		a.documented = map[string]*documentedCache{}
	}
	c, ok := a.documented[upstream]
	if ok && now.Sub(c.loaded) < time.Minute {
		return c.doc
	}
	doc := specwatch.LoadDocumented(a.Cfg.DataDir, upstream)
	a.documented[upstream] = &documentedCache{doc: doc, loaded: now}
	return doc
}

type documentedCache struct {
	doc    *specwatch.Documented
	loaded time.Time
}
