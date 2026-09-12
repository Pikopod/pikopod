package importer

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
)

// A realistic schema exercising: custom root names via a schema block, enum,
// interface, union, custom scalar, input types, lists, non-null wrappers,
// descriptions, defaults, a forward reference (UserFilter used before
// defined), a subscription, and the User→posts→Post→author→User cycle.
const testSDL = `"""
Acme social graph.
"""
schema {
  query: Query
  mutation: Mutation
  subscription: Subscription
}

scalar DateTime

enum Role {
  ADMIN
  MEMBER
  GUEST
}

interface Node {
  id: ID!
}

type User implements Node {
  id: ID!
  name: String!
  email: String
  role: Role!
  score: Float
  active: Boolean!
  posts(limit: Int = 10): [Post!]!
  createdAt: DateTime
}

type Post implements Node {
  id: ID!
  title: String!
  author: User!
  tags: [String!]
}

union SearchResult = User | Post

input CreateUserInput {
  name: String!
  email: String
  role: Role = MEMBER
}

type Query {
  "Fetch a single user by id."
  user(id: ID!): User
  users(limit: Int, role: Role, filter: UserFilter): [User!]!
  search(term: String!): [SearchResult!]
  node(id: ID!): Node
}

input UserFilter {
  nameContains: String
}

type Mutation {
  createUser(input: CreateUserInput!): User!
  deleteUser(id: ID!, hard: Boolean): Boolean!
}

type Subscription {
  userCreated: User!
}
`

func graphqlIR(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := NormalizeGraphQLSDL([]byte(testSDL))
	if err != nil {
		t.Fatalf("normalize SDL: %v", err)
	}
	return def
}

func endpointByOp(t *testing.T, def *ir.ApiDefinition, opID string) *ir.Endpoint {
	t.Helper()
	for i := range def.Endpoints {
		if def.Endpoints[i].OperationID != nil && def.Endpoints[i].OperationID.Value == opID {
			return &def.Endpoints[i]
		}
	}
	t.Fatalf("no endpoint with operationId %q", opID)
	return nil
}

func namedSchema(t *testing.T, def *ir.ApiDefinition, name string) *ir.NamedSchema {
	t.Helper()
	for i := range def.Schemas {
		if def.Schemas[i].Name == name {
			return &def.Schemas[i]
		}
	}
	t.Fatalf("no named schema %q", name)
	return nil
}

func propOf(t *testing.T, node *ir.IrSchemaNode, name string) *ir.PropertySchema {
	t.Helper()
	for i := range node.Properties {
		if node.Properties[i].Name == name {
			return &node.Properties[i]
		}
	}
	t.Fatalf("no property %q", name)
	return nil
}

