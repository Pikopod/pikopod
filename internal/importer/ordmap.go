package importer

import (
	"strconv"
	"strings"
)

// OrdMap is a JSON object with ECMAScript property semantics: iteration order
// is observable in the output, so index-like keys sort before insertion order.
type OrdMap struct {
	keys   []string
	values map[string]any
}

func NewOrdMap() *OrdMap {
	return &OrdMap{values: make(map[string]any)}
}

func (m *OrdMap) Len() int { return len(m.keys) }

func (m *OrdMap) Has(key string) bool {
	_, ok := m.values[key]
	return ok
}

func (m *OrdMap) Get(key string) (any, bool) {
	v, ok := m.values[key]
	return v, ok
}

// GetOr returns the value for key, or nil when absent (JS `obj.key`).
func (m *OrdMap) GetOr(key string) any {
	return m.values[key]
}

func (m *OrdMap) Set(key string, value any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

func (m *OrdMap) Delete(key string) {
	if _, ok := m.values[key]; !ok {
		return
	}
	delete(m.values, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// Keys returns keys in ECMAScript OwnPropertyKeys order: integer-index keys
// ascending first, then the rest in insertion order.
func (m *OrdMap) Keys() []string {
	var indexKeys, plainKeys []string
	for _, k := range m.keys {
		if isArrayIndexKey(k) {
			indexKeys = append(indexKeys, k)
		} else {
			plainKeys = append(plainKeys, k)
		}
	}
	// Ascending numeric sort; index keys are canonical so numeric order is
	// length-then-lexicographic.
	for i := 1; i < len(indexKeys); i++ {
		for j := i; j > 0 && indexLess(indexKeys[j], indexKeys[j-1]); j-- {
			indexKeys[j], indexKeys[j-1] = indexKeys[j-1], indexKeys[j]
		}
	}
	return append(indexKeys, plainKeys...)
}

func indexLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// isArrayIndexKey reports whether key is a canonical numeric string in
// 0..2^32-2 — ECMAScript's definition of an array index.
func isArrayIndexKey(key string) bool {
	if key == "" || len(key) > 10 {
		return false
	}
	if key == "0" {
		return true
	}
	if key[0] == '0' {
		return false // non-canonical, e.g. "042"
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(key, 10, 64)
	return err == nil && n <= 4294967294
}

// deepClone deep-copies a parsed value (reftools clone equivalent).
func deepClone(value any) any {
	switch v := value.(type) {
	case *OrdMap:
		out := NewOrdMap()
		for _, k := range v.keys {
			out.Set(k, deepClone(v.values[k]))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = deepClone(item)
		}
		return out
	default:
		return value
	}
}

// jpescape escapes a JSON-pointer segment (~ → ~0, / → ~1).
func jpescape(segment string) string {
	return strings.ReplaceAll(strings.ReplaceAll(segment, "~", "~0"), "/", "~1")
}

// jpunescape unescapes a JSON-pointer segment (~1 → /, then ~0 → ~), in that
// replacement order.
func jpunescape(segment string) string {
	return strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
}
