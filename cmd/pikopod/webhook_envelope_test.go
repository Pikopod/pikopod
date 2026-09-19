package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sidecarYAML = `wrap:
  timestamp: "{{now_rfc3339}}"
  payload: "{{json_string body}}"
signature:
  algorithm: hmac-sha256
  content: "{{timestamp}}{{payload}}"
  keyEnv: EXAMPLEPAY_WEBHOOK_KEY
  keyEncoding: base64
  output: base64
  in: body
  name: signature
`

func writeSidecar(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "webhooks.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSidecarEnvelopePersistsAndSurvivesUpdate(t *testing.T) {
	t.Setenv("EXAMPLEPAY_WEBHOOK_KEY", "")
	cfg := testConfig(t, "https://example.invalid")
	spec := writeSpec(t, hookSpecEmitOnly)
	if err := sandboxAdd(cfg, "bank", spec, "seed-e1", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := applyWebhookSidecar(cfg, "bank", writeSidecar(t, sidecarYAML), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hmac-sha256", "$EXAMPLEPAY_WEBHOOK_KEY", "is not set"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("sidecar output missing %q:\n%s", want, out.String())
		}
	}
	_, def, err := loadSandboxDef(cfg, "bank")
	if err != nil || def.WebhookEnvelope == nil || def.WebhookEnvelope.Signature == nil {
		t.Fatalf("envelope not persisted: %v %+v", err, def.WebhookEnvelope)
	}
	if err := sandboxUpdate(cfg, "bank", spec, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, def, _ = loadSandboxDef(cfg, "bank"); def.WebhookEnvelope == nil {
		t.Fatal("re-importing the spec must keep the sidecar envelope")
	}
	var list strings.Builder
	if err := sandboxList(cfg, &list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.String(), "envelope: hmac-sha256 in body \"signature\", key from $EXAMPLEPAY_WEBHOOK_KEY") {
		t.Fatalf("list must show the envelope:\n%s", list.String())
	}
}

func TestSidecarRefusesBadEnvelope(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "bank", writeSpec(t, hookSpecEmitOnly), "seed-e2", "", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(sidecarYAML, "{{now_rfc3339}}", "{{clock}}", 1)
	err := applyWebhookSidecar(cfg, "bank", writeSidecar(t, bad), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "{{clock}}") {
		t.Fatalf("want a refusal naming the reference, got %v", err)
	}
	if _, def, _ := loadSandboxDef(cfg, "bank"); def.WebhookEnvelope != nil {
		t.Fatal("a refused sidecar must not be persisted")
	}
}

func TestUpRefusesSignedEnvelopeWithoutKey(t *testing.T) {
	t.Setenv("EXAMPLEPAY_WEBHOOK_KEY", "")
	cfg := testConfig(t, "https://example.invalid")
	if err := sandboxAdd(cfg, "bank", writeSpec(t, hookSpecEmitOnly), "seed-e3", "http://127.0.0.1:1/hook", "", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := applyWebhookSidecar(cfg, "bank", writeSidecar(t, sidecarYAML), io.Discard); err != nil {
		t.Fatal(err)
	}
	_, err := newSandboxServer(cfg)
	if err == nil || !strings.Contains(err.Error(), "EXAMPLEPAY_WEBHOOK_KEY") {
		t.Fatalf("up must refuse before serving, naming the variable: %v", err)
	}
	t.Setenv("EXAMPLEPAY_WEBHOOK_KEY", "c2VjcmV0LWtleQ==")
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		t.Fatalf("with the key set, up must start: %v", err)
	}
	sbx.Close()
	t.Setenv("EXAMPLEPAY_WEBHOOK_KEY", "not base64!")
	if _, err := newSandboxServer(cfg); err == nil || !strings.Contains(err.Error(), "cannot decode") {
		t.Fatalf("a malformed key must be refused: %v", err)
	}
}
