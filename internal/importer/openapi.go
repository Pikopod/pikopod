package importer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

var httpMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE"}

func normalizeOpenAPIValue(parsed any, limits ParseLimits, status string) (*ir.ApiDefinition, error) {
	return normalizeOpenAPIValueFrom(parsed, limits, status, nil)
}

func normalizeOpenAPIValueFrom(parsed any, limits ParseLimits, status string, src *Source) (*ir.ApiDefinition, error) {
	doc, ok := parsed.(*OrdMap)
	if !ok {
		return nil, specErr(SpecParseError, "root of an OpenAPI document must be an object")
	}
	if err := assertSupportedVersion(doc); err != nil {
		return nil, err
	}
	resolver, err := newRefResolverFrom(doc, limits, src)
	if err != nil {
		return nil, err
	}

	examples := []ir.Example{}
	endpoints, err := normalizeEndpoints(doc, resolver, limits, &examples)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(examples, func(a, b int) bool { return ir.JSLess(examples[a].ID, examples[b].ID) })
	schemas, err := normalizeNamedSchemas(doc, resolver, limits)
	if err != nil {
		return nil, err
	}
	webhooks, err := normalizeWebhooks(doc, resolver, limits)
	if err != nil {
		return nil, err
	}
	envelope, err := normalizeWebhookEnvelope(doc)
	if err != nil {
		return nil, err
	}

	return &ir.ApiDefinition{
		IrVersion:         ir.IRVersion,
		NormalizerVersion: ir.NormalizerVersion,
		Status:            status,
		SourceKind:        "openapi",
		SourceTier:        "A",
		Metadata:          normalizeMetadata(doc),
		Servers:           normalizeServers(doc),
		AuthSchemes:       normalizeAuthSchemes(doc),
		Endpoints:         endpoints,
		Schemas:           schemas,
		Webhooks:          webhooks,
		WebhookEnvelope:   envelope,
		Examples:          examples,
	}, nil
}

func assertSupportedVersion(doc *OrdMap) error {
	if version, ok := doc.GetOr("openapi").(string); ok &&
		(strings.HasPrefix(version, "3.0") || strings.HasPrefix(version, "3.1")) {
		return nil
	}
	if swagger, ok := doc.GetOr("swagger").(string); ok && swagger == "2.0" {
		return specErr(SpecUnsupportedVersion, "Swagger 2.0 is not supported; use OpenAPI 3.0 or 3.1")
	}
	return specErr(SpecUnsupportedVersion, "unrecognized document; expected OpenAPI 3.0 or 3.1")
}

func optionalString(value any, pointer string) *ir.Prov[string] {
	if s, ok := value.(string); ok {
		p := ir.Explicit(s, pointer)
		return &p
	}
	return nil
}

func getMap(m *OrdMap, key string) *OrdMap {
	if sub, ok := m.GetOr(key).(*OrdMap); ok {
		return sub
	}
	return NewOrdMap()
}

func normalizeMetadata(doc *OrdMap) ir.Metadata {
	info := getMap(doc, "info")
	title := "Untitled API"
	if t, ok := info.GetOr("title").(string); ok {
		title = t
	}
	return ir.Metadata{
		Title:          ir.Explicit(title, "#/info/title"),
		Version:        optionalString(info.GetOr("version"), "#/info/version"),
		Description:    optionalString(info.GetOr("description"), "#/info/description"),
		TermsOfService: optionalString(info.GetOr("termsOfService"), "#/info/termsOfService"),
	}
}

