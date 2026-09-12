// Handoff rendering: a JSON handoff's "source" discriminator selects the
// PR-comment markdown, and each source owns its own comment marker.
package pr

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/pikopod/pikopod/internal/specupdate"
)

// RenderHandoff returns (source, markdown). Unknown sources fail loudly —
// a marker per unknown source would fragment the update-in-place contract.
func RenderHandoff(raw []byte) (string, string, error) {
	var head struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", "", fmt.Errorf("handoff is not JSON: %w", err)
	}
	var buf bytes.Buffer
	switch head.Source {
	case "spec-diff":
		var rep specdiff.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			return "", "", err
		}
		fmt.Fprintf(&buf, "### pikopod spec-diff — `%s` → `%s`\n\n", rep.Old, rep.New)
		rep.WriteMarkdown(&buf)

	case "spec-update":
		var h struct {
			Upstream    string             `json:"upstream"`
			Applied     []specupdate.Patch `json:"applied"`
			Suggestions []specupdate.Patch `json:"suggestions"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return "", "", err
		}
		fmt.Fprintf(&buf, "### pikopod spec-update — %s\n\n", h.Upstream)
		if len(h.Applied) == 0 && len(h.Suggestions) == 0 {
			buf.WriteString("No traffic-evidenced changes.\n")
			break
		}
		if len(h.Applied) > 0 {
			fmt.Fprintf(&buf, "**%d additive patch(es) applied** (this PR):\n\n", len(h.Applied))
			buf.WriteString("| Change | Endpoint | Evidence |\n|---|---|---|\n")
			for _, p := range h.Applied {
				fmt.Fprintf(&buf, "| `%s` | `%s %s` | %s |\n", p.Kind, p.Method, p.Template, evidenceLine(p))
			}
			buf.WriteString("\n")
		}
		if len(h.Suggestions) > 0 {
			fmt.Fprintf(&buf, "**%d suggestion(s)** — narrowings, review required, never auto-applied:\n\n", len(h.Suggestions))
			for _, p := range h.Suggestions {
				fmt.Fprintf(&buf, "- `%s %s`: %s\n", p.Method, p.Template, p.Reason)
			}
		}

	case "conformance":
		var h struct {
			Upstream   string `json:"upstream"`
			Records    int    `json:"records"`
			Violations []struct {
				Method      string `json:"method"`
				Template    string `json:"template"`
				Status      int    `json:"status"`
				Pointer     string `json:"pointer"`
				Severity    string `json:"severity"`
				Message     string `json:"message"`
				Occurrences int    `json:"occurrences"`
			} `json:"violations"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return "", "", err
		}
		fmt.Fprintf(&buf, "### pikopod conformance — %s\n\n", h.Upstream)
		if len(h.Violations) == 0 {
			fmt.Fprintf(&buf, "%d response(s) checked — the provider obeys its own docs.\n", h.Records)
			break
		}
		fmt.Fprintf(&buf, "%d response(s) checked, **%d violation(s)** — the provider disagrees with its own documentation:\n\n", h.Records, len(h.Violations))
		buf.WriteString("| Severity | Endpoint | Where | Violation | Seen |\n|---|---|---|---|---|\n")
		for _, v := range h.Violations {
			where := v.Pointer
			if where == "" {
				where = "-"
			}
			fmt.Fprintf(&buf, "| %s | `%s %s` → %d | `%s` | %s | %d× |\n",
				v.Severity, v.Method, v.Template, v.Status, where, v.Message, v.Occurrences)
		}

	case "replay-ci":
		var h struct {
			Findings []struct {
				Upstream string `json:"upstream"`
				Method   string `json:"method"`
				Template string `json:"template"`
				Kind     string `json:"kind"`
				Field    string `json:"field"`
				Detail   string `json:"detail"`
			} `json:"findings"`
			Records int `json:"records"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return "", "", err
		}
		buf.WriteString("### pikopod replay --ci\n\n")
		if len(h.Findings) == 0 {
			fmt.Fprintf(&buf, "%d recording(s) gated — clean, no drift against frozen baselines.\n", h.Records)
			break
		}
		fmt.Fprintf(&buf, "%d recording(s) gated — **%d drift finding(s)**:\n\n", h.Records, len(h.Findings))
		buf.WriteString("| Kind | Endpoint | Field | Detail |\n|---|---|---|---|\n")
		for _, f := range h.Findings {
			fmt.Fprintf(&buf, "| `%s` | `%s %s` (%s) | `%s` | %s |\n", f.Kind, f.Method, f.Template, f.Upstream, f.Field, f.Detail)
		}

	default:
		return "", "", fmt.Errorf("unknown handoff source %q (expected spec-diff, spec-update, conformance, or replay-ci)", head.Source)
	}
	return head.Source, buf.String(), nil
}

func evidenceLine(p specupdate.Patch) string {
	ev := p.Evidence
	switch {
	case ev.Occurrences > 0:
		return fmt.Sprintf("%d occurrence(s)", ev.Occurrences)
	case ev.Presence > 0:
		return fmt.Sprintf("presence %.0f%% since %s", ev.Presence*100, ev.Since)
	default:
		return p.Reason
	}
}
