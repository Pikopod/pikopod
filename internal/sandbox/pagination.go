// Deterministic cursor pagination. The cursor is an opaque base64url
// encoding of the last resource key returned.
package sandbox

import (
	"encoding/base64"
	"math"
	"strconv"
	"strings"
)

const (
	paginationDefaultLimit = 20
	paginationMaxLimit     = 100
)

// jsNumber approximates JS Number(str): trimmed, "" → 0, else float parse.
func jsNumber(raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func parseLimit(raw *string) int {
	if raw == nil {
		return paginationDefaultLimit
	}
	f, ok := jsNumber(*raw)
	if !ok || f != math.Trunc(f) || math.IsInf(f, 0) || f < 1 {
		return paginationDefaultLimit
	}
	n := int(f)
	if n > paginationMaxLimit {
		return paginationMaxLimit
	}
	return n
}

func encodeCursor(resourceKey string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(resourceKey))
}

// decodeCursor decodes a cursor to a resource key; nil for a missing/garbled
// one (Node's base64url decoder is lenient about padding).
func decodeCursor(raw *string) *string {
	if raw == nil || *raw == "" {
		return nil
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		if b, err := enc.DecodeString(*raw); err == nil {
			if len(b) == 0 {
				return nil
			}
			s := string(b)
			return &s
		}
	}
	return nil
}
