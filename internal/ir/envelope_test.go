package ir

import (
	"strings"
	"testing"
)

func signedEnvelope() *WebhookEnvelope {
	return &WebhookEnvelope{
		Wrap: map[string]string{"timestamp": "{{now_rfc3339}}", "payload": "{{json_string body}}"},
		Signature: &WebhookSignature{
			Algorithm: "hmac-sha256", Content: "{{timestamp}}{{payload}}", KeyEnv: "EXAMPLEPAY_WEBHOOK_KEY",
			KeyEncoding: "base64", Output: "base64", In: "body", Name: "signature",
		},
	}
}

func TestEnvelopeValidateAcceptsTheDocumentedShape(t *testing.T) {
	if err := signedEnvelope().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeValidateNamesTheProblem(t *testing.T) {
	cases := []struct {
		name string
		edit func(e *WebhookEnvelope)
		want string
	}{
		{"unknown ref in wrap", func(e *WebhookEnvelope) { e.Wrap["ts"] = "{{nope}}" }, "{{nope}}"},
		{"wrap cannot read another wrap field", func(e *WebhookEnvelope) { e.Wrap["copy"] = "{{payload}}" }, "{{payload}}"},
		{"unclosed placeholder", func(e *WebhookEnvelope) { e.Wrap["ts"] = "{{now_rfc3339" }, "unclosed"},
		{"bad algorithm", func(e *WebhookEnvelope) { e.Signature.Algorithm = "md5" }, "algorithm"},
		{"bad key encoding", func(e *WebhookEnvelope) { e.Signature.KeyEncoding = "pem" }, "keyEncoding"},
		{"bad output", func(e *WebhookEnvelope) { e.Signature.Output = "binary" }, "output"},
		{"bad location", func(e *WebhookEnvelope) { e.Signature.In = "query" }, "signature.in"},
		{"no name", func(e *WebhookEnvelope) { e.Signature.Name = "" }, "signature.name"},
		{"name collides with wrap", func(e *WebhookEnvelope) { e.Signature.Name = "payload" }, "also a wrap field"},
		{"no key env", func(e *WebhookEnvelope) { e.Signature.KeyEnv = "" }, "keyEnv"},
		{"no content", func(e *WebhookEnvelope) { e.Signature.Content = " " }, "content is empty"},
		{"signature ref outside format", func(e *WebhookEnvelope) { e.Signature.Content = "{{signature}}" }, "{{signature}}"},
		{"format without signature", func(e *WebhookEnvelope) { e.Signature.Format = "sha256=x" }, "must contain {{signature}}"},
		{"empty envelope", func(e *WebhookEnvelope) { e.Wrap = nil; e.Signature = nil }, "declares nothing"},
	}
	for _, tc := range cases {
		e := signedEnvelope()
		tc.edit(e)
		err := e.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestTemplateRefs(t *testing.T) {
	got := TemplateRefs("a{{ x }}b{{y}}{{z")
	want := []string{"x", "y", "{{z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}
