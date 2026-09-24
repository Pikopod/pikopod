package specdiff

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

type Positions struct {
	New ir.Positions
	Old ir.Positions
}

func (p Positions) lookup(f Finding) (ir.Position, bool) {
	switch f.SourceSide {
	case SideNew:
		return p.New.Lookup(f.SourcePointer)
	case SideOld:
		return p.Old.Lookup(f.SourcePointer)
	}
	return ir.Position{}, false
}

func ghEscape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

func ghProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

func (r *Report) WriteGitHubActions(w io.Writer, positions Positions) {
	for _, level := range []Level{Err, Warn, Info} {
		for _, f := range bySeverity(r.Items, level) {
			cmd := map[Level]string{Err: "error", Warn: "warning", Info: "notice"}[f.Level]
			props := "title=" + ghProperty(f.ID)
			if pos, ok := positions.lookup(f.Finding); ok && pos.Line > 0 {
				props = "file=" + ghProperty(pos.File) + ",line=" + fmt.Sprint(pos.Line) + ",col=" + fmt.Sprint(pos.Col) + "," + props
			}
			fmt.Fprintf(w, "::%s %s::%s\n", cmd, props, ghEscape(f.ID+": "+f.Method+" "+f.Template+" — "+f.Detail))
		}
	}
}

type Report struct {
	Source  string          `json:"source"`
	Old     string          `json:"old"`
	New     string          `json:"new"`
	Summary map[Level]int   `json:"summary"`
	Items   []ReportFinding `json:"findings"`
}

type ReportFinding struct {
	Finding
	Fingerprint string `json:"fingerprint"`
}

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
