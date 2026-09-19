package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
)

// One declared, triggered webhook and a second resource nothing declares an
// event for. The outbox is on, so an invented <slug>.created would show.
const undeclaredHookSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Bank", "version": "1"},
  "paths": {
    "/virtual-accounts": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"currency": {"type": "string"}}}}}},
        "responses": {"201": {"description": "created"}}
      }
    },
    "/payouts": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"amount": {"type": "number"}}}}}},
        "responses": {"201": {"description": "created"}}
      }
    }
  },
  "webhooks": {
    "virtualaccount.approved": {
      "post": {
        "x-pikopod-trigger": {"method": "post", "path": "/virtual-accounts"},
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"event": {"type": "string"}}}}}},
        "responses": {"200": {"description": "ack"}}
      }
    }
  }
}`

// The provider documents no event for payouts, so pikopod must send none.
// An invented one is indistinguishable at the handler from a real delivery.
func TestNoDeliveryForUndeclaredEvent(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(undeclaredHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_undeclared", Seed: "s"})
	do(t, e, "POST", "/payouts", `{"amount":5000}`, nil)

	if all := e.Deliveries(""); len(all) != 0 {
		var events []string
		for _, d := range all {
			events = append(events, d.Event)
		}
		t.Fatalf("the sandbox invented %d event(s) the provider never sends: %v", len(all), events)
	}
}

// The resource sits under "transaction", not "data" or "object": the shape
// many payment providers document.
const nestedHookSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Pay", "version": "1"},
  "paths": {
    "/transactions": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"amount": {"type": "number"}, "currency": {"type": "string"}}}}}},
        "responses": {"201": {"description": "created"}}
      }
    }
  },
  "webhooks": {
    "transaction.created": {
      "post": {
        "x-pikopod-trigger": {"method": "post", "path": "/transactions"},
        "requestBody": {"content": {"application/json": {"schema": {
          "type": "object",
          "properties": {
            "event": {"type": "string"},
            "transaction": {"type": "object", "properties": {
              "id": {"type": "string"},
              "amount": {"type": "number"},
              "currency": {"type": "string"}
            }}
          }
        }}}},
        "responses": {"200": {"description": "ack"}}
      }
    }
  }
}`

// The caller's values must reach a nested payload, not be replaced by synthesis.
func TestOverlayGraftsIntoNestedPayload(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(nestedHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_nested", Seed: "s"})
	do(t, e, "POST", "/transactions", `{"amount":71717,"currency":"TZS"}`, nil)

	got := e.Deliveries("transaction.created")
	if len(got) != 1 {
		t.Fatalf("want one transaction.created delivery, got %d", len(got))
	}
	var payload map[string]any
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	tx, _ := payload["transaction"].(map[string]any)
	if tx == nil {
		t.Fatalf("payload must follow the documented shape: %s", got[0].Payload)
	}
	if tx["amount"] != 71717.0 || tx["currency"] != "TZS" {
		t.Fatalf("real data dropped from the nested payload: amount=%v currency=%v", tx["amount"], tx["currency"])
	}
}

// Emitting is a trigger for DECLARED events, not a way to post arbitrary JSON.
func TestEmitWebhookRefusesUndeclaredEvent(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(undeclaredHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_emit_refuse", Seed: "s"})
	err = e.EmitWebhook("payout.settled", nil)
	if err == nil {
		t.Fatal("an undeclared event must be refused")
	}
	if msg := err.Error(); !strings.Contains(msg, "payout.settled") || !strings.Contains(msg, "not declared") {
		t.Fatalf("refusal must name the event and the reason: %s", msg)
	}
	if got := e.Deliveries(""); len(got) != 0 {
		t.Fatalf("a refused emit must send nothing, got %d", len(got))
	}
}

// A declared event fires on demand, and the caller's data reaches the nested
// payload through the same overlay that serves triggered deliveries.
func TestEmitWebhookFiresDeclaredEventWithData(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(nestedHookSpec))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, def, Config{ID: "sbx_emit_ok", Seed: "s"})
	if err := e.EmitWebhook("transaction.created", json.RawMessage(`{"amount":50000,"currency":"KES"}`)); err != nil {
		t.Fatal(err)
	}
	got := e.Deliveries("transaction.created")
	if len(got) != 1 {
		t.Fatalf("want one delivery, got %d", len(got))
	}
	var payload map[string]any
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	tx, _ := payload["transaction"].(map[string]any)
	if tx == nil || tx["amount"] != 50000.0 || tx["currency"] != "KES" {
		t.Fatalf("emitted data must overlay the nested payload: %s", got[0].Payload)
	}
}

// A miss is reported, never papered over: with no overlapping key the overlay
// says so and leaves the payload untouched; with one it grafts into the object
// that shares the most keys, however deep.
func TestOverlayReportsAMissAndGraftsByShape(t *testing.T) {
	inner := NewJSONObject()
	inner.Set("id", "synth")
	inner.Set("amount", 1.0)
	payload := NewJSONObject()
	payload.Set("event", "x")
	payload.Set("transaction", inner)

	if overlayResourceData(payload, map[string]any{"unrelated": 1}) {
		t.Fatal("no key overlaps; the overlay must report a miss, not claim a graft")
	}
	if v, _ := inner.Get("id"); v != "synth" {
		t.Fatal("a miss must leave the payload untouched")
	}
	if !overlayResourceData(payload, map[string]any{"id": "real", "amount": 71717.0}) {
		t.Fatal("overlapping keys must graft")
	}
	if v, _ := inner.Get("amount"); v != 71717.0 {
		t.Fatalf("graft must land in the nested object that shares the keys, got %v", v)
	}
}
