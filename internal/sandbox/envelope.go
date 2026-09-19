package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/ir"
)

type envelopeRenderer struct {
	spec *ir.WebhookEnvelope
	key  []byte
	seed string
}

type wireDelivery struct {
	Body    []byte
	Headers map[string]string
}

// render produces exactly what the provider would put on the wire for d.
func (r *envelopeRenderer) render(d *WebhookDelivery) (*wireDelivery, error) {
	vars := map[string]string{
		ir.EnvelopeRefBody:       string(d.Payload),
		ir.EnvelopeRefBodyString: string(d.Payload),
		ir.EnvelopeRefEvent:      d.Event,
		ir.EnvelopeRefID:         d.ID,
		ir.EnvelopeRefTimestamp:  strconv.FormatInt(d.VirtualTimeMs/1000, 10),
		ir.EnvelopeRefTimestampM: strconv.FormatInt(d.VirtualTimeMs, 10),
		ir.EnvelopeRefNow:        time.UnixMilli(d.VirtualTimeMs).UTC().Format(time.RFC3339),
		ir.EnvelopeRefUUID:       deterministicUUID(r.seed + ":webhook:uuid:" + d.ID),
	}
	body := map[string]any{}
	if len(r.spec.Wrap) == 0 {
		if err := json.Unmarshal(d.Payload, &body); err != nil {
			return nil, fmt.Errorf("payload for %s is not an object, so nothing can be added beside it", d.Event)
		}
	}
	for name, tpl := range r.spec.Wrap {
		if strings.TrimSpace(tpl) == "{{"+ir.EnvelopeRefBody+"}}" {
			body[name] = json.RawMessage(d.Payload)
			vars[name] = string(d.Payload)
			continue
		}
		rendered := expand(tpl, vars)
		body[name] = rendered
		vars[name] = rendered
	}
	headers := map[string]string{"content-type": "application/json"}
	for name, tpl := range r.spec.Headers {
		headers[strings.ToLower(name)] = expand(tpl, vars)
	}
	if sig := r.spec.Signature; sig != nil {
		value, err := r.sign(sig, expand(sig.Content, vars))
		if err != nil {
			return nil, err
		}
		if sig.Format != "" {
			vars[ir.EnvelopeRefSignature] = value
			value = expand(sig.Format, vars)
		}
		if sig.In == "header" {
			headers[strings.ToLower(sig.Name)] = value
		} else {
			body[sig.Name] = value
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &wireDelivery{Body: raw, Headers: headers}, nil
}

func (r *envelopeRenderer) sign(sig *ir.WebhookSignature, content string) (string, error) {
	var h func() hash.Hash
	switch sig.Algorithm {
	case "hmac-sha256":
		h = sha256.New
	case "hmac-sha512":
		h = sha512.New
	default:
		return "", fmt.Errorf("unsupported signature algorithm %q", sig.Algorithm)
	}
	mac := hmac.New(h, r.key)
	mac.Write([]byte(content))
	sum := mac.Sum(nil)
	if sig.Output == "hex" {
		return hex.EncodeToString(sum), nil
	}
	return base64.StdEncoding.EncodeToString(sum), nil
}

func expand(tpl string, vars map[string]string) string {
	var out strings.Builder
	rest := tpl
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			out.WriteString(rest)
			return out.String()
		}
		close := strings.Index(rest[open:], "}}")
		if close < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:open])
		out.WriteString(vars[strings.TrimSpace(rest[open+2:open+close])])
		rest = rest[open+close+2:]
	}
}

// DecodeSigningKey turns the environment value into key bytes per the
// declared encoding.
func DecodeSigningKey(value, encoding string) ([]byte, error) {
	switch encoding {
	case "", "raw":
		return []byte(value), nil
	case "base64":
		return base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	case "hex":
		return hex.DecodeString(strings.TrimSpace(value))
	}
	return nil, fmt.Errorf("unknown key encoding %q", encoding)
}

func deterministicUUID(seed string) string {
	raw, _ := hex.DecodeString(NewPrng(seed).Hex(32))
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	s := hex.EncodeToString(raw)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
