package sanitize

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Sanitization SUBSTITUTES rather than deletes — dropping an auth header or an id
// would break replay. Detection is generic and FAILS CLOSED: unclassifiable → DROP.

type Mode string

const (
	ModeDrop       Mode = "DROP"
	ModeSubstitute Mode = "SUBSTITUTE"
	ModeTokenize   Mode = "TOKENIZE"
	ModeAllow      Mode = "ALLOW"
)

// Rule overrides the detector by lowercased field name (exact) or JSON-pointer prefix.
type Rule struct {
	Field         string `json:"field,omitempty"`
	PointerPrefix string `json:"pointerPrefix,omitempty"`
	Mode          Mode   `json:"mode"`
	// Values, when non-empty, marks this a spec-derived ENUM rule: Mode
	// applies to a leaf only if its value is a string that either exactly
	// matches one of these declared members, or still matches the general
	// enum-token shape (specEnumRE) — never unconditionally on field name
	// alone. A non-string leaf never matches. The shape half is what lets a
	// brand-new member the provider hasn't declared yet — the exact thing
	// drift detection exists to catch — still read in clear text, while a
	// free-text value arriving under the same field name (an error message,
	// say) still falls through to the fail-closed default.
	Values []string `json:"values,omitempty"`
}

type Redaction struct {
	Pointer string `json:"pointer"`
	Mode    Mode   `json:"mode"`
}

type Result struct {
	Sanitized  any         `json:"sanitized"`
	Redactions []Redaction `json:"redactions"`
}

