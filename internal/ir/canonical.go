package ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

var volatileKeys = map[string]bool{"sourcePointer": true, "evidence": true}

func Canonicalize(def *ApiDefinition) (string, error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("ir: marshal for canonicalization: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("ir: reparse for canonicalization: %w", err)
	}
	return CanonicalStringify(v), nil
}

func CanonicalStringify(value any) string {
	var b strings.Builder
	writeJSON(&b, canonicalValue(value))
	return b.String()
}

func NormalizedHash(def *ApiDefinition) (string, error) {
	canonical, err := Canonicalize(def)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}

func canonicalValue(value any) any {
	switch v := value.(type) {
	case []any:
		type entry struct {
			item any
			key  string
		}
		entries := make([]entry, len(v))
		for i, item := range v {
			ci := canonicalValue(item)
			var b strings.Builder
			writeJSON(&b, ci)
			entries[i] = entry{item: ci, key: b.String()}
		}
		sort.SliceStable(entries, func(i, j int) bool { return jsLess(entries[i].key, entries[j].key) })
		out := make([]any, len(entries))
		for i, e := range entries {
			out[i] = e.item
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			if volatileKeys[k] {
				continue
			}
			keys = append(keys, k)
		}
		sort.SliceStable(keys, func(i, j int) bool { return jsLess(keys[i], keys[j]) })
		out := orderedObject{keys: keys, values: make([]any, len(keys))}
		for i, k := range keys {
			out.values[i] = canonicalValue(v[k])
		}
		return out
	case float64:
		if v == 0 {
			return float64(0)
		}
		return v
	default:
		return value
	}
}

type orderedObject struct {
	keys   []string
	values []any
}

func writeJSON(b *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeJSONString(b, v)
	case float64:
		b.WriteString(FormatJSNumber(v))
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, item)
		}
		b.WriteByte(']')
	case orderedObject:
		b.WriteByte('{')
		for i, k := range v.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			writeJSON(b, v.values[i])
		}
		b.WriteByte('}')
	case map[string]any:
		writeJSON(b, canonicalValue(v))
	default:

		panic(fmt.Sprintf("ir: unexpected canonical value type %T", value))
	}
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func FormatJSNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	neg := ""
	if f < 0 {
		neg = "-"
		f = -f
	}

	sci := strconv.FormatFloat(f, 'e', -1, 64)
	ePos := strings.IndexByte(sci, 'e')
	mantissa := strings.Replace(sci[:ePos], ".", "", 1)
	exp10, _ := strconv.Atoi(sci[ePos+1:])
	k := len(mantissa)
	n := exp10 + 1

	switch {
	case k <= n && n <= 21:
		return neg + mantissa + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return neg + mantissa[:n] + "." + mantissa[n:]
	case -6 < n && n <= 0:
		return neg + "0." + strings.Repeat("0", -n) + mantissa
	default:
		expPart := strconv.Itoa(n - 1)
		if n-1 >= 0 {
			expPart = "+" + expPart
		}
		if k == 1 {
			return neg + mantissa + "e" + expPart
		}
		return neg + mantissa[:1] + "." + mantissa[1:] + "e" + expPart
	}
}

func jsLess(a, b string) bool {
	if isASCII(a) && isASCII(b) {
		return a < b
	}
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func JSLess(a, b string) bool { return jsLess(a, b) }