func TestGraphQLSDLConversion(t *testing.T) {
	def := graphqlIR(t)

	if def.SourceKind != "graphql" {
		t.Errorf("sourceKind = %q, want graphql", def.SourceKind)
	}
	want := ir.NormalizerVersion + "+pikopod-graphql@1"
	if def.NormalizerVersion != want {
		t.Errorf("normalizerVersion = %q, want %q", def.NormalizerVersion, want)
	}
	if len(def.AuthSchemes) != 0 {
		t.Errorf("GraphQL must not invent auth schemes, got %d", len(def.AuthSchemes))
	}

	// Query field → GET on a synthetic collection path, scalar arg as a
	// required query parameter.
	user := endpointByOp(t, def, "query.user")
	if user.Method.Value != "GET" || user.PathTemplate.Value != "/graphql/query/user" {
		t.Errorf("query.user = %s %s", user.Method.Value, user.PathTemplate.Value)
	}
	if len(user.Parameters) != 1 {
		t.Fatalf("query.user parameters = %d, want 1", len(user.Parameters))
	}
	p := user.Parameters[0]
	if p.Name != "id" || p.Location != "query" || !p.Required.Value {
		t.Errorf("query.user id param = %+v", p)
	}
	if user.Summary == nil || user.Summary.Value != "Fetch a single user by id." {
		t.Errorf("query.user summary lost: %+v", user.Summary)
	}
	// 200 response is a $ref to the User component.
	if len(user.Responses) != 1 || user.Responses[0].StatusCode != "200" {
		t.Fatalf("query.user responses = %+v", user.Responses)
	}
	respSchema := user.Responses[0].Content[0].Schema
	if respSchema.Ref == nil || *respSchema.Ref != ir.NamedSchemaID("User") {
		t.Errorf("query.user 200 schema ref = %v", respSchema.Ref)
	}

	// Non-scalar args are skipped as parameters and noted in the description.
	users := endpointByOp(t, def, "query.users")
	names := map[string]bool{}
	for _, p := range users.Parameters {
		names[p.Name] = true
	}
	if !names["limit"] || !names["role"] || names["filter"] {
		t.Errorf("query.users params = %v (want limit+role, no filter)", names)
	}
	if users.Description == nil || !strings.Contains(users.Description.Value, "filter: UserFilter") {
		t.Errorf("query.users description must note the skipped arg: %+v", users.Description)
	}
	// The list return type survives: array of $ref User.
	usersResp := users.Responses[0].Content[0].Schema
	if usersResp.Type.Value != "array" || usersResp.Items == nil || usersResp.Items.Ref == nil || *usersResp.Items.Ref != ir.NamedSchemaID("User") {
		t.Errorf("query.users 200 schema = %+v", usersResp)
	}

	// Mutation with a single input-object arg → POST whose body IS the input
	// type; requiredness comes from the input's non-null fields.
	create := endpointByOp(t, def, "mutation.createUser")
	if create.Method.Value != "POST" || create.PathTemplate.Value != "/graphql/mutation/createUser" {
		t.Errorf("mutation.createUser = %s %s", create.Method.Value, create.PathTemplate.Value)
	}
	if create.RequestBody == nil || !create.RequestBody.Required.Value {
		t.Fatalf("mutation.createUser requestBody missing or optional")
	}
	bodySchema := create.RequestBody.Content[0].Schema
	if bodySchema.Ref == nil || *bodySchema.Ref != ir.NamedSchemaID("CreateUserInput") {
		t.Errorf("createUser body schema ref = %v", bodySchema.Ref)
	}
	input := namedSchema(t, def, "CreateUserInput")
	if !propOf(t, &input.Schema, "name").Required.Value {
		t.Error("CreateUserInput.name must be required (non-null)")
	}
	if propOf(t, &input.Schema, "email").Required.Value {
		t.Error("CreateUserInput.email must not be required (nullable)")
	}
	if propOf(t, &input.Schema, "role").Required.Value {
		t.Error("CreateUserInput.role must not be required (nullable with default)")
	}

	// Multi-arg mutation → args-object body, non-null args required.
	del := endpointByOp(t, def, "mutation.deleteUser")
	delSchema := del.RequestBody.Content[0].Schema
	if delSchema.Type.Value != "object" {
		t.Fatalf("deleteUser body schema = %+v", delSchema)
	}
	if !propOf(t, &delSchema, "id").Required.Value {
		t.Error("deleteUser.id must be required")
	}
	if propOf(t, &delSchema, "hard").Required.Value {
		t.Error("deleteUser.hard must not be required")
	}

	// Enum values survive, in declaration order.
	role := namedSchema(t, def, "Role")
	if role.Schema.EnumValues == nil {
		t.Fatal("Role enum values lost")
	}
	got := role.Schema.EnumValues.Value
	wantVals := []any{"ADMIN", "MEMBER", "GUEST"}
	if len(got) != len(wantVals) {
		t.Fatalf("Role enum = %v", got)
	}
	for i := range wantVals {
		if got[i] != wantVals[i] {
			t.Errorf("Role enum[%d] = %v, want %v", i, got[i], wantVals[i])
		}
	}

	// The User→posts→Post→author→User cycle terminates via $refs.
	userSchema := namedSchema(t, def, "User")
	posts := propOf(t, &userSchema.Schema, "posts")
	if posts.Schema.Type.Value != "array" || posts.Schema.Items == nil || posts.Schema.Items.Ref == nil || *posts.Schema.Items.Ref != ir.NamedSchemaID("Post") {
		t.Errorf("User.posts = %+v", posts.Schema)
	}
	post := namedSchema(t, def, "Post")
	author := propOf(t, &post.Schema, "author")
	if author.Schema.Ref == nil || *author.Schema.Ref != ir.NamedSchemaID("User") {
		t.Errorf("Post.author = %+v", author.Schema)
	}

	// Union → oneOf composition; custom scalar → string with format.
	search := namedSchema(t, def, "SearchResult")
	if search.Schema.Composition == nil || search.Schema.Composition.Kind != "oneOf" || len(search.Schema.Composition.Members) != 2 {
		t.Errorf("SearchResult = %+v", search.Schema.Composition)
	}
	dt := namedSchema(t, def, "DateTime")
	if dt.Schema.Type.Value != "string" || dt.Schema.Format == nil || dt.Schema.Format.Value != "DateTime" {
		t.Errorf("DateTime scalar = %+v", dt.Schema)
	}

	// Catch-all POST /graphql with a {query, variables} body, query required.
	execute := endpointByOp(t, def, "graphql.execute")
	if execute.Method.Value != "POST" || execute.PathTemplate.Value != "/graphql" {
		t.Errorf("graphql.execute = %s %s", execute.Method.Value, execute.PathTemplate.Value)
	}
	exBody := execute.RequestBody.Content[0].Schema
	if !propOf(t, &exBody, "query").Required.Value {
		t.Error("catch-all body query field must be required")
	}

	// Subscription → webhook carrying the return type.
	if len(def.Webhooks) != 1 || def.Webhooks[0].Event.Value != "userCreated" {
		t.Fatalf("webhooks = %+v", def.Webhooks)
	}
	if def.Webhooks[0].PayloadSchema == nil || def.Webhooks[0].PayloadSchema.Ref == nil || *def.Webhooks[0].PayloadSchema.Ref != ir.NamedSchemaID("User") {
		t.Errorf("userCreated payload = %+v", def.Webhooks[0].PayloadSchema)
	}
}

