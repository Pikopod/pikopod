// Package ir is the golden-pinned IR contract. Collections are unordered sets
// keyed by stable ids; sourcePointer is debug-only and stripped before hashing.
package ir

// HttpMethod is one of GET POST PUT PATCH DELETE HEAD OPTIONS TRACE.
type HttpMethod = string

// ParameterLocation is one of path, query, header, cookie.
type ParameterLocation = string

// ScalarType is one of string number integer boolean object array null unknown.
type ScalarType = string

// IrSchemaNode is a normalized schema node: `nullable` is always a boolean
// flag and `$ref` is expanded or carried as a `ref` to a NamedSchema id.
type IrSchemaNode struct {
	ID            string             `json:"id"`
	Type          Prov[ScalarType]   `json:"type"`
	Nullable      Prov[bool]         `json:"nullable"`
	Format        *Prov[string]      `json:"format"`
	EnumValues    *Prov[[]any]       `json:"enumValues"`
	Constraints   []SchemaConstraint `json:"constraints"`
	Properties    []PropertySchema   `json:"properties"`
	Items         *IrSchemaNode      `json:"items"`
	Composition   *SchemaComposition `json:"composition"`
	Ref           *string            `json:"ref"`
	SourcePointer string             `json:"sourcePointer"`
}

type PropertySchema struct {
	Name     string       `json:"name"`
	Required Prov[bool]   `json:"required"`
	Schema   IrSchemaNode `json:"schema"`
}

type SchemaConstraint struct {
	Key   string    `json:"key"` // minLength, maxLength, pattern, minimum, ...
	Value Prov[any] `json:"value"`
}

type SchemaComposition struct {
	Kind    string         `json:"kind"` // allOf | oneOf | anyOf
	Members []IrSchemaNode `json:"members"`
}

type Parameter struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Location      ParameterLocation `json:"location"`
	Required      Prov[bool]        `json:"required"`
	Deprecated    Prov[bool]        `json:"deprecated"`
	Description   *Prov[string]     `json:"description"`
	Schema        IrSchemaNode      `json:"schema"`
	SourcePointer string            `json:"sourcePointer"`
}

type MediaType struct {
	MediaType string       `json:"mediaType"`
	Schema    IrSchemaNode `json:"schema"`
}

type RequestBody struct {
	ID            string        `json:"id"`
	Required      Prov[bool]    `json:"required"`
	Description   *Prov[string] `json:"description"`
	Content       []MediaType   `json:"content"`
	SourcePointer string        `json:"sourcePointer"`
}

type ResponseDef struct {
	ID            string        `json:"id"`
	StatusCode    string        `json:"statusCode"` // "200", "4XX", "default"
	Description   *Prov[string] `json:"description"`
	Content       []MediaType   `json:"content"`
	SourcePointer string        `json:"sourcePointer"`
}

type SecurityRequirement struct {
	SchemeID string   `json:"schemeId"`
	Scopes   []string `json:"scopes"`
}

type Endpoint struct {
	ID           string           `json:"id"`
	Method       Prov[HttpMethod] `json:"method"`
	PathTemplate Prov[string]     `json:"pathTemplate"`
	// CanonicalPath is the positional template (`/users/{}`) used for identity.
	CanonicalPath string                `json:"canonicalPath"`
	OperationID   *Prov[string]         `json:"operationId"`
	Summary       *Prov[string]         `json:"summary"`
	Description   *Prov[string]         `json:"description"`
	Deprecated    Prov[bool]            `json:"deprecated"`
	Parameters    []Parameter           `json:"parameters"`
	RequestBody   *RequestBody          `json:"requestBody"`
	Responses     []ResponseDef         `json:"responses"`
	Security      []SecurityRequirement `json:"security"`
	Tags          []string              `json:"tags"`
	SourcePointer string                `json:"sourcePointer"`
}

type Metadata struct {
	Title          Prov[string]  `json:"title"`
	Version        *Prov[string] `json:"version"`
	Description    *Prov[string] `json:"description"`
	TermsOfService *Prov[string] `json:"termsOfService"`
}

type ServerVariable struct {
	Name        string          `json:"name"`
	Default     Prov[string]    `json:"default"`
	EnumValues  *Prov[[]string] `json:"enumValues"`
	Description *Prov[string]   `json:"description"`
}

type Server struct {
	ID            string           `json:"id"`
	URL           Prov[string]     `json:"url"`
	Description   *Prov[string]    `json:"description"`
	Variables     []ServerVariable `json:"variables"`
	SourcePointer string           `json:"sourcePointer"`
}

