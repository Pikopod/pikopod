package volatile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/pathtmpl"
	"github.com/pikopod/pikopod/internal/proxy"
)

type RefusalReason string

const (
	Structural RefusalReason = "STRUCTURAL"

	TypeUnstable RefusalReason = "TYPE_UNSTABLE"

	Stable RefusalReason = "STABLE"

	OverBroad RefusalReason = "OVER_BROAD"

	InsufficientSamples RefusalReason = "INSUFFICIENT_SAMPLES"

	AlreadyConfigured RefusalReason = "ALREADY_CONFIGURED"

	Redacted RefusalReason = "REDACTED"
)

type Suggestion struct {
	Name     string   `json:"name"`
	Paths    []string `json:"paths"`
	Samples  int      `json:"samples"`
	Distinct int      `json:"distinct_values"`
	Churn    float64  `json:"churn"`
}

type Refusal struct {
	Name   string        `json:"name"`
	Path   string        `json:"path"`
	Reason RefusalReason `json:"reason"`
	Detail string        `json:"detail"`
}

type DeadEntry struct {
	Name    string `json:"name"`
	Records int    `json:"records"`
}

type Analysis struct {
	Suggestions []Suggestion `json:"suggestions"`
	Refusals    []Refusal    `json:"refusals"`
	DeadEntries []DeadEntry  `json:"dead_entries"`
	Records     int          `json:"records"`
}

const (
	minSuggestSamples = 10
	churnFloor        = 0.5

	maxDistinctTracked = 4096
)

type fieldAgg struct {
	paths map[string]*pathAgg
}

type pathAgg struct {
	samples    int
	distinct   map[string]int
	types      map[string]bool
	structural bool
	redacted   bool
}

func Analyze(records []*proxy.Record, configured []string) *Analysis {
	an := &Analysis{Records: len(records)}
	cfg := map[string]bool{}
	for _, c := range configured {
		cfg[strings.ToLower(c)] = true
	}

	byName := map[string]*fieldAgg{}
	matchedCfg := map[string]bool{}
	for _, rec := range records {
		if rec.RespKind != "json" {
			continue
		}
		redacted := map[string]bool{}
		for _, r := range rec.Redacted {
			if r.Section == "resp_body" {

				redacted[strings.TrimPrefix(r.Pointer, "/")] = true
			}
		}
		family := rec.Method + " " + pathtmpl.Templatize(stripQuery(rec.Path)) + " " + baseline.StatusClass(rec.Status)
		flat := baseline.Flatten(rec.RespBody)
		for path, fv := range flat {
			name := strings.ToLower(Leaf(path))
			if cfg[name] {
				if !matchedCfg[name] {
					an.Refusals = append(an.Refusals, Refusal{Name: name, Path: path, Reason: AlreadyConfigured,
						Detail: "already in volatile_fields; its values are suppressed and it is not re-suggested"})
				}
				matchedCfg[name] = true
				continue
			}
			agg := byName[name]
			if agg == nil {
				agg = &fieldAgg{paths: map[string]*pathAgg{}}
				byName[name] = agg
			}
			key := family + "|" + path
			pa := agg.paths[key]
			if pa == nil {
				pa = &pathAgg{distinct: map[string]int{}, types: map[string]bool{}}
				agg.paths[key] = pa
			}
			pa.samples++
			pa.types[fv.Type] = true
			if fv.Type == "object" || fv.Type == "array" {
				pa.structural = true
			}
			if redacted[path] {
				pa.redacted = true
			}
			if len(pa.distinct) < maxDistinctTracked {
				pa.distinct[fv.Value]++
			}
		}
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		agg := byName[name]

		var churnPaths, stablePaths []string
		var refusal *Refusal
		samples, distinct := 0, 0
		for key, pa := range agg.paths {
			path := key[strings.LastIndex(key, "|")+1:]
			switch {
			case pa.redacted:
				refusal = &Refusal{Name: name, Path: path, Reason: Redacted,
					Detail: "the sanitizer rewrote this field — wire churn is unprovable from stored values"}
			case pa.structural:
				refusal = &Refusal{Name: name, Path: path, Reason: Structural,
					Detail: "sometimes a container — shape drift is a finding, not noise"}
			case len(pa.types) > 1:
				refusal = &Refusal{Name: name, Path: path, Reason: TypeUnstable,
					Detail: fmt.Sprintf("type flips across records (%s) — that is a drift finding", typeList(pa.types))}
			case pa.samples < minSuggestSamples:
				refusal = &Refusal{Name: name, Path: path, Reason: InsufficientSamples,
					Detail: fmt.Sprintf("only %d observation(s); churn needs ≥%d", pa.samples, minSuggestSamples)}
			case float64(len(pa.distinct))/float64(pa.samples) >= churnFloor:
				churnPaths = append(churnPaths, path)
				samples += pa.samples
				distinct += len(pa.distinct)
			default:
				stablePaths = append(stablePaths, path)
			}
			if refusal != nil {
				break
			}
		}
		switch {
		case refusal != nil:
			an.Refusals = append(an.Refusals, *refusal)
		case len(churnPaths) == 0:

		case len(stablePaths) > 0:
			sort.Strings(stablePaths)
			an.Refusals = append(an.Refusals, Refusal{Name: name, Path: stablePaths[0], Reason: OverBroad,
				Detail: fmt.Sprintf("`%s` also names a STABLE field (%s) — a name-based entry would silence its assertions", name, stablePaths[0])})
		default:
			sort.Strings(churnPaths)
			an.Suggestions = append(an.Suggestions, Suggestion{
				Name: name, Paths: churnPaths, Samples: samples, Distinct: distinct,
				Churn: float64(distinct) / float64(max(samples, 1)),
			})
		}
	}

	cfgNames := make([]string, 0, len(cfg))
	for n := range cfg {
		cfgNames = append(cfgNames, n)
	}
	sort.Strings(cfgNames)
	for _, n := range cfgNames {
		if !matchedCfg[n] {
			an.DeadEntries = append(an.DeadEntries, DeadEntry{Name: n, Records: len(records)})
		}
	}
	return an
}

func typeList(types map[string]bool) string {
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
