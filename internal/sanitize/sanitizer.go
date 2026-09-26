package sanitize

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

type Mode string

const (
	ModeDrop       Mode = "DROP"
	ModeSubstitute Mode = "SUBSTITUTE"
	ModeTokenize   Mode = "TOKENIZE"
	ModeAllow      Mode = "ALLOW"
)

type Rule struct {
	Field         string   `json:"field,omitempty"`
	PointerPrefix string   `json:"pointerPrefix,omitempty"`
	Mode          Mode     `json:"mode"`
	AllowedValues []string `json:"allowedValues,omitempty"`
}

func (r *Rule) admits(node any) bool {
	if len(r.AllowedValues) == 0 {
		return true
	}
	s, ok := node.(string)
	if !ok {
		return false
	}
	for _, v := range r.AllowedValues {
		if v == s {
			return true
		}
	}
	return false
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

	nameKeyRE  = regexp.MustCompile(`(?i)(^|_|-)(name|surname|username|nickname|city|street|address|dob|birthdate|birthday|birth|gender|beneficiary|sender|recipient|payee|payer|holder)($|_|-)`)
	phoneKeyRE = regexp.MustCompile(`(?i)(^|_|-)(phone|mobile|msisdn)($|_|-)`)

	cardKeyRE = regexp.MustCompile(`(?i)(^|_|-)(card|pan|iban|bvn|nin|ssn)($|_|-)`)

	secretNumberKeyRE = regexp.MustCompile(`(?i)(^|_|-)(cvv2?|cvc2?|cid|csc|pin|otp|passcode|security[_-]?code|one[_-]?time[_-]?(code|password|pin))($|_|-)`)

	expiryKeyRE   = regexp.MustCompile(`(?i)(^|_|-)(expiry|expiration|exp[_-]?month|exp[_-]?year|valid[_-]?(thru|until))($|_|-)`)
	jwtRE         = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*$`)
	highEntropyRE = regexp.MustCompile(`^[A-Za-z0-9_\-+/=]{28,}$`)
	httpMethodRE  = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|TRACE|CONNECT)$`)
	longAlnumRE   = regexp.MustCompile(`^[A-Za-z0-9]{16,}$`)
	enumishRE     = regexp.MustCompile(`^[a-z][a-z._/+-]{0,30}$`)
	longDigitsRE  = regexp.MustCompile(`^\d{6,}$`)
	mediumAlnumRE = regexp.MustCompile(`^[A-Za-z0-9]{6,15}$`)
	prefixedIDRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9]{6,}$`)
	whitespaceRE  = regexp.MustCompile(`\s`)
)

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

		switch {
		case secretKeyRE.MatchString(k) || secretNumberKeyRE.MatchString(k):
			return ModeSubstitute
		case idKeyRE.MatchString(k) || nameKeyRE.MatchString(k) || phoneKeyRE.MatchString(k) || cardKeyRE.MatchString(k) || expiryKeyRE.MatchString(k):
			return ModeTokenize
		}

		return ModeAllow
	}

	if shortEnumishRE.MatchString(v) && plainSafeKey(k) {
		return ModeAllow
	}
	return classifySlow(k, v)
}

func classifySlow(k, v string) Mode {
	switch {
	case secretKeyRE.MatchString(k):
		return ModeSubstitute
	case secretNumberKeyRE.MatchString(k):
		return ModeSubstitute
	case jwtRE.MatchString(v):
		return ModeSubstitute
	case expiryKeyRE.MatchString(k):
		return ModeTokenize
	case phoneKeyRE.MatchString(k):
		return ModeTokenize
	case nameKeyRE.MatchString(k):
		return ModeDrop
	case idKeyRE.MatchString(k):
		return ModeTokenize
	case emailRE.MatchString(v) || uuidRE.MatchString(v) || prefixedIDRE.MatchString(v):
		return ModeTokenize
	case highEntropyRE.MatchString(v) && !whitespaceRE.MatchString(v):
		return ModeSubstitute
	case longAlnumRE.MatchString(v):
		return ModeSubstitute
	case httpMethodRE.MatchString(v):
		return ModeAllow
	case enumishRE.MatchString(v):
		return ModeAllow
	case longDigitsRE.MatchString(v):
		return ModeTokenize
	case mediumAlnumRE.MatchString(v):
		return ModeTokenize
	default:
		return ModeDrop
	}
}

var shortEnumishRE = regexp.MustCompile(`^[a-z][a-z._/+-]{0,14}$`)

func plainSafeKey(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return !secretKeyRE.MatchString(k) &&
		!secretNumberKeyRE.MatchString(k) &&
		!idKeyRE.MatchString(k) &&
		!nameKeyRE.MatchString(k) &&
		!phoneKeyRE.MatchString(k) &&
		!cardKeyRE.MatchString(k) &&
		!expiryKeyRE.MatchString(k)
}

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

var multiPrefixedIDRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(_[A-Za-z0-9]+){2,4}$`)

type dropped struct{}

func Sanitize(value any, tok *Tokenizer, rules []Rule, isHeaderRoot bool) Result {
	var redactions []Redaction

	ruleFor := func(key, pointer string, node any) (Mode, bool) {
		for i := range rules {
			r := &rules[i]
			if !r.admits(node) {
				continue
			}
			if r.Field != "" && strings.EqualFold(r.Field, key) {
				return r.Mode, true
			}
			if r.PointerPrefix != "" && strings.HasPrefix(pointer, r.PointerPrefix) {
				return r.Mode, true
			}
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
			return "<<SUBSTITUTE:" + name + ">>"
		default:
			redactions = append(redactions, Redaction{Pointer: pointer, Mode: ModeTokenize})
			switch s := node.(type) {
			case string:
				token, _ := tok.Tokenize(s)
				return token
			case float64:

				token, _ := tok.Tokenize(strconv.FormatFloat(s, 'f', -1, 64))
				return token
			case json.Number:

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