type AuthScheme struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Kind     Prov[string]  `json:"kind"`     // apiKey|http|oauth2|openIdConnect|mutualTLS|unknown
	Location *Prov[string] `json:"location"` // header|query|cookie|n/a
	Scheme   *Prov[string] `json:"scheme"`   // bearer, basic, ...
	// ParameterName is, for apiKey schemes, the header/query/cookie name.
	ParameterName *Prov[string] `json:"parameterName"`
	Description   *Prov[string] `json:"description"`
	SourcePointer string        `json:"sourcePointer"`
}

type NamedSchema struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Schema        IrSchemaNode `json:"schema"`
	SourcePointer string       `json:"sourcePointer"`
}

type Resource struct {
	ID              string         `json:"id"`
	Name            Prov[string]   `json:"name"`
	Operations      Prov[[]string] `json:"operations"` // create|read|update|delete|list
	EndpointIDs     []string       `json:"endpointIds"`
	IdentifierField *Prov[string]  `json:"identifierField"`
	SourcePointer   string         `json:"sourcePointer"`
}

type Webhook struct {
	ID            string            `json:"id"`
	Event         Prov[string]      `json:"event"`
	Method        *Prov[HttpMethod] `json:"method"`
	PayloadSchema *IrSchemaNode     `json:"payloadSchema"`
	Description   *Prov[string]     `json:"description"`
	SourcePointer string            `json:"sourcePointer"`
	// Trigger comes from the x-pikopod-trigger extension; absent on sources
	// that predate it, so goldens stay byte-identical.
	Trigger *WebhookTrigger `json:"trigger,omitempty"`
}

// WebhookTrigger names the operation whose success fires a webhook event.
type WebhookTrigger struct {
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
}

type ErrorEntry struct {
	ID            string        `json:"id"`
	StatusCode    string        `json:"statusCode"`
	Description   *Prov[string] `json:"description"`
	Schema        *IrSchemaNode `json:"schema"`
	EndpointIDs   []string      `json:"endpointIds"`
	SourcePointer string        `json:"sourcePointer"`
}

type Relationship struct {
	ID             string        `json:"id"`
	FromResourceID string        `json:"fromResourceId"`
	ToResourceID   string        `json:"toResourceId"`
	Kind           Prov[string]  `json:"kind"` // references|contains|unknown
	ViaField       *Prov[string] `json:"viaField"`
	SourcePointer  string        `json:"sourcePointer"`
}

type StateTransition struct {
	ID            string        `json:"id"`
	ResourceID    string        `json:"resourceId"`
	Field         Prov[string]  `json:"field"`
	FromState     *Prov[string] `json:"fromState"`
	ToState       Prov[string]  `json:"toState"`
	ViaEndpointID *Prov[string] `json:"viaEndpointId"`
	SourcePointer string        `json:"sourcePointer"`
}

type PaginationStrategy struct {
	Kind          Prov[string] `json:"kind"` // none|cursor|offset|page|link|unknown
	Parameters    []string     `json:"parameters"`
	SourcePointer string       `json:"sourcePointer"`
}

type Example struct {
	ID            string    `json:"id"`
	ForNodeID     string    `json:"forNodeId"`
	MediaType     *string   `json:"mediaType"`
	Value         Prov[any] `json:"value"`
	SourcePointer string    `json:"sourcePointer"`
}

// SourceKind is one of openapi, graphql, postman, documentation.
type SourceKind = string

// SourceTier: A structured, B semi-structured, C unstructured.
type SourceTier = string

// IrStatus: Tier C imports land DRAFT and require explicit human confirmation.
type IrStatus = string

// ApiStyle: RESOURCE_ORIENTED, ACTION_ORIENTED or MIXED; null until the
// heuristic pass runs.
type ApiStyle = string

// ApiDefinition is the normalized internal representation — the versioned
// contract between ingestion and every phase after it.
type ApiDefinition struct {
	IrVersion         string             `json:"irVersion"`
	NormalizerVersion string             `json:"normalizerVersion"`
	Status            IrStatus           `json:"status"`
	SourceKind        SourceKind         `json:"sourceKind"`
	SourceTier        SourceTier         `json:"sourceTier"`
	Metadata          Metadata           `json:"metadata"`
	Servers           []Server           `json:"servers"`
	AuthSchemes       []AuthScheme       `json:"authSchemes"`
	Endpoints         []Endpoint         `json:"endpoints"`
	Schemas           []NamedSchema      `json:"schemas"`
	Resources         []Resource         `json:"resources"`
	ApiStyle          *Prov[ApiStyle]    `json:"apiStyle"`
	Webhooks          []Webhook          `json:"webhooks"`
	ErrorCatalogue    []ErrorEntry       `json:"errorCatalogue"`
	Relationships     []Relationship     `json:"relationships"`
	StateTransitions  []StateTransition  `json:"stateTransitions"`
	Pagination        PaginationStrategy `json:"pagination"`
	Examples          []Example          `json:"examples"`
}