var (
	authHeaders = set("authorization", "proxy-authorization", "x-api-key", "x-auth-token", "x-hub-signature", "x-hub-signature-256", "x-signature")
	dropHeaders = set("cookie", "set-cookie")
	safeHeaders = set("content-type", "accept", "content-length", "user-agent", "accept-encoding", "retry-after", "x-ratelimit-remaining", "x-request-id", "traceparent")

	idKeyRE     = regexp.MustCompile(`(?i)(^|_|-)(id|ids|uuid|guid|email|account|customer|user|order|ref)($|_|-)`)
	secretKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|secret|password|passwd|token|credential|auth|signature|private[_-]?key|access[_-]?key|bearer)`)
	// nameKeyRE: person/PII keys whose free-text values are enum-ish by shape, so
	// this key list is the whole defense against a name reaching disk. DROP by key.
	nameKeyRE  = regexp.MustCompile(`(?i)(^|_|-)(name|surname|username|nickname|city|street|address|dob|birthdate|birthday|birth|gender|beneficiary|sender|recipient|payee|payer|holder)($|_|-)`)
	phoneKeyRE = regexp.MustCompile(`(?i)(^|_|-)(phone|mobile|msisdn)($|_|-)`)
	// cardKeyRE: financial instrument numbers — often sent as JSON numbers,
	// which the shape detectors never see.
	cardKeyRE = regexp.MustCompile(`(?i)(^|_|-)(card|pan|iban|bvn|nin|ssn)($|_|-)`)
	// secretNumberKeyRE: short-digit secrets no shape detector can catch ("123");
	// SUBSTITUTE whatever the JSON type — {"cvv":123} and {"cvv":"123"} are equal.
	secretNumberKeyRE = regexp.MustCompile(`(?i)(^|_|-)(cvv2?|cvc2?|cid|csc|pin|otp|passcode|security[_-]?code|one[_-]?time[_-]?(code|password|pin))($|_|-)`)
	// expiryKeyRE: card expiry, tokenized regardless of JSON type; deliberately
	// stricter than the goldens allow (see parity_test.go knownStricterKeyRE).
	expiryKeyRE   = regexp.MustCompile(`(?i)(^|_|-)(expiry|expiration|exp[_-]?month|exp[_-]?year|valid[_-]?(thru|until))($|_|-)`)
	jwtRE         = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*$`)
	highEntropyRE = regexp.MustCompile(`^[A-Za-z0-9_\-+/=]{28,}$`)
	httpMethodRE  = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|TRACE|CONNECT)$`)
	longAlnumRE   = regexp.MustCompile(`^[A-Za-z0-9]{16,}$`)
	enumishRE     = regexp.MustCompile(`^[a-z][a-z._/+-]{0,30}$`)
	// specEnumRE is the WIDENED shape checked for a Rule.Values match: case-
	// insensitive and underscore-tolerant, unlike enumishRE above. It is only
	// ever consulted for a field the provider's own spec already declares as
	// an enum (see Rule.Values) — never applied globally, which is what keeps
	// it from doing what widening enumishRE itself would do (start allowing
	// free-text city/merchant/reference values through).
	specEnumRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._/+-]{0,30}$`)
	longDigitsRE  = regexp.MustCompile(`^\d{6,}$`)
	mediumAlnumRE = regexp.MustCompile(`^[A-Za-z0-9]{6,15}$`)
	prefixedIDRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9]{6,}$`)
	whitespaceRE  = regexp.MustCompile(`\s`)
)

// Classify a leaf value by key name and shape, ordered most→least confident.
// The DEFAULT is DROP (fail closed). Non-string scalars carry no PII → ALLOW.
func Classify(key string, value any, isHeader bool) Mode {
	k := strings.ToLower(key)
	if isHeader {
		if authHeaders[k] {
			return ModeSubstitute
		}
		if dropHeaders[k] {
			return ModeDrop
		}
		if safeHeaders[k] {
			return ModeAllow
		}
	}
	v, isString := value.(string)
	if !isString {
		// Numbers can be credentials/identifiers too: a PAN or account number sent
		// as a JSON number must not ride through on its type.
		switch {
		case secretKeyRE.MatchString(k) || secretNumberKeyRE.MatchString(k):
			return ModeSubstitute
		case idKeyRE.MatchString(k) || nameKeyRE.MatchString(k) || phoneKeyRE.MatchString(k) || cardKeyRE.MatchString(k) || expiryKeyRE.MatchString(k):
			return ModeTokenize
		}
		// RESIDUAL, by design: numbers carry no shape signal, so key names are the
		// only evidence and an unrecognized key passes through. `pikopod inspect` says so.
		return ModeAllow // other numbers/bools/null; containers handled by the walker
	}

	switch {
	case secretKeyRE.MatchString(k):
		return ModeSubstitute // key name says credential
	case secretNumberKeyRE.MatchString(k):
		return ModeSubstitute // cvv/pin/otp: same secret whatever the JSON type
	case jwtRE.MatchString(v):
		return ModeSubstitute
	case expiryKeyRE.MatchString(k):
		return ModeTokenize // card expiry: tokenized in string form too
	case phoneKeyRE.MatchString(k):
		return ModeTokenize // phone numbers are referential identifiers
	case nameKeyRE.MatchString(k):
		return ModeDrop // person/PII field name → no replay role, fail closed
	case idKeyRE.MatchString(k):
		return ModeTokenize
	case emailRE.MatchString(v) || uuidRE.MatchString(v) || prefixedIDRE.MatchString(v):
		return ModeTokenize
	case highEntropyRE.MatchString(v) && !whitespaceRE.MatchString(v):
		return ModeSubstitute // 28+ opaque
	case longAlnumRE.MatchString(v):
		return ModeSubstitute // long opaque alnum → secret-shaped
	case httpMethodRE.MatchString(v):
		return ModeAllow
	case enumishRE.MatchString(v):
		// Lowercase word with no digits (or mime/path) is enum-ish → keep. Must
		// precede the identifier rules so "pending" is not tokenized.
		return ModeAllow
	case longDigitsRE.MatchString(v):
		return ModeTokenize // long numeric identifier
	case mediumAlnumRE.MatchString(v):
		return ModeTokenize // medium alnum with digits/mixed → identifier
	default:
		return ModeDrop // free text / unknown shape → fail closed
	}
}

// KeyLooksLikeIdentifier reports value-shaped KEYS (emails, uuids, jwts, long
// digits, prefixed ids); opaque shapes need a digit so field names are spared.
func KeyLooksLikeIdentifier(k string) bool { return keyLooksLikeIdentifier(k) }

func keyLooksLikeIdentifier(k string) bool {
	if emailRE.MatchString(k) || uuidRE.MatchString(k) || jwtRE.MatchString(k) || longDigitsRE.MatchString(k) {
		return true
	}
	if !strings.ContainsAny(k, "0123456789") {
		return false
	}
	return prefixedIDRE.MatchString(k) ||
		multiPrefixedIDRE.MatchString(k) ||
		(highEntropyRE.MatchString(k) && !whitespaceRE.MatchString(k)) ||
		longAlnumRE.MatchString(k)
}

// multiPrefixedIDRE covers multi-segment tokens (tok_live_abc12345); the digit
// requirement above keeps ordinary snake_case field names out.
var multiPrefixedIDRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(_[A-Za-z0-9]+){2,4}$`)