func normalizeServers(doc *OrdMap) []ir.Server {
	serversRaw, _ := doc.GetOr("servers").([]any)
	out := []ir.Server{}
	i := 0
	for _, sRaw := range serversRaw {
		s, ok := sRaw.(*OrdMap)
		if !ok {
			continue
		}
		url := ""
		if u, ok := s.GetOr("url").(string); ok {
			url = u
		}
		variables := []ir.ServerVariable{}
		if varsRaw, ok := s.GetOr("variables").(*OrdMap); ok {
			for _, name := range varsRaw.Keys() {
				varObj, _ := varsRaw.GetOr(name).(*OrdMap)
				if varObj == nil {
					varObj = NewOrdMap()
				}
				def := ""
				if d, ok := varObj.GetOr("default").(string); ok {
					def = d
				}
				var enumValues *ir.Prov[[]string]
				if enumRaw, ok := varObj.GetOr("enum").([]any); ok {
					vals := []string{}
					for _, e := range enumRaw {
						if es, ok := e.(string); ok {
							vals = append(vals, es)
						}
					}
					ev := ir.Explicit(vals, fmt.Sprintf("#/servers/%d/variables/%s/enum", i, name))
					enumValues = &ev
				}
				variables = append(variables, ir.ServerVariable{
					Name:        name,
					Default:     ir.Explicit(def, fmt.Sprintf("#/servers/%d/variables/%s/default", i, name)),
					EnumValues:  enumValues,
					Description: optionalString(varObj.GetOr("description"), fmt.Sprintf("#/servers/%d/variables/%s/description", i, name)),
				})
			}
		}
		sort.SliceStable(variables, func(a, b int) bool { return ir.JSLess(variables[a].Name, variables[b].Name) })
		out = append(out, ir.Server{
			ID:            ir.ServerID(url),
			URL:           ir.Explicit(url, fmt.Sprintf("#/servers/%d/url", i)),
			Description:   optionalString(s.GetOr("description"), fmt.Sprintf("#/servers/%d/description", i)),
			Variables:     variables,
			SourcePointer: fmt.Sprintf("#/servers/%d", i),
		})
		i++
	}
	return out
}

func normalizeAuthSchemes(doc *OrdMap) []ir.AuthScheme {
	schemes := getMap(getMap(doc, "components"), "securitySchemes")
	out := []ir.AuthScheme{}
	for _, name := range schemes.Keys() {
		s, ok := schemes.GetOr(name).(*OrdMap)
		if !ok {
			continue
		}
		ptr := "#/components/securitySchemes/" + name
		kind := mapAuthKind(s.GetOr("type"))
		var location ir.Prov[string]
		if in, ok := s.GetOr("in").(string); ok && (in == "header" || in == "query" || in == "cookie") {
			location = ir.Explicit(in, ptr+"/in")
		} else {
			location = ir.Explicit("n/a", ptr)
		}
		var parameterName *ir.Prov[string]
		if kind == "apiKey" {
			parameterName = optionalString(s.GetOr("name"), ptr+"/name")
		}
		out = append(out, ir.AuthScheme{
			ID:            ir.AuthSchemeID(name),
			Name:          name,
			Kind:          ir.Explicit(kind, ptr+"/type"),
			Location:      &location,
			Scheme:        optionalString(s.GetOr("scheme"), ptr+"/scheme"),
			ParameterName: parameterName,
			Description:   optionalString(s.GetOr("description"), ptr+"/description"),
			SourcePointer: ptr,
		})
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].Name, out[b].Name) })
	return out
}

func mapAuthKind(t any) string {
	if s, ok := t.(string); ok {
		switch s {
		case "apiKey", "http", "oauth2", "openIdConnect", "mutualTLS":
			return s
		}
	}
	return "unknown"
}

func normalizeNamedSchemas(doc *OrdMap, resolver *refResolver, limits ParseLimits) ([]ir.NamedSchema, error) {
	schemas := getMap(getMap(doc, "components"), "schemas")
	out := []ir.NamedSchema{}
	for _, name := range schemas.Keys() {
		id := ir.NamedSchemaID(name)
		ptr := "#/components/schemas/" + name
		schema, err := normalizeSchema(schemas.GetOr(name), id, "schema", ptr, resolver, limits, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, ir.NamedSchema{ID: id, Name: name, Schema: schema, SourcePointer: ptr})
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].Name, out[b].Name) })
	return out, nil
}

