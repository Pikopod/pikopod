package importer

import (
	"strings"
	"testing"
)

const envelopeSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}},
"webhooks":{"transaction.created":{"post":{"x-pikopod-emit-only":true,"responses":{"200":{"description":"ack"}}}}},
"x-pikopod-webhook-envelope":{
  "wrap":{"timestamp":"{{now_rfc3339}}","payload":"{{json_string body}}"},
  "signature":{"algorithm":"hmac-sha256","content":"{{timestamp}}{{payload}}","keyEnv":"EXAMPLEPAY_WEBHOOK_KEY",
               "keyEncoding":"base64","output":"base64","in":"body","name":"signature"}
}}`

func TestWebhookEnvelopeExtensionCarriedIntoIR(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(envelopeSpec))
	if err != nil {
		t.Fatal(err)
	}
	env := def.WebhookEnvelope
	if env == nil || env.Signature == nil {
		t.Fatalf("envelope not carried: %+v", env)
	}
	if env.Wrap["payload"] != "{{json_string body}}" || env.Signature.KeyEnv != "EXAMPLEPAY_WEBHOOK_KEY" {
		t.Fatalf("envelope fields lost: %+v %+v", env.Wrap, env.Signature)
	}
}

func TestWebhookEnvelopeAbsentByDefault(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(emitOnlySpec))
	if err != nil {
		t.Fatal(err)
	}
	if def.WebhookEnvelope != nil {
		t.Fatal("a spec without the extension must carry no envelope")
	}
}

func TestWebhookEnvelopeRefusedAtImport(t *testing.T) {
	cases := map[string]string{
		"unknown reference": strings.Replace(envelopeSpec, "{{now_rfc3339}}", "{{clock}}", 1),
		"unknown field":     strings.Replace(envelopeSpec, `"wrap"`, `"wraps"`, 1),
		"wrong type":        strings.Replace(envelopeSpec, `"{{now_rfc3339}}"`, `12`, 1),
	}
	for name, spec := range cases {
		if _, err := NormalizeOpenAPI([]byte(spec)); err == nil || !strings.Contains(err.Error(), "x-pikopod-webhook-envelope") {
			t.Errorf("%s: want a refusal naming the extension, got %v", name, err)
		}
	}
}
