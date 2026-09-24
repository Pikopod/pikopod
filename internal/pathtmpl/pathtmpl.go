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

func ClassifySegment(seg string) string {
	switch {
	case uuidSegRE.MatchString(seg):
		return "{uuid}"
	case dateSegRE.MatchString(seg):
		return "{date}"
	case prefixedSegRE.MatchString(seg) && hasDigit(seg[strings.Index(seg, "_")+1:]):

		return seg[:strings.Index(seg, "_")+1] + "{id}"
	case digitsSegRE.MatchString(seg):
		return "{id}"
	case idish(seg):
		return "{id}"
	default:
		return ""
	}
}

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

const DefaultCardinalityThreshold = 32

type Promotion struct {
	Parent   string
	Position int
}

type Guard struct {
	mu        sync.Mutex
	threshold int

	seen map[string]map[string]int

	promoted map[string]bool

	pinned map[string]map[string]bool
}

const pinCount = 3

func NewGuard(threshold int) *Guard {
	if threshold <= 0 {
		threshold = DefaultCardinalityThreshold
	}
	return &Guard{threshold: threshold, seen: map[string]map[string]int{}, promoted: map[string]bool{}, pinned: map[string]map[string]bool{}}
}

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
				parent = append(parent, seg)
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

func (g *Guard) MergeKey(template string) string {
	merged, _ := g.Apply(template)
	return merged
}