func normalizeEndpoints(doc *OrdMap, resolver *refResolver, limits ParseLimits, examples *[]ir.Example) ([]ir.Endpoint, error) {
	paths := getMap(doc, "paths")
	endpoints := []ir.Endpoint{}

	for _, path := range paths.Keys() {
		pathItemResolved, err := resolver.resolve(paths.GetOr(path))
		if err != nil {
			return nil, err
		}
		item, ok := pathItemResolved.(*OrdMap)
		if !ok {
			continue
		}
		sharedParams, _ := item.GetOr("parameters").([]any)

		for _, method := range httpMethods {
			op, ok := item.GetOr(strings.ToLower(method)).(*OrdMap)
			if !ok {
				continue
			}
			epID := ir.EndpointID(method, path)
			opPointer := "#/paths/" + jpescape(path) + "/" + strings.ToLower(method)

			opParams, _ := op.GetOr("parameters").([]any)
			merged := append(append([]any{}, sharedParams...), opParams...)
			parameters, err := normalizeParameters(merged, epID, opPointer, resolver, limits)
			if err != nil {
				return nil, err
			}
			requestBody, err := normalizeRequestBody(op.GetOr("requestBody"), epID, opPointer, resolver, limits, examples)
			if err != nil {
				return nil, err
			}
			responses, err := normalizeResponses(op.GetOr("responses"), epID, opPointer, resolver, limits, examples)
			if err != nil {
				return nil, err
			}

			securityRaw := op.GetOr("security")
			if securityRaw == nil {
				securityRaw = doc.GetOr("security")
			}

			deprecated := ir.Derived(false, opPointer)
			if b, ok := op.GetOr("deprecated").(bool); ok && b {
				deprecated = ir.Explicit(true, opPointer+"/deprecated")
			}

			endpoints = append(endpoints, ir.Endpoint{
				ID:            epID,
				Method:        ir.Explicit(method, opPointer),
				PathTemplate:  ir.Explicit(path, "#/paths/"+jpescape(path)),
				CanonicalPath: ir.CanonicalPathTemplate(path),
				OperationID:   optionalString(op.GetOr("operationId"), opPointer+"/operationId"),
				Summary:       optionalString(op.GetOr("summary"), opPointer+"/summary"),
				Description:   optionalString(op.GetOr("description"), opPointer+"/description"),
				Deprecated:    deprecated,
				Parameters:    parameters,
				RequestBody:   requestBody,
				Responses:     responses,
				Security:      normalizeSecurity(securityRaw),
				Tags:          normalizeTags(op.GetOr("tags")),
				SourcePointer: opPointer,
			})
		}
	}

	sort.SliceStable(endpoints, func(a, b int) bool { return ir.JSLess(endpoints[a].ID, endpoints[b].ID) })
	return endpoints, nil
}

func normalizeTags(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, t := range arr {
		if s, ok := t.(string); ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a], out[b]) })
	return out
}

func normalizeParameters(rawParams []any, epID, opPointer string, resolver *refResolver, limits ParseLimits) ([]ir.Parameter, error) {

	byKey := map[string]ir.Parameter{}
	for i, rawParam := range rawParams {
		resolved, err := resolver.resolve(rawParam)
		if err != nil {
			return nil, err
		}
		p, ok := resolved.(*OrdMap)
		if !ok {
			continue
		}
		name, _ := p.GetOr("name").(string)
		location := normalizeLocation(p.GetOr("in"))
		if name == "" || location == "" {
			continue
		}
		ptr := fmt.Sprintf("%s/parameters/%d", opPointer, i)
		paramID := ir.ParameterID(epID, location, name)

		var required ir.Prov[bool]
		if location == "path" {
			required = ir.Derived(true, ptr)
		} else if b, ok := p.GetOr("required").(bool); ok && b {
			required = ir.Explicit(true, ptr+"/required")
		} else {
			required = ir.Derived(false, ptr)
		}
		deprecated := ir.Derived(false, ptr)
		if b, ok := p.GetOr("deprecated").(bool); ok && b {
			deprecated = ir.Explicit(true, ptr+"/deprecated")
		}

		schemaRaw := p.GetOr("schema")
		if schemaRaw == nil {
			schemaRaw = NewOrdMap()
		}
		schema, err := normalizeSchema(schemaRaw, paramID, "schema", ptr+"/schema", resolver, limits, 0)
		if err != nil {
			return nil, err
		}
		byKey[location+":"+name] = ir.Parameter{
			ID:            paramID,
			Name:          name,
			Location:      location,
			Required:      required,
			Deprecated:    deprecated,
			Description:   optionalString(p.GetOr("description"), ptr+"/description"),
			Schema:        schema,
			SourcePointer: ptr,
		}
	}
	out := make([]ir.Parameter, 0, len(byKey))
	for _, p := range byKey {
		out = append(out, p)
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].ID, out[b].ID) })
	return out, nil
}

