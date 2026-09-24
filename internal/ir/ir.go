package ir

type HttpMethod = string

type ParameterLocation = string

type ScalarType = string

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
	Key   string    `json:"key"`
	Value Prov[any] `json:"value"`
}

type SchemaComposition struct {
	Kind    string         `json:"kind"`
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
	StatusCode    string        `json:"statusCode"`
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
	Kind     Prov[string]  `json:"kind"`
	Location *Prov[string] `json:"location"`
	Scheme   *Prov[string] `json:"scheme"`

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

type Webhook struct {
	ID            string            `json:"id"`
	Event         Prov[string]      `json:"event"`
	Method        *Prov[HttpMethod] `json:"method"`
	PayloadSchema *IrSchemaNode     `json:"payloadSchema"`
	Description   *Prov[string]     `json:"description"`
	SourcePointer string            `json:"sourcePointer"`

	Trigger *WebhookTrigger `json:"trigger,omitempty"`

	EmitOnly bool `json:"emitOnly,omitempty"`
}

type WebhookTrigger struct {
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
}

type Example struct {
	ID            string    `json:"id"`
	ForNodeID     string    `json:"forNodeId"`
	MediaType     *string   `json:"mediaType"`
	Value         Prov[any] `json:"value"`
	SourcePointer string    `json:"sourcePointer"`
}

type SourceKind = string

type SourceTier = string

type IrStatus = string

type ApiDefinition struct {
	IrVersion         string           `json:"irVersion"`
	NormalizerVersion string           `json:"normalizerVersion"`
	Status            IrStatus         `json:"status"`
	SourceKind        SourceKind       `json:"sourceKind"`
	SourceTier        SourceTier       `json:"sourceTier"`
	Metadata          Metadata         `json:"metadata"`
	Servers           []Server         `json:"servers"`
	AuthSchemes       []AuthScheme     `json:"authSchemes"`
	Endpoints         []Endpoint       `json:"endpoints"`
	Schemas           []NamedSchema    `json:"schemas"`
	Webhooks          []Webhook        `json:"webhooks"`
	WebhookEnvelope   *WebhookEnvelope `json:"webhookEnvelope,omitempty"`
	Examples          []Example        `json:"examples"`
}
