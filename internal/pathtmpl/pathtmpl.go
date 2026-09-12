// Package pathtmpl collapses concrete request paths into endpoint templates,
// so per-id paths cannot explode cardinality and starve every baseline.
package pathtmpl

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	uuidSegRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	dateSegRE     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(T[0-9:.+Zz-]+)?$`)
	prefixedSegRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9]{4,}$`)
	digitsSegRE   = regexp.MustCompile(`^\d{4,}$`)
	longOpaqueRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	alnumSegRE    = regexp.MustCompile(`^[A-Za-z0-9]{8,}$`)
)

// idish: 16+ opaque chars, or 8+ alnum containing at least one digit
// (RE2 has no lookahead, so the digit check is plain code).
func idish(seg string) bool {
	if longOpaqueRE.MatchString(seg) {
		return true
	}
	return alnumSegRE.MatchString(seg) && hasDigit(seg)
}

func hasDigit(s string) bool {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// ClassifySegment returns the template placeholder for an identifier-shaped
// segment, or "" if the segment looks static (a resource name).
func ClassifySegment(seg string) string {
	switch {
	case uuidSegRE.MatchString(seg):
		return "{uuid}"
	case dateSegRE.MatchString(seg):
		return "{date}"
	case prefixedSegRE.MatchString(seg) && hasDigit(seg[strings.Index(seg, "_")+1:]):
		// Keep the prefix readable (tx_abc123 → tx_{id}); the digit check
		// stops snake_case resource names reading as prefixed ids.
		return seg[:strings.Index(seg, "_")+1] + "{id}"
	case digitsSegRE.MatchString(seg):
		return "{id}"
	case idish(seg):
		return "{id}"
	default:
		return ""
	}
}

// Templatize collapses a path (no query) using classification only.
func Templatize(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		if ph := ClassifySegment(seg); ph != "" {
			segs[i] = ph
		}
	}
	return strings.Join(segs, "/")
}

// DefaultCardinalityThreshold is how many distinct values a "static" segment
// position may produce before it is force-promoted to {id}.
const DefaultCardinalityThreshold = 32

// Promotion reports a position the guard force-promoted: templates that
// previously differed at that position must be merged by the caller.
type Promotion struct {
	Parent   string // template of everything before the promoted position
	Position int    // segment index that was promoted
}

// Guard watches distinct values per (parent template, position) and promotes
// runaway positions. One goroutine per upstream; the learner serializes.
type Guard struct {
	mu        sync.Mutex
	threshold int
	// distinct values (with counts) seen at each static position, keyed by
	// parent|position. Counts drive frequency-pinning at promotion time.
	seen map[string]map[string]int
	// promoted positions: parent|position → true.
	promoted map[string]bool
	// static literals surviving a promotion (seen ≥ pinCount times BEFORE the
	// blowup — /orders/summary stays static even when /orders/{id} exists).
	pinned map[string]map[string]bool
}

// pinCount is how many sightings make a literal trustworthy as static.
const pinCount = 3

func NewGuard(threshold int) *Guard {
	if threshold <= 0 {
		threshold = DefaultCardinalityThreshold
	}
	return &Guard{threshold: threshold, seen: map[string]map[string]int{}, promoted: map[string]bool{}, pinned: map[string]map[string]bool{}}
}

// Apply templatizes path and applies/updates promotions. It returns the final
// template and any NEW promotion triggered by this path.
func (g *Guard) Apply(path string) (string, *Promotion) {
	g.mu.Lock()
	defer g.mu.Unlock()

	segs := strings.Split(path, "/")
	var promo *Promotion
	parent := make([]string, 0, len(segs))
	for i, seg := range segs {
		if seg == "" {
			parent = append(parent, seg)
			continue
		}
		if ph := ClassifySegment(seg); ph != "" {
			segs[i] = ph
			parent = append(parent, ph)
			continue
		}
		key := strings.Join(parent, "/") + "|" + strconv.Itoa(i)
		if g.promoted[key] {
			if g.pinned[key][seg] {
				parent = append(parent, seg) // trusted static literal survives promotion
				continue
			}
			segs[i] = "{id}"
			parent = append(parent, "{id}")
			continue
		}
		vals, ok := g.seen[key]
		if !ok {
			vals = map[string]int{}
			g.seen[key] = vals
		}
		vals[seg]++
		if len(vals) > g.threshold {
			g.promoted[key] = true
			// Frequency-pinning: literals seen repeatedly before the blowup
			// are real routes, not ids — they stay static.
			pins := map[string]bool{}
			for v, count := range vals {
				if count >= pinCount {
					pins[v] = true
				}
			}
			g.pinned[key] = pins
			delete(g.seen, key)
			if pins[seg] {
				parent = append(parent, seg)
				promo = &Promotion{Parent: strings.Join(parent[:len(parent)-1], "/"), Position: i}
				continue
			}
			segs[i] = "{id}"
			parent = append(parent, "{id}")
			promo = &Promotion{Parent: strings.Join(parent[:len(parent)-1], "/"), Position: i}
			continue
		}
		parent = append(parent, seg)
	}
	return strings.Join(segs, "/"), promo
}

// MergeKey re-templatizes an existing template under current promotions, so
// the learner can merge baseline families after a promotion.
func (g *Guard) MergeKey(template string) string {
	merged, _ := g.Apply(template)
	return merged
}
