package archetype

import (
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
)

const triggeredSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{"/charges":{"post":{"operationId":"createCharge","responses":{"201":{"description":"created"}}}}},
"webhooks":{"charge.created":{"post":{"x-pikopod-trigger":{"method":"post","path":"/charges"},"responses":{"200":{"description":"ack"}}}}}}`

// A trigger a person wrote into the spec is an explicit fact; the same text
// written by the model on a docs import stays extracted.
func TestUserAuthoredTriggerIsExplicitAndModelWrittenIsNot(t *testing.T) {
	authored, err := importer.NormalizeOpenAPI([]byte(triggeredSpec))
	if err != nil {
		t.Fatal(err)
	}
	if authored.Webhooks[0].Event.IsUncertain() {
		t.Fatal("a spec import must carry the event as an explicit fact")
	}
	extracted, err := importer.NormalizeLLMExtracted([]byte(triggeredSpec))
	if err != nil {
		t.Fatal(err)
	}
	if !extracted.Webhooks[0].Event.IsUncertain() {
		t.Fatal("a docs import must keep model-written events as extracted facts")
	}
	for _, a := range All() {
		if a.ID != "duplicate_delivery" {
			continue
		}
		if !Bind(&a, authored).Applicable {
			t.Fatalf("duplicate_delivery must bind on the authored spec: %s", Bind(&a, authored).Reason)
		}
		if b := Bind(&a, extracted); b.Applicable || len(b.InferredOnly) == 0 {
			t.Fatalf("on the extracted spec it must refuse but offer the candidate: %+v", b)
		}
		return
	}
	t.Fatal("duplicate_delivery archetype missing")
}