func TestGraphQLIntrospectionConversion(t *testing.T) {
	introspection := `{"data": {"__schema": {
	  "queryType": {"name": "Query"},
	  "mutationType": {"name": "Mutation"},
	  "types": [
	    {"kind": "OBJECT", "name": "Query", "fields": [
	      {"name": "user", "description": "Fetch a user.",
	       "args": [{"name": "id", "type": {"kind": "NON_NULL", "name": null, "ofType": {"kind": "SCALAR", "name": "ID"}}}],
	       "type": {"kind": "OBJECT", "name": "User"}}
	    ]},
	    {"kind": "OBJECT", "name": "Mutation", "fields": [
	      {"name": "createUser",
	       "args": [{"name": "input", "type": {"kind": "NON_NULL", "ofType": {"kind": "INPUT_OBJECT", "name": "CreateUserInput"}}}],
	       "type": {"kind": "NON_NULL", "ofType": {"kind": "OBJECT", "name": "User"}}}
	    ]},
	    {"kind": "OBJECT", "name": "User", "fields": [
	      {"name": "id", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "ID"}}},
	      {"name": "role", "type": {"kind": "ENUM", "name": "Role"}}
	    ]},
	    {"kind": "ENUM", "name": "Role", "enumValues": [{"name": "ADMIN"}, {"name": "MEMBER"}]},
	    {"kind": "INPUT_OBJECT", "name": "CreateUserInput", "inputFields": [
	      {"name": "name", "type": {"kind": "NON_NULL", "ofType": {"kind": "SCALAR", "name": "String"}}},
	      {"name": "email", "type": {"kind": "SCALAR", "name": "String"}}
	    ]},
	    {"kind": "SCALAR", "name": "String"},
	    {"kind": "SCALAR", "name": "ID"},
	    {"kind": "OBJECT", "name": "__Schema", "fields": []}
	  ]
	}}}`

	kind, err := Detect([]byte(introspection))
	if err != nil || kind != KindGraphQLIntrospection {
		t.Fatalf("Detect = %q, %v", kind, err)
	}
	def, err := NormalizeOpenAPI([]byte(introspection))
	if err != nil {
		t.Fatalf("normalize introspection: %v", err)
	}

	want := ir.NormalizerVersion + "+pikopod-graphql@1"
	if def.NormalizerVersion != want {
		t.Errorf("normalizerVersion = %q", def.NormalizerVersion)
	}
	user := endpointByOp(t, def, "query.user")
	if user.Method.Value != "GET" || user.PathTemplate.Value != "/graphql/query/user" {
		t.Errorf("query.user = %s %s", user.Method.Value, user.PathTemplate.Value)
	}
	if len(user.Parameters) != 1 || user.Parameters[0].Name != "id" || !user.Parameters[0].Required.Value {
		t.Errorf("query.user params = %+v", user.Parameters)
	}
	create := endpointByOp(t, def, "mutation.createUser")
	bodySchema := create.RequestBody.Content[0].Schema
	if bodySchema.Ref == nil || *bodySchema.Ref != ir.NamedSchemaID("CreateUserInput") {
		t.Errorf("createUser body ref = %v", bodySchema.Ref)
	}
	input := namedSchema(t, def, "CreateUserInput")
	if !propOf(t, &input.Schema, "name").Required.Value || propOf(t, &input.Schema, "email").Required.Value {
		t.Error("CreateUserInput requiredness from non-null fields lost")
	}
	role := namedSchema(t, def, "Role")
	if role.Schema.EnumValues == nil || len(role.Schema.EnumValues.Value) != 2 {
		t.Errorf("Role enum = %+v", role.Schema.EnumValues)
	}
	// Builtin scalars and __-prefixed introspection types never become components.
	for i := range def.Schemas {
		if def.Schemas[i].Name == "String" || def.Schemas[i].Name == "ID" || strings.HasPrefix(def.Schemas[i].Name, "__") {
			t.Errorf("unexpected component %q", def.Schemas[i].Name)
		}
	}
}

