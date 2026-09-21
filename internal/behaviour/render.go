package behaviour

import (
	"fmt"
	"io"
	"strings"
	"time"
)

func (r *Report) WriteText(w io.Writer, now time.Time) {
	fmt.Fprintf(w, "observed state machine — %s\n", r.Upstream)
	if len(r.Fields) == 0 {
		fmt.Fprintln(w, "  no value transitions observed yet: a field must change on the same resource across two recordings")
		return
	}
	for _, f := range r.Fields {
		days := int(now.Sub(f.Since).Hours() / 24)
		fmt.Fprintf(w, "\n  %s %s · %s          (%d transitions over %d days)\n", f.Method, f.Template, f.Field, f.Transitions, days)
		for _, e := range f.Edges {
			note := ""
			if e.Count == 1 {
				note = "     ← seen once"
			}
			fmt.Fprintf(w, "    %-12s → %-12s %5d%s\n", e.From, e.To, e.Count, note)
		}
		switch {
		case !f.WarmedUp:
			fmt.Fprintf(w, "    still warming up (%d samples); nothing claimed about transitions not seen\n", f.Samples)
		case len(f.NeverObserved) == 0:
			fmt.Fprintln(w, "    every pair of observed states has been seen in both directions")
		default:
			shown := f.NeverObserved
			more := ""
			if len(shown) > 12 {
				more = fmt.Sprintf(", and %d more", len(shown)-12)
				shown = shown[:12]
			}
			fmt.Fprintf(w, "    never observed: %s%s\n", strings.Join(shown, ", "), more)
		}
	}
	fmt.Fprintln(w, "\n\"never observed\" is an absence in recorded traffic, not a rule the provider follows; nothing here is enforced")
}
