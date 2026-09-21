package volatile

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/pikopod/pikopod/internal/errfmt"
)

const (
	Malformed RefusalReason = "MALFORMED"
	Dead      RefusalReason = "DEAD_ENTRY"
)

const volatileDocs = "docs/config-reference.md#volatile_fields"

type Drop struct {
	Entry string `json:"entry"`
	Path  string `json:"path"`
	Count int    `json:"count"`
}

type entry struct {
	raw    string
	suffix string
	bare   bool
}

type Matcher struct {
	entries []entry
	mu      sync.Mutex
	drops   map[string]map[string]int // entry → path → count
}

const maxDropPaths = 2000

func Compile(entries []string) (*Matcher, []Refusal, error) {
	return CompileSets(entries, nil)
}

func CompileSets(user, curated []string) (*Matcher, []Refusal, error) {
	m := &Matcher{drops: map[string]map[string]int{}}
	var refusals []Refusal
	seen := map[string]bool{}
	add := func(raw string, label string) error {
		norm := strings.ToLower(strings.TrimSpace(raw))
		if why := malformed(norm); why != "" {
			refusals = append(refusals, Refusal{Name: raw, Reason: Malformed, Detail: why})
			return errfmt.New("invalid volatile_fields entry", fmt.Sprintf("%q %s", raw, why), "use a bare field name (status) or a parent/child suffix (meta/status, items[]/status)", volatileDocs)
		}
		if seen[norm] {
			refusals = append(refusals, Refusal{Name: raw, Reason: AlreadyConfigured, Detail: "listed more than once; the first entry already covers it"})
			return nil
		}
		seen[norm] = true
		if label == "" {
			label = raw
		}
		m.entries = append(m.entries, entry{raw: label, suffix: norm, bare: !strings.Contains(norm, "/")})
		return nil
	}
	for _, e := range user {
		if err := add(e, ""); err != nil {
			return nil, refusals, err
		}
	}
	for _, e := range curated {
		if err := add(e, "curated:"+strings.ToLower(e)); err != nil {
			return nil, refusals, err
		}
	}
	return m, refusals, nil
}

func malformed(norm string) string {
	if norm == "" {
		return "is empty"
	}
	if strings.HasPrefix(norm, "/") || strings.HasSuffix(norm, "/") {
		return "must not start or end with a slash"
	}
	for _, seg := range strings.Split(norm, "/") {
		name := strings.TrimSuffix(seg, "[]")
		if name == "" {
			return "has an empty path segment"
		}
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == '$' || r == '@') {
				return fmt.Sprintf("contains %q, which is not a field-name character", string(r))
			}
		}
	}
	return ""
}

func (m *Matcher) Entries() []string {
	out := make([]string, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.raw)
	}
	return out
}

func Leaf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(path, "[]")
}

func (m *Matcher) Match(path string) (string, bool) {
	if m == nil || len(m.entries) == 0 {
		return "", false
	}
	leaf := Leaf(path)
	for i := range m.entries {
		e := &m.entries[i]
		if e.bare {
			if strings.EqualFold(leaf, e.suffix) {
				return e.raw, true
			}
			continue
		}
		if suffixMatches(path, e.suffix) {
			return e.raw, true
		}
	}
	return "", false
}

func suffixMatches(path, suffix string) bool {
	path = strings.TrimSuffix(path, "[]")
	if len(path) < len(suffix) {
		return false
	}
	start := len(path) - len(suffix)
	if !strings.EqualFold(path[start:], suffix) {
		return false
	}
	return start == 0 || path[start-1] == '/'
}

func (m *Matcher) RecordDrop(path string) {
	if m == nil {
		return
	}
	entry, ok := m.Match(path)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := m.drops[entry]
	if paths == nil {
		paths = map[string]int{}
		m.drops[entry] = paths
	}
	if _, known := paths[path]; !known && len(paths) >= maxDropPaths {
		return
	}
	paths[path]++
}

func (m *Matcher) Drops() []Drop {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Drop
	for entry, paths := range m.drops {
		for p, n := range paths {
			out = append(out, Drop{Entry: entry, Path: p, Count: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Entry != out[j].Entry {
			return out[i].Entry < out[j].Entry
		}
		return out[i].Path < out[j].Path
	})
	return out
}