// dropped marks a value for removal; pruned before returning.
type dropped struct{}

// Sanitize walks value, substituting/tokenizing/dropping leaves. isHeaderRoot
// applies header classification to top-level keys (pass headers as a map).
func Sanitize(value any, tok *Tokenizer, rules []Rule, isHeaderRoot bool) Result {
	var redactions []Redaction

	ruleFor := func(key, pointer string, value any) (Mode, bool) {
		for _, r := range rules {
			matched := (r.Field != "" && strings.EqualFold(r.Field, key)) ||
				(r.PointerPrefix != "" && strings.HasPrefix(pointer, r.PointerPrefix))
			if !matched {
				continue
			}
			if len(r.Values) > 0 {
				s, isString := value.(string)
				if !isString || !(containsString(r.Values, s) || specEnumRE.MatchString(s)) {
					continue // conditional rule doesn't cover this value; keep looking
				}
			}
			return r.Mode, true
		}
		return "", false
	}

	var walk func(node any, pointer, key string, isHeader bool) any
	walk = func(node any, pointer, key string, isHeader bool) any {
		switch n := node.(type) {
		case []any:
			out := make([]any, 0, len(n))
			for i, v := range n {
				w := walk(v, pointer+"/"+strconv.Itoa(i), key, isHeader)
				if _, isDropped := w.(dropped); !isDropped {
					out = append(out, w)
				}
			}
			return out
		case map[string]any:
			out := make(map[string]any, len(n))
			for k, v := range n {
				// Keys are payload bytes too (APIs key objects BY identifier). The
				// sanitized key is chosen FIRST: pointers persist and would leak it.
				outKey := k
				keyTokenized := false
				if keyLooksLikeIdentifier(k) {
					outKey, _ = tok.Tokenize(k)
					keyTokenized = true
				}
				childPtr := pointer + "/" + escapePointer(outKey)
				w := walk(v, childPtr, k, isHeader)
				if _, isDropped := w.(dropped); isDropped {
					continue
				}
				if keyTokenized {
					redactions = append(redactions, Redaction{Pointer: childPtr, Mode: ModeTokenize})
				}
				out[outKey] = w
			}
			return out
		}
		// Leaf: rule override wins, else the detector; fail-closed default is DROP.
		mode, ok := ruleFor(key, pointer, node)
		if !ok {
			mode = Classify(key, node, isHeader)
		}
		switch mode {
		case ModeAllow:
			return node
		case ModeDrop:
			redactions = append(redactions, Redaction{Pointer: pointer, Mode: ModeDrop})
			return dropped{}
		case ModeSubstitute:
			redactions = append(redactions, Redaction{Pointer: pointer, Mode: ModeSubstitute})
			name := key
			if name == "" {
				name = "value"
			}
			return "<<SUBSTITUTE:" + name + ">>" // resolved to a sandbox credential at replay
		default: // TOKENIZE
			redactions = append(redactions, Redaction{Pointer: pointer, Mode: ModeTokenize})
			switch s := node.(type) {
			case string:
				token, _ := tok.Tokenize(s)
				return token
			case float64:
				// Numeric identifier (PAN, account number): the JSON type becomes
				// string — a visible redaction, never the raw digits.
				token, _ := tok.Tokenize(strconv.FormatFloat(s, 'f', -1, 64))
				return token
			case json.Number:
				// UseNumber path: the literal digits, no float64 round-trip
				// (a 17-digit account number must tokenize losslessly).
				token, _ := tok.Tokenize(s.String())
				return token
			}
			return node
		}
	}

	sanitized := walk(value, "", "", isHeaderRoot)
	if _, isDropped := sanitized.(dropped); isDropped {
		sanitized = nil
	}
	return Result{Sanitized: sanitized, Redactions: redactions}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func escapePointer(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1")
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}
