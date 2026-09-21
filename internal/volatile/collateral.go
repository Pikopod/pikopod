package volatile

import (
	"fmt"
	"sort"

	"github.com/pikopod/pikopod/internal/baseline"
)

const presenceFloor = 0.9

func Collateral(l *baseline.Learner, m *Matcher) []Refusal {
	if l == nil || m == nil {
		return nil
	}
	var out []Refusal
	matched := map[string]bool{}
	churned := map[string]bool{}
	type stable struct{ entry, path string }
	var stables []stable
	for _, fam := range l.Families() {
		for path, st := range fam.Fields {
			entry, ok := m.Match(path)
			if !ok {
				continue
			}
			matched[entry] = true
			if st.Churned {
				churned[entry] = true
				continue
			}
			if fam.Samples < minSuggestSamples || len(st.Types) != 1 || float64(st.Count) < presenceFloor*float64(fam.Samples) {
				continue
			}
			stables = append(stables, stable{entry, path})
		}
	}
	sort.Slice(stables, func(i, j int) bool {
		return stables[i].entry+stables[i].path < stables[j].entry+stables[j].path
	})
	seen := map[string]bool{}
	for _, s := range stables {
		if seen[s.entry+"|"+s.path] {
			continue
		}
		seen[s.entry+"|"+s.path] = true
		if churned[s.entry] {
			out = append(out, Refusal{Name: s.entry, Path: s.path, Reason: OverBroad,
				Detail: fmt.Sprintf("`%s` also covers %s, whose values never changed; the entry silences its value assertion for nothing", s.entry, s.path)})
		} else {
			out = append(out, Refusal{Name: s.entry, Path: s.path, Reason: Stable,
				Detail: fmt.Sprintf("%s never changed value across %d+ samples; there is nothing to silence", s.path, minSuggestSamples)})
		}
	}
	for _, e := range m.Entries() {
		if !matched[e] {
			out = append(out, Refusal{Name: e, Reason: Dead, Detail: "matches no field the agent has learned; you believe a field is excluded while nothing is"})
		}
	}
	return out
}
