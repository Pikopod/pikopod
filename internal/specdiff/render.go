// Rendering: text (terminal), json (machine handoff — the same shape the PR
// surface consumes), markdown (PR comments).
package specdiff

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Report is the JSON envelope — also the PR-surface handoff shape.
type Report struct {
	Source  string          `json:"source"` // "spec-diff"
	Old     string          `json:"old"`
	New     string          `json:"new"`
	Summary map[Level]int   `json:"summary"`
	Items   []ReportFinding `json:"findings"`
}

type ReportFinding struct {
	Finding
	Fingerprint string `json:"fingerprint"`
}

// BuildReport wraps findings with fingerprints and a severity summary.
func BuildReport(oldSrc, newSrc string, findings []Finding) *Report {
	r := &Report{Source: "spec-diff", Old: oldSrc, New: newSrc, Summary: map[Level]int{}, Items: []ReportFinding{}}
	for _, f := range findings {
		r.Summary[f.Level]++
		r.Items = append(r.Items, ReportFinding{Finding: f, Fingerprint: f.Fingerprint()})
	}
	return r
}

func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText renders for the terminal, grouped by severity (ERR first).
func (r *Report) WriteText(w io.Writer) {
	if len(r.Items) == 0 {
		fmt.Fprintln(w, "no declared changes between the two specs")
		return
	}
	fmt.Fprintf(w, "%d change(s): %d ERR, %d WARN, %d INFO\n\n",
		len(r.Items), r.Summary[Err], r.Summary[Warn], r.Summary[Info])
	for _, level := range []Level{Err, Warn, Info} {
		for _, f := range bySeverity(r.Items, level) {
			fmt.Fprintf(w, "%-4s %-6s %-40s %s\n     %s  [%s]\n",
				f.Level, f.Method, f.Template, f.ID, f.Detail, f.Fingerprint)
		}
	}
}

// WriteMarkdown renders for a PR comment body.
func (r *Report) WriteMarkdown(w io.Writer) {
	if len(r.Items) == 0 {
		fmt.Fprintln(w, "No declared changes between the two specs.")
		return
	}
	fmt.Fprintf(w, "**%d change(s)** — %d breaking (ERR), %d warning, %d info\n\n",
		len(r.Items), r.Summary[Err], r.Summary[Warn], r.Summary[Info])
	fmt.Fprintln(w, "| Level | Endpoint | Change |")
	fmt.Fprintln(w, "|---|---|---|")
	for _, level := range []Level{Err, Warn, Info} {
		for _, f := range bySeverity(r.Items, level) {
			icon := map[Level]string{Err: "🔴", Warn: "🟡", Info: "🔵"}[f.Level]
			fmt.Fprintf(w, "| %s %s | `%s %s` | %s |\n", icon, f.Level, f.Method, f.Template, escapePipes(f.Detail))
		}
	}
}

func bySeverity(items []ReportFinding, level Level) []ReportFinding {
	var out []ReportFinding
	for _, f := range items {
		if f.Level == level {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Template != out[j].Template {
			return out[i].Template < out[j].Template
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func escapePipes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
