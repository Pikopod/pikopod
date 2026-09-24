package sandbox

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

func loadAppveyor(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/parity/importer/specs/appveyor-swagger.json")
	if err != nil {
		t.Fatalf("read appveyor spec: %v", err)
	}
	def, err := importer.NormalizeOpenAPI(raw)
	if err != nil {
		t.Fatalf("normalize appveyor spec: %v", err)
	}
	return def
}

const widgetsSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Widgets", "version": "1.0.0"},
  "paths": {
    "/widgets": {
      "get": {
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {
            "items": {"type": "array", "items": {"$ref": "#/components/schemas/Widget"}},
            "total": {"type": "integer"},
            "generatedAt": {"type": "string", "format": "date-time"}
          }
        }}}}}
      },
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"201": {"description": "created", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      }
    },
    "/widgets/{widgetId}": {
      "parameters": [{"name": "widgetId", "in": "path", "required": true, "schema": {"type": "string"}}],
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}},
      "put": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      },
      "patch": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}},
        "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      },
      "delete": {"responses": {"204": {"description": "gone"}}}
    },
    "/gadgets": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {
          "type": "object",
          "required": ["name"],
          "properties": {"name": {"type": "string"}, "size": {"type": "integer"}}
        }}}},
        "responses": {"201": {"description": "created", "content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"id": {"type": "string"}, "name": {"type": "string"}}
        }}}}}
      }
    }
  },
  "components": {"schemas": {"Widget": {
    "type": "object",
    "properties": {
      "id": {"type": "string"},
      "name": {"type": "string"},
      "createdAt": {"type": "string", "format": "date-time"}
    }
  }}}
}`

func loadWidgets(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(widgetsSpec))
	if err != nil {
		t.Fatalf("normalize widgets spec: %v", err)
	}
	return def
}

func newEngine(t *testing.T, def *ir.ApiDefinition, cfg Config) *Engine {
	t.Helper()
	store, err := OpenMemoryStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	e, err := NewEngine(def, cfg, store)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

type recorded struct {
	status  int
	headers map[string]string
	body    string
}

func do(t *testing.T, e *Engine, method, path, body string, headers map[string]string) recorded {
	t.Helper()
	var req = httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	out := recorded{status: w.Code, headers: map[string]string{}, body: w.Body.String()}
	for k := range w.Header() {
		out.headers[strings.ToLower(k)] = w.Header().Get(k)
	}
	return out
}

func TestDeterminismAcrossEngines(t *testing.T) {
	def := loadAppveyor(t)
	cfg := Config{ID: "sbx_det", Seed: "seed-alpha", Mode: "deterministic", VirtualClockMs: SandboxBaseEpochMs}
	e1 := newEngine(t, def, cfg)
	e2 := newEngine(t, def, cfg)
	auth := map[string]string{"Authorization": e1.Credential()}
	if e1.Credential() != e2.Credential() {
		t.Fatalf("credentials diverge: %q vs %q", e1.Credential(), e2.Credential())
	}

	steps := []struct {
		name, method, path, body string
	}{
		{"create", "POST", "/roles", `{"name":"QA"}`},
		{"read", "GET", "/roles/roles_1", ""},
		{"list", "GET", "/roles", ""},
		{"read-your-write", "GET", "/roles/roles_1", ""},
		{"synthesized-error", "GET", "/roles/missing_9", ""},
	}
	var createBody, readBody string
	for _, s := range steps {
		r1 := do(t, e1, s.method, s.path, s.body, auth)
		r2 := do(t, e2, s.method, s.path, s.body, auth)
		if r1.status != r2.status || r1.body != r2.body {
			t.Fatalf("%s: engines diverge:\n  e1 %d %s\n  e2 %d %s", s.name, r1.status, r1.body, r2.status, r2.body)
		}
		if len(r1.headers) != len(r2.headers) {
			t.Fatalf("%s: header sets diverge: %v vs %v", s.name, r1.headers, r2.headers)
		}
		for k, v := range r1.headers {
			if k == "date" {
				continue
			}
			if r2.headers[k] != v {
				t.Fatalf("%s: header %q diverges: %q vs %q", s.name, k, v, r2.headers[k])
			}
		}
		switch s.name {
		case "create":
			createBody = r1.body
			if r1.status != 200 {
				t.Fatalf("create status = %d, want 200: %s", r1.status, r1.body)
			}
		case "read-your-write":
			readBody = r1.body
		case "synthesized-error":
			if r1.status != 404 {
				t.Fatalf("missing id status = %d, want 404", r1.status)
			}

			if r1.body == `{"message":"Not Found"}` || r1.body == "" {
				t.Fatalf("expected a synthesized declared-error body, got %q", r1.body)
			}
		}
	}

	if createBody != readBody {
		t.Fatalf("read-your-write mismatch:\n  create: %s\n  read:   %s", createBody, readBody)
	}

	e3 := newEngine(t, def, Config{ID: "sbx_det", Seed: "seed-beta", VirtualClockMs: SandboxBaseEpochMs})
	r3 := do(t, e3, "GET", "/roles/missing_9", "", map[string]string{"Authorization": e3.Credential()})
	r1 := do(t, e1, "GET", "/roles/missing_9", "", auth)
	if r3.body == r1.body {
		t.Fatalf("different seeds produced identical synthesized error bodies: %s", r1.body)
	}
}

func TestStatefulCRUD(t *testing.T) {
	def := loadWidgets(t)
	e := newEngine(t, def, Config{ID: "sbx_state", Seed: "s1"})

	create := do(t, e, "POST", "/widgets", `{"name":"Anvil"}`, nil)
	if create.status != 201 {
		t.Fatalf("create status = %d body=%s", create.status, create.body)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(create.body), &created); err != nil {
		t.Fatalf("create body not JSON: %v", err)
	}
	if created["id"] != "widgets_1" {
		t.Fatalf("id = %v, want widgets_1", created["id"])
	}
	if created["name"] != "Anvil" {
		t.Fatalf("name = %v, want the client's value (read-your-write)", created["name"])
	}
	if created["createdAt"] != "2025-01-01T00:00:00.000Z" {
		t.Fatalf("createdAt = %v, want the virtual clock ISO instant", created["createdAt"])
	}
	if create.headers["etag"] != `"0"` {
		t.Fatalf("etag = %q, want \"0\"", create.headers["etag"])
	}

	read := do(t, e, "GET", "/widgets/widgets_1", "", nil)
	if read.status != 200 || read.body != create.body {
		t.Fatalf("read = %d %s, want the created body %s", read.status, read.body, create.body)
	}

	list := do(t, e, "GET", "/widgets", "", nil)
	if list.status != 200 {
		t.Fatalf("list status = %d", list.status)
	}
	var envelope struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(list.body), &envelope); err != nil {
		t.Fatalf("list body not the declared envelope: %v (%s)", err, list.body)
	}
	if envelope.Total != 1 || len(envelope.Items) != 1 || envelope.Items[0]["id"] != "widgets_1" {
		t.Fatalf("list envelope wrong: %s", list.body)
	}

	put := do(t, e, "PUT", "/widgets/widgets_1", `{"name":"Anvil II"}`, nil)
	if put.status != 200 || put.headers["etag"] != `"1"` {
		t.Fatalf("put = %d etag=%q body=%s, want 200 etag \"1\"", put.status, put.headers["etag"], put.body)
	}
	var updated map[string]any
	json.Unmarshal([]byte(put.body), &updated)
	if updated["name"] != "Anvil II" || updated["id"] != "widgets_1" {
		t.Fatalf("put body wrong: %s", put.body)
	}

	stale := do(t, e, "PUT", "/widgets/widgets_1", `{"name":"X"}`, map[string]string{"If-Match": `"0"`})
	if stale.status != 412 {
		t.Fatalf("stale If-Match status = %d, want 412", stale.status)
	}

	patch := do(t, e, "PATCH", "/widgets/widgets_1", `{"name":"Anvil III"}`, nil)
	if patch.status != 200 || patch.headers["etag"] != `"2"` {
		t.Fatalf("patch = %d etag=%q, want 200 etag \"2\"", patch.status, patch.headers["etag"])
	}

	del := do(t, e, "DELETE", "/widgets/widgets_1", "", nil)
	if del.status != 204 || del.body != "" {
		t.Fatalf("delete = %d body=%q, want bare 204", del.status, del.body)
	}
	gone := do(t, e, "GET", "/widgets/widgets_1", "", nil)
	if gone.status != 404 || gone.body != `{"message":"Not Found"}` {
		t.Fatalf("after delete: %d %s, want the neutral 404 envelope", gone.status, gone.body)
	}
}

func TestAuthEnforcement(t *testing.T) {
	def := loadAppveyor(t)
	e := newEngine(t, def, Config{ID: "sbx_auth", Seed: "auth-seed"})

	missing := do(t, e, "GET", "/roles", "", nil)
	if missing.status != 401 || missing.body != `{"message":"Authentication required"}` {
		t.Fatalf("missing cred = %d %s", missing.status, missing.body)
	}

	wrong := do(t, e, "GET", "/roles", "", map[string]string{"Authorization": "nope"})
	if wrong.status != 401 || wrong.body != `{"message":"Unauthorized"}` {
		t.Fatalf("wrong cred = %d %s", wrong.status, wrong.body)
	}

	real := do(t, e, "GET", "/roles", "", map[string]string{"Authorization": "xpay_secret_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6"})
	if real.status != 403 || !strings.Contains(real.body, "issued test credentials") {
		t.Fatalf("real-looking cred = %d %s, want the loud 403", real.status, real.body)
	}

	ok := do(t, e, "GET", "/roles", "", map[string]string{"Authorization": e.Credential()})
	if ok.status != 200 {
		t.Fatalf("issued cred = %d %s, want 200", ok.status, ok.body)
	}
	if !strings.HasPrefix(e.Credential(), SandboxCredentialPrefix) {
		t.Fatalf("credential %q must carry the issued prefix", e.Credential())
	}
}

func TestRouteSemantics(t *testing.T) {
	def := loadAppveyor(t)
	e := newEngine(t, def, Config{ID: "sbx_routes", Seed: "r"})
	auth := map[string]string{"Authorization": e.Credential()}

	nf := do(t, e, "GET", "/definitely-not-a-path", "", nil)
	if nf.status != 404 || nf.body != `{"message":"Not Found"}` {
		t.Fatalf("unknown path = %d %s", nf.status, nf.body)
	}

	mna := do(t, e, "DELETE", "/roles", "", auth)
	if mna.status != 405 || mna.body != `{"message":"Method Not Allowed"}` {
		t.Fatalf("method not allowed = %d %s", mna.status, mna.body)
	}
	if mna.headers["allow"] != "GET, POST, PUT" {
		t.Fatalf("allow = %q, want \"GET, POST, PUT\"", mna.headers["allow"])
	}
}

func TestPureSynthesisEmptyList(t *testing.T) {
	def := loadWidgets(t)
	e := newEngine(t, def, Config{ID: "sbx_synth", Seed: "s"})

	list := do(t, e, "GET", "/widgets", "", nil)
	if list.status != 200 {
		t.Fatalf("status = %d", list.status)
	}
	var envelope struct {
		Items       []any  `json:"items"`
		Total       int    `json:"total"`
		GeneratedAt string `json:"generatedAt"`
	}
	if err := json.Unmarshal([]byte(list.body), &envelope); err != nil {
		t.Fatalf("body is not the declared envelope: %v (%s)", err, list.body)
	}
	if envelope.Items == nil || len(envelope.Items) != 0 {
		t.Fatalf("items = %v, want an empty array", envelope.Items)
	}
	if envelope.Total != 0 {
		t.Fatalf("total = %d, want the item count 0", envelope.Total)
	}
	if envelope.GeneratedAt != "2025-01-01T00:00:00.000Z" {
		t.Fatalf("generatedAt = %q, want the virtual-clock instant", envelope.GeneratedAt)
	}
}

func TestBodyHandling(t *testing.T) {
	def := loadWidgets(t)
	e := newEngine(t, def, Config{ID: "sbx_body", Seed: "b"})

	invalid := do(t, e, "POST", "/gadgets", `{}`, nil)
	if invalid.status != 400 || invalid.body != `{"message":"Validation failed","errors":["name is required"]}` {
		t.Fatalf("missing required = %d %s", invalid.status, invalid.body)
	}
	wrongType := do(t, e, "POST", "/gadgets", `{"name":"g","size":"large"}`, nil)
	if wrongType.status != 400 || wrongType.body != `{"message":"Validation failed","errors":["size must be integer"]}` {
		t.Fatalf("wrong type = %d %s", wrongType.status, wrongType.body)
	}

	malformed := do(t, e, "POST", "/gadgets", `{"name":`, nil)
	if malformed.status != 400 || malformed.body != `{"message":"Malformed JSON body"}` {
		t.Fatalf("malformed = %d %s", malformed.status, malformed.body)
	}

	array := do(t, e, "POST", "/gadgets", `[1,2]`, nil)
	if array.status != 400 || array.body != `{"message":"Request body must be a JSON object"}` {
		t.Fatalf("array body = %d %s", array.status, array.body)
	}
}

func TestIdempotencyReplay(t *testing.T) {
	def := loadWidgets(t)
	e := newEngine(t, def, Config{ID: "sbx_idem", Seed: "i"})
	hdr := map[string]string{"Idempotency-Key": "abc"}

	first := do(t, e, "POST", "/widgets", `{"name":"One"}`, hdr)
	if first.status != 201 {
		t.Fatalf("first = %d", first.status)
	}
	replay := do(t, e, "POST", "/widgets", `{"name":"One"}`, hdr)
	if replay.status != 201 || replay.body != first.body {
		t.Fatalf("replay = %d %s, want the original response", replay.status, replay.body)
	}
	if replay.headers["idempotent-replayed"] != "true" {
		t.Fatalf("replay missing the idempotent-replayed marker: %v", replay.headers)
	}
	reused := do(t, e, "POST", "/widgets", `{"name":"Two"}`, hdr)
	if reused.status != 422 {
		t.Fatalf("reused key with different request = %d, want 422", reused.status)
	}

	list := do(t, e, "GET", "/widgets", "", nil)
	var envelope struct {
		Total int `json:"total"`
	}
	json.Unmarshal([]byte(list.body), &envelope)
	if envelope.Total != 1 {
		t.Fatalf("total = %d, want 1 (no duplicate create)", envelope.Total)
	}
}

func TestListPagination(t *testing.T) {
	def := loadWidgets(t)
	e := newEngine(t, def, Config{ID: "sbx_page", Seed: "p"})
	for i := 0; i < 5; i++ {
		r := do(t, e, "POST", "/widgets", `{"name":"W"}`, nil)
		if r.status != 201 {
			t.Fatalf("seed create %d = %d", i, r.status)
		}
	}
	page1 := do(t, e, "GET", "/widgets?limit=2", "", nil)
	link := page1.headers["link"]
	if link == "" || !strings.Contains(link, `rel="next"`) {
		t.Fatalf("page 1 missing the Link next cursor: %v", page1.headers)
	}

	url := link[strings.Index(link, "<")+1 : strings.Index(link, ">")]
	page2 := do(t, e, "GET", url, "", nil)
	if page2.status != 200 {
		t.Fatalf("page 2 = %d", page2.status)
	}
	var env1, env2 struct {
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal([]byte(page1.body), &env1)
	json.Unmarshal([]byte(page2.body), &env2)
	if len(env1.Items) != 2 || len(env2.Items) != 2 {
		t.Fatalf("page sizes = %d, %d; want 2, 2", len(env1.Items), len(env2.Items))
	}
	if env1.Items[1]["id"] == env2.Items[0]["id"] {
		t.Fatalf("cursor pagination repeated a resource: %v", env2.Items[0]["id"])
	}
}