func normalizeLocation(value any) string {
	if s, ok := value.(string); ok {
		switch s {
		case "path", "query", "header", "cookie":
			return s
		}
	}
	return ""
}

func normalizeContent(contentRaw any, parentID, pointer string, resolver *refResolver, limits ParseLimits, examples *[]ir.Example) ([]ir.MediaType, error) {
	content, ok := contentRaw.(*OrdMap)
	if !ok {
		return []ir.MediaType{}, nil
	}
	out := []ir.MediaType{}
	for _, mediaType := range content.Keys() {
		var schemaRaw any = NewOrdMap()
		mtPointer := pointer + "/content/" + jpescape(mediaType)
		if mt, ok := content.GetOr(mediaType).(*OrdMap); ok {
			if s := mt.GetOr("schema"); s != nil {
				schemaRaw = s
			}
			if err := collectExamples(mt, parentID, mediaType, mtPointer, resolver, examples); err != nil {
				return nil, err
			}
		}
		schema, err := normalizeSchema(schemaRaw, parentID, "content:"+mediaType,
			mtPointer+"/schema", resolver, limits, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, ir.MediaType{MediaType: mediaType, Schema: schema})
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].MediaType, out[b].MediaType) })
	return out, nil
}

func normalizeRequestBody(raw any, epID, opPointer string, resolver *refResolver, limits ParseLimits, examples *[]ir.Example) (*ir.RequestBody, error) {
	resolved, err := resolver.resolve(raw)
	if err != nil {
		return nil, err
	}
	rb, ok := resolved.(*OrdMap)
	if !ok {
		return nil, nil
	}
	ptr := opPointer + "/requestBody"
	required := ir.Derived(false, ptr)
	if b, ok := rb.GetOr("required").(bool); ok && b {
		required = ir.Explicit(true, ptr+"/required")
	}
	content, err := normalizeContent(rb.GetOr("content"), ir.RequestBodyID(epID), ptr, resolver, limits, examples)
	if err != nil {
		return nil, err
	}
	return &ir.RequestBody{
		ID:            ir.RequestBodyID(epID),
		Required:      required,
		Description:   optionalString(rb.GetOr("description"), ptr+"/description"),
		Content:       content,
		SourcePointer: ptr,
	}, nil
}

func normalizeResponses(raw any, epID, opPointer string, resolver *refResolver, limits ParseLimits, examples *[]ir.Example) ([]ir.ResponseDef, error) {
	responses, ok := raw.(*OrdMap)
	if !ok {
		return []ir.ResponseDef{}, nil
	}
	out := []ir.ResponseDef{}
	for _, statusCode := range responses.Keys() {
		resolved, err := resolver.resolve(responses.GetOr(statusCode))
		if err != nil {
			return nil, err
		}
		resp, ok := resolved.(*OrdMap)
		if !ok {
			resp = NewOrdMap()
		}
		ptr := opPointer + "/responses/" + statusCode
		content, err := normalizeContent(resp.GetOr("content"), ir.ResponseID(epID, statusCode), ptr, resolver, limits, examples)
		if err != nil {
			return nil, err
		}
		out = append(out, ir.ResponseDef{
			ID:            ir.ResponseID(epID, statusCode),
			StatusCode:    statusCode,
			Description:   optionalString(resp.GetOr("description"), ptr+"/description"),
			Content:       content,
			SourcePointer: ptr,
		})
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].StatusCode, out[b].StatusCode) })
	return out, nil
}

func normalizeSecurity(raw any) []ir.SecurityRequirement {
	arr, ok := raw.([]any)
	if !ok {
		return []ir.SecurityRequirement{}
	}
	out := []ir.SecurityRequirement{}
	for _, requirement := range arr {
		req, ok := requirement.(*OrdMap)
		if !ok {
			continue
		}
		for _, name := range req.Keys() {
			scopes := []string{}
			if scopesRaw, ok := req.GetOr(name).([]any); ok {
				for _, s := range scopesRaw {
					if str, ok := s.(string); ok {
						scopes = append(scopes, str)
					}
				}
			}
			out = append(out, ir.SecurityRequirement{SchemeID: ir.AuthSchemeID(name), Scopes: scopes})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].SchemeID, out[b].SchemeID) })
	return out
}

