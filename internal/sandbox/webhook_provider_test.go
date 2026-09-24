package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
)

const providerHookSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Bank", "version": "1"},
  "paths": {
    "/virtual-accounts": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"currency": {"type": "string"}}}}}},
        "responses": {"201": {"description": "created"}}
      },
      "get": {"responses": {"200": {"description": "ok"}}}
    }
  },
  "webhooks": {
    "virtualaccount.approved": {
      "post": {
        "x-pikopod-trigger": {"method": "post", "path": "/virtual-accounts"},
        "requestBody": {"content": {"application/json": {"schema": {
          "type": "object",
          "properties": {
            "event": {"type": "string"},
            "data": {"type": "object", "properties": {
              "id": {"type": "string"},
              "currency": {"type": "string"},
              "account_number": {"type": "string"},
              "status": {"type": "string", "enum": ["approved"]}
            }}
          }
        }}}},
        "responses": {"200": {"description": "ack"}}
      }
    }
  }
}`

func TestProviderFaithfulWebhookEmission(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(providerHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Webhooks) != 1 || def.Webhooks[0].Trigger == nil {
		t.Fatalf("trigger not carried into IR: %+v", def.Webhooks)
	}
	e := newEngine(t, def, Config{ID: "sbx_prov", Seed: "s"})

	do(t, e, "POST", "/virtual-accounts", `{"currency":"NGN"}`, nil)
	got := e.Deliveries("virtualaccount.approved")
	if len(got) != 1 {
		t.Fatalf("trigger must fire the PROVIDER'S event name, got %d deliveries (all: %v)", len(got), e.Deliveries(""))
	}
	var payload map[string]any
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}

	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("payload must match the documented schema: %v", payload)
	}

	if data["status"] != "approved" {
		t.Fatalf("documented enum value expected, got %v", data["status"])
	}

	if data["currency"] != "NGN" {
		t.Fatalf("resource data must overlay the synthesized payload, got %v", data["currency"])
	}
	if s, _ := data["account_number"].(string); s == "" {
		t.Fatal("undocumented-by-resource fields must still synthesize")
	}

	if extra := e.Deliveries("virtual-accounts.created"); len(extra) != 0 {
		t.Fatalf("trigger-matched operations must not double-fire the generic event: %d", len(extra))
	}

	e2 := newEngine(t, def, Config{ID: "sbx_prov2", Seed: "s"})
	do(t, e2, "POST", "/virtual-accounts", `{"currency":"NGN"}`, nil)
	got2 := e2.Deliveries("virtualaccount.approved")
	if string(got[0].Payload) != string(got2[0].Payload) {
		t.Fatalf("same seed must produce identical payloads:\n%s\n%s", got[0].Payload, got2[0].Payload)
	}
}

func TestDelayWebhookFaultIsVirtual(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(providerHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_delay", Seed: "s"})
	e.ArmFault(FaultRule{Kind: FaultDelayWebhook, Event: "virtualaccount.approved", DelayMs: 60000, Probability: 1})
	do(t, e, "POST", "/virtual-accounts", `{"currency":"NGN"}`, nil)

	now := e.VirtualClockMs()

	if due, _ := e.DeliveriesDueBy("virtualaccount.approved", now); len(due) != 0 {
		t.Fatalf("delayed delivery must be invisible before due, got %d", len(due))
	}

	due, arrival := e.DeliveriesDueBy("virtualaccount.approved", now+120000)
	if len(due) != 1 || arrival != now+60000 {
		t.Fatalf("delayed delivery must arrive at due time: n=%d arrival=%d want %d", len(due), arrival, now+60000)
	}

	do(t, e, "POST", "/virtual-accounts", `{"currency":"USD"}`, nil)
	all, _ := e.DeliveriesDueBy("virtualaccount.approved", now+120000)
	onTime := 0
	for _, d := range all {
		if d.DueMs == 0 {
			onTime++
		}
	}
	if onTime != 1 {
		t.Fatalf("delay fault must be one-shot, on-time=%d of %d", onTime, len(all))
	}
}

func TestLegacyGenericEmissionUnchanged(t *testing.T) {
	spec := strings.Replace(providerHookSpec, `"x-pikopod-trigger": {"method": "post", "path": "/virtual-accounts"},`, "", 1)
	spec = strings.Replace(spec, `"virtualaccount.approved"`, `"virtual-accounts.created"`, 1)
	def, err := importer.NormalizeOpenAPI([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_legacy", Seed: "s"})
	do(t, e, "POST", "/virtual-accounts", `{"currency":"NGN"}`, nil)
	if got := e.Deliveries("virtual-accounts.created"); len(got) != 1 {
		t.Fatalf("name-matched declared event must fire, got %d", len(got))
	}
}
