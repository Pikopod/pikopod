package scenario

import (
	"sort"
	"strconv"
	"strings"
)

type DiffEntry struct {
	Pointer  string `json:"pointer"`
	Kind     string `json:"kind"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}

const maxDiffEntries = 200

func escapePointerSeg(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1")
}

func structuralDiff(expected, actual any) []DiffEntry {
	out := []DiffEntry{}
	diffWalk("", expected, actual, &out)
	if len(out) > maxDiffEntries {
		out = out[:maxDiffEntries]
	}
	return out
}

func diffWalk(pointer string, expected, actual any, out *[]DiffEntry) {
	if len(*out) >= maxDiffEntries {
		return
	}
	if deepEqual(expected, actual) {
		return
	}

	eObj, eIsObj := expected.(map[string]any)
	aObj, aIsObj := actual.(map[string]any)
	eArr, eIsArr := expected.([]any)
	aArr, aIsArr := actual.([]any)

	if eIsObj && aIsObj {
		keySet := map[string]bool{}
		for k := range eObj {
			keySet[k] = true
		}
		for k := range aObj {
			keySet[k] = true
		}
		keys := make([]string, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := pointer + "/" + escapePointerSeg(k)
			ev, eHas := eObj[k]
			av, aHas := aObj[k]
			switch {
			case eHas && !aHas:
				*out = append(*out, DiffEntry{Pointer: p, Kind: "removed", Expected: ev})
			case !eHas && aHas:
				*out = append(*out, DiffEntry{Pointer: p, Kind: "added", Actual: av})
			default:
				diffWalk(p, ev, av, out)
			}
		}
		return
	}

	if eIsArr && aIsArr {
		n := len(eArr)
		if len(aArr) > n {
			n = len(aArr)
		}
		for i := 0; i < n; i++ {
			p := pointer + "/" + strconv.Itoa(i)
			switch {
			case i >= len(eArr):
				*out = append(*out, DiffEntry{Pointer: p, Kind: "added", Actual: aArr[i]})
			case i >= len(aArr):
				*out = append(*out, DiffEntry{Pointer: p, Kind: "removed", Expected: eArr[i]})
			default:
				diffWalk(p, eArr[i], aArr[i], out)
			}
		}
		return
	}

	p := pointer
	if p == "" {
		p = "/"
	}
	*out = append(*out, DiffEntry{Pointer: p, Kind: "changed", Expected: expected, Actual: actual})
}
