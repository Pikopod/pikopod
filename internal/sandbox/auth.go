// Auth enforcement: only EXPLICIT/DERIVED schemes, only issued `pikopod_sbx_test_`
// tokens. Issuance is seed-derived because this engine has no control plane.
package sandbox

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"regexp"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// SandboxCredentialPrefix marks issued test tokens — never a real secret.
const SandboxCredentialPrefix = "pikopod_sbx_test_"

// SandboxWebhookSecretPrefix marks issued signing secrets. Deliberately not a
// provider's prefix: it must authenticate like one, never look like one.
const SandboxWebhookSecretPrefix = "pikopod_sbx_whsec_"

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// deriveCredential mints the sandbox's deterministic issued token.
func deriveCredential(seed string) string {
	return SandboxCredentialPrefix + NewPrng(seed+":credential").Hex(48)
}

// entropyPerChar is Shannon entropy in bits per character (over code points).
func entropyPerChar(value string) float64 {
	freq := map[rune]int{}
	total := 0
	for _, ch := range value {
		freq[ch]++
		total++
	}
	bits := 0.0
	for _, count := range freq {
		p := float64(count) / float64(total)
		bits -= p * math.Log2(p)
	}
	return bits
}

var jwtSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)

func looksLikeJwt(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if !jwtSegment.MatchString(p) {
			return false
		}
	}
	return true
}

var longBase64Run = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)

func looksLikeOpaqueSecret(value string) bool {
	if len(value) < 24 {
		return false
	}
	hasLower := strings.ContainsAny(value, "abcdefghijklmnopqrstuvwxyz")
	hasUpper := strings.ContainsAny(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	hasDigit := strings.ContainsAny(value, "0123456789")
	classes := 0
	for _, b := range []bool{hasLower, hasUpper, hasDigit} {
		if b {
			classes++
		}
	}
	longRun := longBase64Run.MatchString(value)
	return (classes >= 2 && entropyPerChar(value) >= 3.2) || (longRun && entropyPerChar(value) >= 3.8)
}

// looksLikeRealSecret distinguishes a real, sensitive credential from a benign
// test token.
func looksLikeRealSecret(value string) bool {
	v := strings.TrimSpace(value)
	if len(v) == 0 {
		return false
	}
	if strings.HasPrefix(v, SandboxCredentialPrefix) {
		return false // our own token
	}
	return looksLikeJwt(v) || looksLikeOpaqueSecret(v)
}

// isEnforceableScheme: only apiKey (with a known parameter name) and HTTP
// bearer/basic are simulated; provenance must be trusted (not INFERRED/LLM).
func isEnforceableScheme(scheme *ir.AuthScheme) bool {
	if scheme.Kind.IsGuess() {
		return false // heuristic guesses are never enforced; LLM-extracted schemes are (ir.Prov.IsGuess)
	}
	switch scheme.Kind.Value {
	case "apiKey":
		return scheme.ParameterName != nil
	case "http":
		s := ""
		if scheme.Scheme != nil {
			s = strings.ToLower(scheme.Scheme.Value)
		}
		return s == "bearer" || s == "basic"
	}
	return false
}

var wsSplit = regexp.MustCompile(`\s+`)

// extractCredential pulls the presented credential for a scheme (nil = absent).
func extractCredential(scheme *ir.AuthScheme, req *ingressRequest) *string {
	if scheme.Kind.Value == "apiKey" {
		if scheme.ParameterName == nil {
			return nil
		}
		param := scheme.ParameterName.Value
		loc := ""
		if scheme.Location != nil {
			loc = scheme.Location.Value
		}
		switch loc {
		case "header":
			return req.header(param)
		case "query":
			if vs, ok := req.query[param]; ok && len(vs) > 0 {
				return &vs[0]
			}
			return nil
		case "cookie":
			return requestCookie(req, param)
		}
		return nil
	}
	// http bearer / basic
	auth := req.header("authorization")
	if auth == nil {
		return nil
	}
	parts := wsSplit.Split(*auth, -1)
	typ, rest := "", ""
	restPresent := false
	if len(parts) > 0 {
		typ = parts[0]
	}
	if len(parts) > 1 {
		rest = parts[1]
		restPresent = true
	}
	kind := ""
	if scheme.Scheme != nil {
		kind = strings.ToLower(scheme.Scheme.Value)
	}
	if kind == "bearer" && strings.ToLower(typ) == "bearer" {
		if !restPresent {
			return nil
		}
		return &rest
	}
	if kind == "basic" && strings.ToLower(typ) == "basic" && restPresent && rest != "" {
		decoded := lenientBase64(rest)
		colon := strings.Index(decoded, ":")
		out := decoded
		if colon != -1 {
			out = decoded[colon+1:] // the password half
		}
		return &out
	}
	return nil
}

// lenientBase64 tolerates missing padding; an undecodable input yields "".
func lenientBase64(s string) string {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b)
		}
	}
	return ""
}

func requestCookie(req *ingressRequest, name string) *string {
	raw := req.header("cookie")
	if raw == nil {
		return nil
	}
	for _, part := range strings.Split(*raw, ";") {
		eq := strings.Index(part, "=")
		if eq != -1 && strings.TrimSpace(part[:eq]) == name {
			v := strings.TrimSpace(part[eq+1:])
			return &v
		}
	}
	return nil
}

func wwwAuthenticate(schemes []*ir.AuthScheme) map[string]string {
	for _, s := range schemes {
		if s.Kind.Value == "http" && s.Scheme != nil && strings.ToLower(s.Scheme.Value) == "bearer" {
			return map[string]string{"www-authenticate": "Bearer"}
		}
	}
	return map[string]string{}
}

// enforceAuth returns nil when the request may proceed, else a mirrored 401/403.
func (e *Engine) enforceAuth(endpoint *ir.Endpoint, req *ingressRequest) *RawResponse {
	if len(endpoint.Security) == 0 {
		return nil // open endpoint
	}

	type requirement struct {
		scheme *ir.AuthScheme
		value  *string
	}
	var requirements []requirement
	for _, sec := range endpoint.Security {
		scheme, ok := e.authSchemes[sec.SchemeID]
		if !ok || !isEnforceableScheme(scheme) {
			continue
		}
		requirements = append(requirements, requirement{scheme: scheme, value: extractCredential(scheme, req)})
	}
	if len(requirements) == 0 {
		return nil // nothing enforceable (e.g. inferred / oauth)
	}

	// Leak guard first: any real-looking secret is rejected loudly.
	for _, r := range requirements {
		if r.value != nil && looksLikeRealSecret(*r.value) {
			return buildErrorResponse(403,
				"A real-looking credential was presented to a sandbox. Sandboxes accept only issued test credentials (prefix "+SandboxCredentialPrefix+").",
				nil)
		}
	}

	anyPresented := false
	for _, r := range requirements {
		if r.value == nil {
			continue
		}
		anyPresented = true
		if e.credentialHashes[hashToken(*r.value)] {
			return nil // any single valid presented credential satisfies auth
		}
	}

	schemes := make([]*ir.AuthScheme, len(requirements))
	for i, r := range requirements {
		schemes[i] = r.scheme
	}
	message := "Authentication required"
	if anyPresented {
		message = "Unauthorized"
	}
	return buildErrorResponse(401, message, wwwAuthenticate(schemes))
}

// IssuedCredential exposes the sandbox's deterministic token so the CLI can print
// it. Safe to display: a TEST credential by construction.
func IssuedCredential(seed string) string { return deriveCredential(seed) }
