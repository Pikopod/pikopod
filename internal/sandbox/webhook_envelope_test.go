package sandbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/importer"
)

// A provider-documented scheme: HMAC-SHA256 over timestamp+payload with a
// base64-decoded key, base64 output, carried in the body beside a stringified
// payload. Written independently of the engine, exactly as a handler would.
const providerSigningKeyB64 = "c2VjcmV0LWtleQ=="

const bodyEnvelope = `"x-pikopod-webhook-envelope": {
    "wrap": {"timestamp": "{{now_rfc3339}}", "payload": "{{json_string body}}"},
    "signature": {"algorithm": "hmac-sha256", "content": "{{timestamp}}{{payload}}", "keyEnv": "EXAMPLEPAY_WEBHOOK_KEY",
                  "keyEncoding": "base64", "output": "base64", "in": "body", "name": "signature"}
  },
  "webhooks": {`

const headerEnvelope = `"x-pikopod-webhook-envelope": {
    "headers": {"x-examplepay-event": "{{event}}", "x-examplepay-delivery": "{{uuid}}", "x-examplepay-timestamp": "{{timestamp}}"},
    "signature": {"algorithm": "hmac-sha256", "content": "{{timestamp}}.{{body}}", "keyEnv": "EXAMPLEPAY_WEBHOOK_KEY",
                  "output": "hex", "in": "header", "name": "x-examplepay-signature", "format": "v1={{signature}}"}
  },
  "webhooks": {`

func envelopeSpec(t *testing.T, envelope string) string {
	t.Helper()
	spec := strings.Replace(nestedHookSpec, `"webhooks": {`, envelope, 1)
	if spec == nestedHookSpec {
		t.Fatal("test spec no longer has a webhooks block to extend")
	}
	return spec
}