func TestGraphQLSDLMalformed(t *testing.T) {
	_, err := NormalizeGraphQLSDL([]byte("type User {\n  name: [String\n}\n"))
	if err == nil {
		t.Fatal("malformed SDL must be rejected")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error must carry line info, got: %v", err)
	}
	if !strings.Contains(err.Error(), "SPEC_PARSE_ERROR") {
		t.Errorf("error must carry the spec code, got: %v", err)
	}
}

func TestGraphQLSDLOversized(t *testing.T) {
	big := strings.Repeat("# padding\n", (graphqlMaxBytes/10)+1)
	_, err := NormalizeGraphQLSDL([]byte(big))
	if err == nil || !strings.Contains(err.Error(), "SPEC_TOO_LARGE") {
		t.Errorf("oversized SDL must be refused with SPEC_TOO_LARGE, got: %v", err)
	}
}

func TestGraphQLSDLTooManyTypes(t *testing.T) {
	var b strings.Builder
	b.WriteString("type Query { ok: Boolean }\n")
	for i := 0; i <= graphqlMaxTypes; i++ {
		b.WriteString("scalar S")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	_, err := NormalizeGraphQLSDL([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "SPEC_TOO_LARGE") {
		t.Errorf("type-count bomb must be refused with SPEC_TOO_LARGE, got: %v", err)
	}
}

// End-to-end: SDL bytes → Detect → IR → the UNCHANGED sandbox engine serves
// the per-field operations deterministically.
func TestGraphQLEndToEndSandbox(t *testing.T) {
	kind, err := Detect([]byte(testSDL))
	if err != nil || kind != KindGraphQLSDL {
		t.Fatalf("Detect = %q, %v", kind, err)
	}
	def, err := NormalizeOpenAPI([]byte(testSDL))
	if err != nil {
		t.Fatalf("NormalizeOpenAPI over SDL: %v", err)
	}

	newEngine := func(seed string) *sandbox.Engine {
		store, err := sandbox.OpenMemoryStore()
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		eng, err := sandbox.NewEngine(def, sandbox.Config{ID: "sbx_graphql", Seed: seed}, store)
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		return eng
	}

	get := func(eng *sandbox.Engine, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		eng.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	a, b := newEngine("seed-1"), newEngine("seed-1")

	// GET on a query op: deterministic 200 shaped by the User schema.
	ra := get(a, "/graphql/query/user?id=u_1")
	rb := get(b, "/graphql/query/user?id=u_1")
	if ra.Code != 200 || rb.Code != 200 {
		t.Fatalf("query.user status = %d / %d, body=%s", ra.Code, rb.Code, ra.Body.String())
	}
	if ra.Body.String() != rb.Body.String() {
		t.Errorf("same-seed engines diverged:\n%s\n%s", ra.Body.String(), rb.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(ra.Body.Bytes(), &body); err != nil {
		t.Fatalf("query.user body is not JSON: %v", err)
	}
	for _, key := range []string{"id", "name", "role", "posts"} {
		if _, ok := body[key]; !ok {
			t.Errorf("query.user body missing schema key %q: %s", key, ra.Body.String())
		}
	}
	if posts, ok := body["posts"].([]any); !ok || len(posts) != 0 {
		t.Errorf("posts must be an empty array on a fresh store: %v", body["posts"])
	}
	if role, ok := body["role"].(string); !ok || (role != "ADMIN" && role != "MEMBER" && role != "GUEST") {
		t.Errorf("role must synthesize from the enum, got %v", body["role"])
	}

	// The list-returning query op serves too.
	if rc := get(a, "/graphql/query/users"); rc.Code != 200 {
		t.Errorf("query.users status = %d", rc.Code)
	}

	// Mutation op: POST with the input-type body.
	post := func(eng *sandbox.Engine, path, payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		eng.ServeHTTP(rec, req)
		return rec
	}
	ma := post(a, "/graphql/mutation/createUser", `{"name":"Ada","email":"ada@example.test"}`)
	mb := post(b, "/graphql/mutation/createUser", `{"name":"Ada","email":"ada@example.test"}`)
	if ma.Code != 200 || ma.Body.String() != mb.Body.String() {
		t.Errorf("mutation.createUser = %d, deterministic=%v, body=%s", ma.Code, ma.Body.String() == mb.Body.String(), ma.Body.String())
	}

	// Catch-all POST /graphql: shaped answer for real GraphQL clients, and
	// the required `query` field is enforced.
	ca := post(a, "/graphql", `{"query":"{ user(id: \"u_1\") { id } }"}`)
	if ca.Code != 200 {
		t.Fatalf("catch-all status = %d, body=%s", ca.Code, ca.Body.String())
	}
	var caBody map[string]any
	if err := json.Unmarshal(ca.Body.Bytes(), &caBody); err != nil {
		t.Fatalf("catch-all body is not JSON: %v", err)
	}
	if _, ok := caBody["data"]; !ok {
		t.Errorf("catch-all body missing data key: %s", ca.Body.String())
	}
	if bad := post(a, "/graphql", `{}`); bad.Code != 400 {
		t.Errorf("catch-all without query must 400, got %d", bad.Code)
	}
}

// The scenario layer works unchanged: happy_path binds against the GraphQL
// IR (query ops are LIST-shaped), and no auth-dependent archetype binds
// because GraphQL SDL carries no auth to enforce.
func TestGraphQLArchetypesBind(t *testing.T) {
	def := graphqlIR(t)
	var happy, unauthorized *archetype.Archetype
	all := archetype.All()
	for i := range all {
		switch all[i].ID {
		case "happy_path":
			happy = &all[i]
		case "unauthorized":
			unauthorized = &all[i]
		}
	}
	if happy == nil || unauthorized == nil {
		t.Fatal("catalogue archetypes missing")
	}

	binding := archetype.Bind(happy, def)
	if !binding.Applicable || len(binding.Candidates) == 0 {
		t.Fatalf("happy_path must bind: %+v", binding)
	}
	found := false
	for _, c := range binding.Candidates {
		if strings.HasPrefix(c.Bindings["op"], "query.") {
			found = true
		}
	}
	if !found {
		t.Errorf("happy_path candidates must include a query op: %+v", binding.Candidates)
	}

	if b := archetype.Bind(unauthorized, def); b.Applicable {
		t.Errorf("unauthorized must NOT bind (no auth invented): %+v", b)
	}
}
