// Deterministic route matching: candidates rank by specificity with a lexical
// tiebreak, so the result never depends on declaration order.
package sandbox

import (
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

type matchKind int

const (
	matchFound matchKind = iota
	matchMethodNotAllowed
	matchNotFound
)

type matchResult struct {
	kind       matchKind
	endpoint   *ir.Endpoint
	pathParams map[string]string
	allow      []string // for method-not-allowed: methods that DO serve the path, sorted
}

var paramSegment = regexp.MustCompile(`^\{(.+)\}$`)

func pathSegments(path string) []string {
	out := []string{}
	for _, s := range strings.Split(path, "/") {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// matchPathTemplate matches one template against concrete segments. On a
// malformed %-escape the raw segment is used rather than erroring.
func matchPathTemplate(template string, target []string) (map[string]string, bool) {
	tpl := pathSegments(template)
	if len(tpl) != len(target) {
		return nil, false
	}
	params := map[string]string{}
	for i, seg := range tpl {
		if m := paramSegment.FindStringSubmatch(seg); m != nil {
			if len(target[i]) == 0 {
				return nil, false // a path parameter cannot be empty
			}
			decoded, err := url.PathUnescape(target[i])
			if err != nil {
				decoded = target[i]
			}
			params[m[1]] = decoded
		} else if seg != target[i] {
			return nil, false
		}
	}
	return params, true
}

func templateParamCount(template string) int {
	n := 0
	for _, seg := range pathSegments(template) {
		if paramSegment.MatchString(seg) {
			n++
		}
	}
	return n
}

func matchRoute(endpoints []ir.Endpoint, method, innerPath string) *matchResult {
	target := pathSegments(innerPath)
	upper := strings.ToUpper(method)

	type candidate struct {
		endpoint *ir.Endpoint
		params   map[string]string
	}
	var pathMatches []candidate
	for i := range endpoints {
		if params, ok := matchPathTemplate(endpoints[i].PathTemplate.Value, target); ok {
			pathMatches = append(pathMatches, candidate{endpoint: &endpoints[i], params: params})
		}
	}
	if len(pathMatches) == 0 {
		return &matchResult{kind: matchNotFound}
	}

	var methodMatches []candidate
	for _, c := range pathMatches {
		if strings.ToUpper(c.endpoint.Method.Value) == upper {
			methodMatches = append(methodMatches, c)
		}
	}
	if len(methodMatches) == 0 {
		seen := map[string]bool{}
		allow := []string{}
		for _, c := range pathMatches {
			m := strings.ToUpper(c.endpoint.Method.Value)
			if !seen[m] {
				seen[m] = true
				allow = append(allow, m)
			}
		}
		sort.Strings(allow) // code-point order (ASCII methods)
		return &matchResult{kind: matchMethodNotAllowed, allow: allow}
	}

	// Most specific wins: fewest params, then lexical path for stability.
	sort.SliceStable(methodMatches, func(a, b int) bool {
		pa := templateParamCount(methodMatches[a].endpoint.PathTemplate.Value)
		pb := templateParamCount(methodMatches[b].endpoint.PathTemplate.Value)
		if pa != pb {
			return pa < pb
		}
		return methodMatches[a].endpoint.PathTemplate.Value < methodMatches[b].endpoint.PathTemplate.Value
	})
	best := methodMatches[0]
	return &matchResult{kind: matchFound, endpoint: best.endpoint, pathParams: best.params}
}
