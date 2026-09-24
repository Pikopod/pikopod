package importer

import (
	"bytes"
	"encoding/json"

	"github.com/pikopod/pikopod/internal/ir"
)

const webhookEnvelopeExtension = "x-pikopod-webhook-envelope"

func normalizeWebhookEnvelope(doc *OrdMap) (*ir.WebhookEnvelope, error) {
	raw, ok := doc.Get(webhookEnvelopeExtension)
	if !ok {
		return nil, nil
	}
	env, err := DecodeWebhookEnvelope(plainValue(raw))
	if err != nil {
		return nil, specErr(SpecConversionFailed, webhookEnvelopeExtension+": "+err.Error())
	}
	return env, nil
}

func DecodeWebhookEnvelope(value any) (*ir.WebhookEnvelope, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var env ir.WebhookEnvelope
	if err := dec.Decode(&env); err != nil {
		return nil, err
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	return &env, nil
}

func plainValue(value any) any {
	switch v := value.(type) {
	case *OrdMap:
		out := make(map[string]any, len(v.keys))
		for _, k := range v.keys {
			out[k] = plainValue(v.values[k])
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = plainValue(item)
		}
		return out
	}
	return value
}
