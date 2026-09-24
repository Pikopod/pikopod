package sandbox

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

const webhookWidgetsSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Widgets", "version": "1.0.0"},
  "webhooks": {
    "widgets.created": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}, "responses": {"200": {"description": "ack"}}}},
    "widgets.updated": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}, "responses": {"200": {"description": "ack"}}}},
    "widgets.deleted": {"post": {"responses": {"200": {"description": "ack"}}}}
  },
  "paths": {
    "/widgets": {
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
        "type": "object",
        "properties": {"items": {"type": "array", "items": {"$ref": "#/components/schemas/Widget"}}, "total": {"type": "integer"}}
      }}}}}},
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"201": {"description": "created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      }
    },
    "/widgets/{widgetId}": {
      "parameters": [{"name": "widgetId", "in": "path", "required": true, "schema": {"type": "string"}}],
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}},
      "patch": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      },
      "delete": {"responses": {"204": {"description": "gone"}}}
    }
  },
  "components": {"schemas": {"Widget": {
    "type": "object",
    "properties": {"id": {"type": "string"}, "name": {"type": "string"}, "createdAt": {"type": "string", "format": "date-time"}}
  }}}
}`

func loadWebhookWidgets(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(webhookWidgetsSpec))
	if err != nil {
		t.Fatalf("normalize webhook widgets spec: %v", err)
	}
	if len(def.Webhooks) != 3 {
		t.Fatalf("importer must populate ir.Webhooks from the OpenAPI webhooks section, got %d", len(def.Webhooks))
	}
	return def
}

func driveCrud(t *testing.T, e *Engine) {
	t.Helper()
	if r := do(t, e, "POST", "/widgets", `{"name":"a"}`, nil); r.status != 201 {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	if r := do(t, e, "PATCH", "/widgets/widgets_1", `{"name":"b"}`, nil); r.status != 200 {
		t.Fatalf("update: %d %s", r.status, r.body)
	}
	if r := do(t, e, "DELETE", "/widgets/widgets_1", "", nil); r.status != 204 {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
}

func TestWebhookOutboxDeterministic(t *testing.T) {
	def := loadWebhookWidgets(t)
	run := func(seed string) []WebhookDelivery {
		e := newEngine(t, def, Config{ID: "sbx_wh", Seed: seed})
		driveCrud(t, e)
		return e.Deliveries("")
	}
	a, b := run("seed-wh"), run("seed-wh")
	if len(a) != 3 {
		t.Fatalf("expected 3 deliveries (created/updated/deleted), got %d", len(a))
	}
	for i := range a {
		if a[i].ID != b[i].ID || string(a[i].Payload) != string(b[i].Payload) || a[i].Signature != b[i].Signature {
			t.Fatalf("delivery %d not deterministic:\n%+v\nvs\n%+v", i, a[i], b[i])
		}
	}
	if events := []string{a[0].Event, a[1].Event, a[2].Event}; events[0] != "widgets.created" || events[1] != "widgets.updated" || events[2] != "widgets.deleted" {
		t.Fatalf("unexpected events: %v", events)
	}

	var env map[string]any
	if err := json.Unmarshal(a[2].Payload, &env); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if env["id"] != a[2].ID || env["event"] != "widgets.deleted" {
		t.Fatalf("envelope id/event wrong: %v", env)
	}
	if sec, ok := env["created"].(float64); !ok || int64(sec) != SandboxBaseEpochMs/1000 {
		t.Fatalf("created must be the virtual clock in seconds, got %v", env["created"])
	}
	data, _ := env["data"].(map[string]any)
	if data == nil || data["id"] != "widgets_1" || len(data) != 1 {
		t.Fatalf("deleted data must be {id: key}, got %v", env["data"])
	}
	if !strings.HasPrefix(a[0].ID, "sbxd_") {
		t.Fatalf("delivery id prefix: %s", a[0].ID)
	}

	e := newEngine(t, def, Config{ID: "sbx_wh_sig", Seed: "seed-wh"})
	want := signWebhookPayload(e.WebhookSecret(), a[0].VirtualTimeMs/1000, a[0].Payload)
	if !hmac.Equal([]byte(want), []byte(a[0].Signature)) {
		t.Fatalf("signature mismatch: want %s got %s", want, a[0].Signature)
	}

	c := run("seed-other")
	if c[0].ID == a[0].ID {
		t.Fatal("a different seed must change delivery ids")
	}
}

func TestWebhookOutboxOffWithoutDeclaration(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_nowh", Seed: "s"})
	if r := do(t, e, "POST", "/widgets", `{"name":"a"}`, nil); r.status != 201 {
		t.Fatalf("create: %d", r.status)
	}
	if got := e.Deliveries(""); len(got) != 0 {
		t.Fatalf("no webhooks declared, yet %d deliveries", len(got))
	}
}

func TestWebhookFaultDuplicate(t *testing.T) {
	e := newEngine(t, loadWebhookWidgets(t), Config{ID: "sbx_dup", Seed: "s"})
	e.ArmFault(FaultRule{Kind: FaultDuplicateWebhook, Event: "widgets.created", Probability: 1})
	do(t, e, "POST", "/widgets", `{"name":"a"}`, nil)
	got := e.Deliveries("widgets.created")
	if len(got) != 2 {
		t.Fatalf("duplicate fault must emit twice, got %d", len(got))
	}

	if got[0].ID != got[1].ID || string(got[0].Payload) != string(got[1].Payload) || got[0].Signature != got[1].Signature {
		t.Fatal("duplicate must be the SAME delivery replayed")
	}
	if got[0].Seq == got[1].Seq {
		t.Fatal("replay must advance the log seq")
	}

	do(t, e, "POST", "/widgets", `{"name":"b"}`, nil)
	if got := e.Deliveries("widgets.created"); len(got) != 3 {
		t.Fatalf("duplicate fault must be one-shot, got %d", len(got))
	}
	if len(e.Faults()) != 0 {
		t.Fatal("consumed webhook fault must be removed")
	}
}

func TestWebhookFaultDrop(t *testing.T) {
	e := newEngine(t, loadWebhookWidgets(t), Config{ID: "sbx_drop", Seed: "s"})
	e.ArmFault(FaultRule{Kind: FaultDropWebhook, Event: "widgets.created", Probability: 1})
	do(t, e, "POST", "/widgets", `{"name":"a"}`, nil)
	if got := e.Deliveries("widgets.created"); len(got) != 0 {
		t.Fatalf("drop fault must suppress the delivery, got %d", len(got))
	}
	do(t, e, "POST", "/widgets", `{"name":"b"}`, nil)
	if got := e.Deliveries("widgets.created"); len(got) != 1 {
		t.Fatalf("drop fault must be one-shot, got %d", len(got))
	}

	e.ArmFault(FaultRule{Kind: FaultDropWebhook, Event: "widgets.updated", Probability: 1})
	do(t, e, "POST", "/widgets", `{"name":"c"}`, nil)
	if got := e.Deliveries("widgets.created"); len(got) != 2 {
		t.Fatalf("a drop armed on another event must not fire, got %d", len(got))
	}
}

func TestWebhookFaultReorder(t *testing.T) {
	e := newEngine(t, loadWebhookWidgets(t), Config{ID: "sbx_re", Seed: "s"})
	e.ArmFault(FaultRule{Kind: FaultReorderWebhook, Event: "widgets.created", Probability: 1})
	do(t, e, "POST", "/widgets", `{"name":"a"}`, nil)

	if got := e.Deliveries("widgets.created"); len(got) != 0 {
		t.Fatalf("reorder must hold the first delivery, got %d", len(got))
	}
	do(t, e, "POST", "/widgets", `{"name":"b"}`, nil)
	got := e.Deliveries("widgets.created")
	if len(got) != 2 {
		t.Fatalf("reorder must release both, got %d", len(got))
	}

	if !(got[0].Seq > got[1].Seq) {
		t.Fatalf("reorder must swap delivery order, got seqs %d then %d", got[0].Seq, got[1].Seq)
	}
	var first, second map[string]any
	json.Unmarshal(got[0].Payload, &first)
	json.Unmarshal(got[1].Payload, &second)
	if first["id"] != "widgets_2" || second["id"] != "widgets_1" {
		t.Fatalf("swapped payloads wrong: %v then %v", first, second)
	}

	do(t, e, "POST", "/widgets", `{"name":"c"}`, nil)
	if got := e.Deliveries("widgets.created"); len(got) != 3 {
		t.Fatalf("reorder must be consumed after two, got %d", len(got))
	}
}

func TestWebhookLogBounded(t *testing.T) {
	e := newEngine(t, loadWebhookWidgets(t), Config{ID: "sbx_cap", Seed: "s"})
	for i := 0; i < 1005; i++ {
		if r := do(t, e, "POST", "/widgets", `{"name":"x"}`, nil); r.status != 201 {
			t.Fatalf("create %d: %d", i, r.status)
		}
	}
	got := e.Deliveries("")
	if len(got) != 1000 {
		t.Fatalf("log must cap at 1000, got %d", len(got))
	}
	if got[0].Seq != 6 || got[len(got)-1].Seq != 1005 {
		t.Fatalf("must drop OLDEST: first seq %d, last %d", got[0].Seq, got[len(got)-1].Seq)
	}
}

func TestWebhookSinkDelivers(t *testing.T) {
	type hit struct {
		headers http.Header
		body    []byte
	}
	var mu sync.Mutex
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		mu.Lock()
		hits = append(hits, hit{headers: r.Header.Clone(), body: body})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	e := newEngine(t, loadWebhookWidgets(t), Config{ID: "sbx_sink", Seed: "s", WebhookURL: srv.URL})
	do(t, e, "POST", "/widgets", `{"name":"a"}`, nil)

	deadline := time.Now().Add(5 * time.Second)
	for e.WebhookSinkStats().Delivered < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("sink never received the delivery: %+v", e.WebhookSinkStats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	d := e.Deliveries("widgets.created")[0]
	h := hits[0]
	if string(h.body) != string(d.Payload) {
		t.Fatalf("sink body must be the signed envelope:\n%s\nvs\n%s", h.body, d.Payload)
	}
	if h.headers.Get("x-pikopod-webhook-id") != d.ID ||
		h.headers.Get("x-pikopod-webhook-event") != "widgets.created" ||
		h.headers.Get("x-pikopod-webhook-timestamp") != fmt.Sprint(d.VirtualTimeMs/1000) ||
		h.headers.Get("x-pikopod-webhook-signature") != "sha256="+d.Signature {
		t.Fatalf("signature headers wrong: %+v", h.headers)
	}
	if h.headers.Get("content-type") != "application/json" {
		t.Fatalf("content-type wrong: %s", h.headers.Get("content-type"))
	}
}

func TestWebhookSinkWedgedNeverBlocks(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	e := newEngine(t, loadWebhookWidgets(t), Config{ID: "sbx_wedge", Seed: "s", WebhookURL: srv.URL})
	start := time.Now()
	const n = 400
	for i := 0; i < n; i++ {
		if r := do(t, e, "POST", "/widgets", `{"name":"x"}`, nil); r.status != 201 {
			t.Fatalf("create %d blocked or failed: %d", i, r.status)
		}
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("data plane blocked behind the sink: %v for %d creates", elapsed, n)
	}
	if got := e.Deliveries("widgets.created"); len(got) != n {
		t.Fatalf("outbox log must be unaffected by the sink, got %d", len(got))
	}
	if e.WebhookSinkStats().Dropped == 0 {
		t.Fatal("overflow past the sink queue must be counted as dropped")
	}
}