func normalizeWebhooks(doc *OrdMap, resolver *refResolver, limits ParseLimits) ([]ir.Webhook, error) {
	webhooks := getMap(doc, "webhooks")
	out := []ir.Webhook{}
	for _, event := range webhooks.Keys() {
		itemResolved, err := resolver.resolve(webhooks.GetOr(event))
		if err != nil {
			return nil, err
		}
		pathItem, ok := itemResolved.(*OrdMap)
		if !ok {
			continue
		}
		for _, method := range httpMethods {
			op, ok := pathItem.GetOr(strings.ToLower(method)).(*OrdMap)
			if !ok {
				continue
			}
			ptr := "#/webhooks/" + jpescape(event) + "/" + strings.ToLower(method)
			rbResolved, err := resolver.resolve(op.GetOr("requestBody"))
			if err != nil {
				return nil, err
			}
			content := []ir.MediaType{}
			if rb, ok := rbResolved.(*OrdMap); ok {
				content, err = normalizeContent(rb.GetOr("content"), ir.WebhookID(event), ptr, resolver, limits, nil)
				if err != nil {
					return nil, err
				}
			}
			var payloadSchema *ir.IrSchemaNode
			if len(content) > 0 {
				payloadSchema = &content[0].Schema
			}
			methodProv := ir.Explicit(method, ptr)
			out = append(out, ir.Webhook{
				ID:            ir.WebhookID(event),
				Event:         ir.Explicit(event, ptr),
				Method:        &methodProv,
				PayloadSchema: payloadSchema,
				Description:   optionalString(op.GetOr("description"), ptr+"/description"),
				SourcePointer: ptr,
				Trigger:       webhookTriggerOf(pathItem, op),
				EmitOnly:      webhookEmitOnlyOf(pathItem, op),
			})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return ir.JSLess(out[a].ID, out[b].ID) })
	return out, nil
}

func webhookEmitOnlyOf(pathItem, op *OrdMap) bool {
	for _, holder := range []*OrdMap{op, pathItem} {
		if v, ok := holder.GetOr("x-pikopod-emit-only").(bool); ok && v {
			return true
		}
	}
	return false
}

func webhookTriggerOf(pathItem, op *OrdMap) *ir.WebhookTrigger {
	for _, holder := range []*OrdMap{op, pathItem} {
		ext, ok := holder.GetOr("x-pikopod-trigger").(*OrdMap)
		if !ok {
			continue
		}
		method, _ := ext.GetOr("method").(string)
		path, _ := ext.GetOr("path").(string)
		if method != "" && path != "" {
			return &ir.WebhookTrigger{Method: strings.ToLower(method), PathTemplate: path}
		}
	}
	return nil
}

func collectExamples(mt *OrdMap, parentID, mediaType, pointer string, resolver *refResolver, examples *[]ir.Example) error {
	if examples == nil {
		return nil
	}
	mtCopy := mediaType
	if mt.Has("example") {
		*examples = append(*examples, ir.Example{
			ID:            ir.ExampleID(parentID, mediaType),
			ForNodeID:     parentID,
			MediaType:     &mtCopy,
			Value:         ir.Explicit(plainValue(mt.GetOr("example")), pointer+"/example"),
			SourcePointer: pointer + "/example",
		})
	}
	named, ok := mt.GetOr("examples").(*OrdMap)
	if !ok {
		return nil
	}
	for _, name := range named.Keys() {
		resolved, err := resolver.resolve(named.GetOr(name))
		if err != nil {
			return err
		}
		entry, ok := resolved.(*OrdMap)
		if !ok || !entry.Has("value") {
			continue
		}
		ptr := pointer + "/examples/" + jpescape(name) + "/value"
		*examples = append(*examples, ir.Example{
			ID:            ir.ExampleID(parentID, mediaType+":"+name),
			ForNodeID:     parentID,
			MediaType:     &mtCopy,
			Value:         ir.Explicit(plainValue(entry.GetOr("value")), ptr),
			SourcePointer: ptr,
		})
	}
	return nil
}
