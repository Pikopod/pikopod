package importer

import "testing"

const emitOnlySpec = `{"openapi":"3.1.0","info":{"title":"T","version":"1"},
"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}},
"webhooks":{
  "transaction.created":{"post":{"x-pikopod-emit-only":true,"responses":{"200":{"description":"ack"}}}},
  "payment.updated":{"post":{"responses":{"200":{"description":"ack"}}}}
}}`

// The extension marks an event no API call causes; its absence is the default.
func TestEmitOnlyExtensionCarriedIntoIR(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(emitOnlySpec))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Webhooks) != 2 {
		t.Fatalf("want 2 webhooks, got %d", len(def.Webhooks))
	}
	byEvent := map[string]bool{}
	for _, w := range def.Webhooks {
		byEvent[w.Event.Value] = w.EmitOnly
	}
	if !byEvent["transaction.created"] {
		t.Fatal("x-pikopod-emit-only: true must set EmitOnly")
	}
	if byEvent["payment.updated"] {
		t.Fatal("EmitOnly must be false without the extension")
	}
}