func providerValidates(t *testing.T, body []byte) (ok bool, why string) {
	t.Helper()
	var env struct {
		Timestamp string `json:"timestamp"`
		Signature string `json:"signature"`
		Payload   string `json:"payload"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false, "body is not the documented envelope: " + err.Error()
	}
	if env.Timestamp == "" || env.Signature == "" || env.Payload == "" {
		return false, "envelope fields missing from body: " + string(body)
	}
	key, err := base64.StdEncoding.DecodeString(providerSigningKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(env.Timestamp + env.Payload))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); env.Signature != want {
		return false, "signature does not verify: got " + env.Signature + " want " + want
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(env.Payload), &inner); err != nil {
		return false, "payload is not a JSON string of the event body: " + err.Error()
	}
	if tx, _ := inner["transaction"].(map[string]any); tx == nil || tx["amount"] != 71717.0 {
		return false, "documented body not inside payload: " + env.Payload
	}
	return true, ""
}

type sinkHit struct {
	headers http.Header
	body    []byte
}

func captureSink(t *testing.T) (*httptest.Server, func() []sinkHit) {
	t.Helper()
	var mu sync.Mutex
	var hits []sinkHit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		mu.Lock()
		hits = append(hits, sinkHit{headers: r.Header.Clone(), body: body})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sinkHit { mu.Lock(); defer mu.Unlock(); return append([]sinkHit(nil), hits...) }
}

func awaitDelivered(t *testing.T, e *Engine, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.WebhookSinkStats().Delivered < n {
		if time.Now().After(deadline) {
			t.Fatalf("sink never received the delivery: %+v", e.WebhookSinkStats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func decodedKey(t *testing.T) []byte {
	t.Helper()
	key, err := DecodeSigningKey(providerSigningKeyB64, "base64")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The headline claim of the feature: a handler written against the provider's
// own documentation accepts the delivery unmodified.
func TestSinkDeliverySatisfiesTheProvidersValidator(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(envelopeSpec(t, bodyEnvelope)))
	if err != nil {
		t.Fatal(err)
	}
	srv, hits := captureSink(t)
	e := newEngine(t, def, Config{ID: "sbx_env", Seed: "s", WebhookURL: srv.URL, WebhookSigningKey: decodedKey(t)})
	do(t, e, "POST", "/transactions", `{"amount":71717,"currency":"TZS"}`, nil)
	awaitDelivered(t, e, 1)

	hit := hits()[0]
	if ok, why := providerValidates(t, hit.body); !ok {
		t.Fatalf("the provider's validator rejects pikopod's delivery: %s", why)
	}
	if hit.headers.Get("x-pikopod-webhook-signature") != "" {
		t.Fatal("with an envelope declared, pikopod's own headers must not leak onto the wire")
	}
	if got := e.Deliveries("transaction.created")[0].Payload; !strings.HasPrefix(string(got), `{"event"`) {
		t.Fatalf("the outbox must keep the documented payload, not the wire form: %s", got)
	}
}

func TestHeaderSignatureWithFormatAndDeterministicIDs(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(envelopeSpec(t, headerEnvelope)))
	if err != nil {
		t.Fatal(err)
	}
	run := func(id string) sinkHit {
		srv, hits := captureSink(t)
		e := newEngine(t, def, Config{ID: id, Seed: "s", WebhookURL: srv.URL, WebhookSigningKey: []byte("raw-key")})
		do(t, e, "POST", "/transactions", `{"amount":71717,"currency":"TZS"}`, nil)
		awaitDelivered(t, e, 1)
		return hits()[0]
	}
	hit := run("sbx_hdr_1")
	if hit.headers.Get("x-examplepay-event") != "transaction.created" {
		t.Fatalf("event header: %q", hit.headers.Get("x-examplepay-event"))
	}
	mac := hmac.New(sha256.New, []byte("raw-key"))
	mac.Write([]byte(hit.headers.Get("x-examplepay-timestamp") + "." + string(hit.body)))
	if want := "v1=" + hex.EncodeToString(mac.Sum(nil)); hit.headers.Get("x-examplepay-signature") != want {
		t.Fatalf("header signature %q, want %q", hit.headers.Get("x-examplepay-signature"), want)
	}
	if !strings.HasPrefix(string(hit.body), `{"event"`) {
		t.Fatalf("without wrap the body is the documented payload: %s", hit.body)
	}
	uuid := hit.headers.Get("x-examplepay-delivery")
	if len(uuid) != 36 || uuid[14] != '4' {
		t.Fatalf("not a v4-shaped uuid: %q", uuid)
	}
	if again := run("sbx_hdr_2"); again.headers.Get("x-examplepay-delivery") != uuid {
		t.Fatalf("uuid must be a function of the seed: %q vs %q", uuid, again.headers.Get("x-examplepay-delivery"))
	}
}

func TestSignedEnvelopeNeedsTheKeyOnlyWhenDelivering(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(envelopeSpec(t, bodyEnvelope)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, err = NewEngine(def, Config{ID: "sbx_nokey", Seed: "s", WebhookURL: "http://127.0.0.1:1/hook"}, st)
	if err == nil || !strings.Contains(err.Error(), "EXAMPLEPAY_WEBHOOK_KEY") {
		t.Fatalf("a sink with no key must be refused naming the variable, got %v", err)
	}
	if _, err := NewEngine(def, Config{ID: "sbx_nokey_nosink", Seed: "s"}, st); err != nil {
		t.Fatalf("without a sink the key is not needed: %v", err)
	}
}

func TestNoEnvelopeKeepsTodaysWireFormat(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(nestedHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	srv, hits := captureSink(t)
	e := newEngine(t, def, Config{ID: "sbx_plain", Seed: "s", WebhookURL: srv.URL})
	do(t, e, "POST", "/transactions", `{"amount":1,"currency":"TZS"}`, nil)
	awaitDelivered(t, e, 1)
	hit := hits()[0]
	d := e.Deliveries("transaction.created")[0]
	if string(hit.body) != string(d.Payload) || hit.headers.Get("x-pikopod-webhook-signature") != "sha256="+d.Signature {
		t.Fatalf("undeclared envelope must change nothing: %s %v", hit.body, hit.headers)
	}
}

func TestDecodeSigningKey(t *testing.T) {
	if k, err := DecodeSigningKey("6b6579", "hex"); err != nil || string(k) != "key" {
		t.Fatalf("hex: %q %v", k, err)
	}
	if k, err := DecodeSigningKey(" a2V5\n", "base64"); err != nil || string(k) != "key" {
		t.Fatalf("base64: %q %v", k, err)
	}
	if _, err := DecodeSigningKey("not base64!", "base64"); err == nil {
		t.Fatal("a malformed key must be refused, not silently signed with")
	}
}
