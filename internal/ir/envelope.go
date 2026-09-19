package ir

import (
	"fmt"
	"sort"
	"strings"
)

// WebhookEnvelope is how a provider wraps and signs the documented payload on
// the wire; the outbox keeps the payload, the sink sees the envelope.
type WebhookEnvelope struct {
	Wrap      map[string]string `json:"wrap,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Signature *WebhookSignature `json:"signature,omitempty"`
}

type WebhookSignature struct {
	Algorithm   string `json:"algorithm"`
	Content     string `json:"content"`
	KeyEnv      string `json:"keyEnv"`
	KeyEncoding string `json:"keyEncoding,omitempty"`
	Output      string `json:"output,omitempty"`
	In          string `json:"in"`
	Name        string `json:"name"`
	Format      string `json:"format,omitempty"`
}

const (
	EnvelopeRefBody       = "body"
	EnvelopeRefBodyString = "json_string body"
	EnvelopeRefEvent      = "event"
	EnvelopeRefID         = "id"
	EnvelopeRefTimestamp  = "timestamp"
	EnvelopeRefTimestampM = "timestamp_ms"
	EnvelopeRefNow        = "now_rfc3339"
	EnvelopeRefUUID       = "uuid"
	EnvelopeRefSignature  = "signature"
)

var envelopeBuiltins = map[string]bool{
	EnvelopeRefBody: true, EnvelopeRefBodyString: true, EnvelopeRefEvent: true, EnvelopeRefID: true,
	EnvelopeRefTimestamp: true, EnvelopeRefTimestampM: true, EnvelopeRefNow: true, EnvelopeRefUUID: true,
}

var (
	envelopeAlgorithms = map[string]bool{"hmac-sha256": true, "hmac-sha512": true}
	envelopeEncodings  = map[string]bool{"": true, "raw": true, "base64": true, "hex": true}
	envelopeOutputs    = map[string]bool{"": true, "base64": true, "hex": true}
	envelopeLocations  = map[string]bool{"body": true, "header": true}
)

// TemplateRefs lists every {{ref}} in a template, trimmed, in order.
func TemplateRefs(tpl string) []string {
	var refs []string
	for rest := tpl; ; {
		open := strings.Index(rest, "{{")
		if open < 0 {
			return refs
		}
		close := strings.Index(rest[open:], "}}")
		if close < 0 {
			return append(refs, rest[open:])
		}
		refs = append(refs, strings.TrimSpace(rest[open+2:open+close]))
		rest = rest[open+close+2:]
	}
}

// Validate refuses anything the renderer could not honour, naming the field.
func (env *WebhookEnvelope) Validate() error {
	if env == nil {
		return nil
	}
	if len(env.Wrap) == 0 && len(env.Headers) == 0 && env.Signature == nil {
		return fmt.Errorf("webhook envelope declares nothing: give it wrap, headers or signature")
	}
	wrapNames := map[string]bool{}
	for _, name := range sortedKeys(env.Wrap) {
		if name == "" {
			return fmt.Errorf("webhook envelope: wrap has an empty field name")
		}
		wrapNames[name] = true
	}
	for _, name := range sortedKeys(env.Wrap) {
		if err := checkRefs("wrap."+name, env.Wrap[name], envelopeBuiltins); err != nil {
			return err
		}
	}
	allowed := map[string]bool{}
	for k := range envelopeBuiltins {
		allowed[k] = true
	}
	for k := range wrapNames {
		allowed[k] = true
	}
	for _, name := range sortedKeys(env.Headers) {
		if err := checkRefs("headers."+name, env.Headers[name], allowed); err != nil {
			return err
		}
	}
	sig := env.Signature
	if sig == nil {
		return nil
	}
	if !envelopeAlgorithms[sig.Algorithm] {
		return fmt.Errorf("webhook envelope: signature.algorithm %q is not hmac-sha256 or hmac-sha512", sig.Algorithm)
	}
	if !envelopeEncodings[sig.KeyEncoding] {
		return fmt.Errorf("webhook envelope: signature.keyEncoding %q is not raw, base64 or hex", sig.KeyEncoding)
	}
	if !envelopeOutputs[sig.Output] {
		return fmt.Errorf("webhook envelope: signature.output %q is not base64 or hex", sig.Output)
	}
	if !envelopeLocations[sig.In] {
		return fmt.Errorf("webhook envelope: signature.in %q is not body or header", sig.In)
	}
	if sig.Name == "" {
		return fmt.Errorf("webhook envelope: signature.name is empty; name the %s that carries the signature", sig.In)
	}
	if sig.In == "body" && wrapNames[sig.Name] {
		return fmt.Errorf("webhook envelope: signature.name %q is also a wrap field", sig.Name)
	}
	if sig.KeyEnv == "" {
		return fmt.Errorf("webhook envelope: signature.keyEnv is empty; name the environment variable holding the key")
	}
	if strings.TrimSpace(sig.Content) == "" {
		return fmt.Errorf("webhook envelope: signature.content is empty; say what bytes are signed")
	}
	if err := checkRefs("signature.content", sig.Content, allowed); err != nil {
		return err
	}
	if sig.Format != "" {
		formatAllowed := map[string]bool{EnvelopeRefSignature: true}
		for k := range allowed {
			formatAllowed[k] = true
		}
		if err := checkRefs("signature.format", sig.Format, formatAllowed); err != nil {
			return err
		}
		found := false
		for _, r := range TemplateRefs(sig.Format) {
			found = found || r == EnvelopeRefSignature
		}
		if !found {
			return fmt.Errorf("webhook envelope: signature.format must contain {{signature}}")
		}
	}
	return nil
}

func checkRefs(field, tpl string, allowed map[string]bool) error {
	for _, r := range TemplateRefs(tpl) {
		if strings.HasPrefix(r, "{{") {
			return fmt.Errorf("webhook envelope: %s has an unclosed {{ in %q", field, tpl)
		}
		if !allowed[r] {
			return fmt.Errorf("webhook envelope: %s refers to {{%s}}, which is not a wrap field or one of %s", field, r, strings.Join(sortedKeys(allowed), ", "))
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
