// Package importer normalizes spec bytes into the IR: detection, hardened
// parsing, Swagger 2.0 conversion, local $ref resolution. Pure, no network.
package importer

import (
	"encoding/json"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
)

// Kind is a detected source kind.
type Kind string

const (
	KindOpenAPI              Kind = "openapi"
	KindPostman              Kind = "postman"
	KindGraphQLSDL           Kind = "graphql-sdl"
	KindGraphQLIntrospection Kind = "graphql-introspection"
	KindDocumentation        Kind = "documentation"
)

// Detect classifies raw bytes by structural markers, never provider names; an
// unrecognized JSON document is a typed error, never a guess.
func Detect(raw []byte) (Kind, error) {
	s := string(raw)
	trimmed := strings.TrimLeftFunc(s, isJSWhitespace)

	// Binary/markup documents are Tier C (unstructured) — checked first so a
	// PDF or HTML page is never mis-parsed as a spec.
	if strings.HasPrefix(s, "%PDF-") {
		return KindDocumentation, nil
	}
	head := trimmed
	if len(head) > 512 {
		head = head[:512]
	}
	lowerHead := strings.ToLower(head)
	// The <body> heuristic must NOT fire on JSON: real Postman collections
	// embed HTML descriptions within their first 512 bytes.
	jsonLeading := strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
	if strings.HasPrefix(lowerHead, "<!doctype html") || strings.HasPrefix(lowerHead, "<html") || (!jsonLeading && strings.Contains(lowerHead, "<body")) {
		return KindDocumentation, nil
	}

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		doc, err := parseJSONSafely(s, detectLimits)
		if err != nil {
			return "", &SpecError{Code: SpecParseError, Message: "malformed JSON document"}
		}
		if obj, ok := doc.(*OrdMap); ok {
			if obj.Has("openapi") || obj.Has("swagger") {
				return KindOpenAPI, nil
			}
			if obj.Has("__schema") {
				return KindGraphQLIntrospection, nil
			}
			if data, ok := obj.Get("data"); ok {
				if dm, ok := data.(*OrdMap); ok && dm.Has("__schema") {
					return KindGraphQLIntrospection, nil
				}
			}
			info, hasInfo := obj.Get("info")
			infoMap, infoIsMap := info.(*OrdMap)
			if hasInfo && infoIsMap {
				if schema, ok := infoMap.Get("schema"); ok {
					if str, ok := schema.(string); ok && strings.Contains(str, "collection") {
						return KindPostman, nil
					}
				}
			}
			if item, ok := obj.Get("item"); ok {
				if _, isArr := item.([]any); isArr && hasInfo && infoIsMap {
					return KindPostman, nil
				}
			}
		}
		return "", &SpecError{Code: SpecUnsupportedVersion, Message: "unrecognized JSON document"}
	}

	// Not JSON: structured YAML/SDL, or unstructured documentation.
	if yamlSpecMarker.MatchString(s) {
		return KindOpenAPI, nil
	}
	if graphqlMarker.MatchString(s) || strings.HasPrefix(trimmed, `"""`) {
		if looksLikeGraphQLSDL(s) {
			return KindGraphQLSDL, nil
		}
	}
	// Anything else — Markdown, prose, HTML fragments — is unstructured Tier C.
	return KindDocumentation, nil
}

// NormalizeOpenAPI normalizes OpenAPI 3.x or Swagger 2.0 bytes into the IR. A
// 2.0 converter upgrade re-versions every 2.0-sourced API via normalizerVersion.
func NormalizeOpenAPI(raw []byte) (*ir.ApiDefinition, error) {
	kind, err := Detect(raw)
	if err != nil {
		return nil, userFacing(err)
	}
	switch kind {
	case KindOpenAPI:
		// fall through
	case KindPostman:
		return NormalizePostman(raw)
	case KindGraphQLSDL:
		return NormalizeGraphQLSDL(raw)
	case KindGraphQLIntrospection:
		return NormalizeGraphQLIntrospection(raw)
	case KindDocumentation:
		return nil, errfmt.New("import spec", "unstructured documentation is not yet supported in pikopod v1 (Tier C requires model-assisted extraction)",
			"provide an OpenAPI 3.x or Swagger 2.0 document instead", "")
	}

	limits := DefaultParseLimits
	doc, err := parseStructured(string(raw), formatAuto, limits)
	if err != nil {
		return nil, userFacing(err)
	}

	normalizerVersion := ir.NormalizerVersion
	if isSwagger2Document(doc) {
		converted, err := convertSwagger2ToOpenAPI(doc, limits)
		if err != nil {
			return nil, userFacing(err)
		}
		doc = converted
		// Mirrors normalizeSourceWithConversion: the converter version
		// participates in normalizerVersion and therefore in normalizedHash.
		normalizerVersion += "+" + swagger2ConverterName + "@" + swagger2ConverterVersion
	}

	def, err := normalizeOpenAPIValue(doc, limits, "ACTIVE")
	if err != nil {
		return nil, userFacing(err)
	}
	def.NormalizerVersion = normalizerVersion
	return def, nil
}

// NormalizeLLMExtracted runs the pipeline over a MODEL-WRITTEN spec, then
// downgrades every tier to LLM_EXTRACTED so OBSERVED traffic wins on conflict.
func NormalizeLLMExtracted(raw []byte) (*ir.ApiDefinition, error) {
	kind, err := Detect(raw)
	if err != nil {
		return nil, userFacing(err)
	}
	if kind != KindOpenAPI {
		return nil, errfmt.New("import spec", "model-extracted documents must be OpenAPI", "this is an internal invariant of the docs extractor; file an issue", "")
	}
	doc, err := parseStructured(string(raw), formatAuto, DefaultParseLimits)
	if err != nil {
		return nil, userFacing(err)
	}
	def, err := normalizeOpenAPIValue(doc, DefaultParseLimits, "DRAFT")
	if err != nil {
		return nil, userFacing(err)
	}
	def.NormalizerVersion = ir.NormalizerVersion + "+llm-extracted"
	if err := downgradeProvenance(def, ir.ProvenanceLLMExtracted, 0.7); err != nil {
		return nil, err
	}
	return def, nil
}

// downgradeProvenance rewrites every Prov tier via a JSON walk; wrappers are
// identified structurally, so no field enumeration can rot.
func downgradeProvenance(def *ir.ApiDefinition, tier string, confidence float64) error {
	raw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if _, hasV := n["value"]; hasV {
				if _, hasP := n["provenance"]; hasP {
					if _, hasC := n["confidence"]; hasC {
						n["provenance"] = tier
						n["confidence"] = confidence
					}
				}
			}
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(doc)
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return json.Unmarshal(rewritten, def)
}

// userFacing wraps a SpecError in the pikopod error contract; other errors
// pass through unchanged.
func userFacing(err error) error {
	spec, ok := err.(*SpecError)
	if !ok {
		return err
	}
	fix := "check that the document is a well-formed OpenAPI 3.x or Swagger 2.0 spec"
	switch spec.Code {
	case SpecTooLarge, SpecDepthExceeded:
		fix = "reduce the document's size or nesting depth"
	case SpecRefUnresolvable:
		fix = "inline or repair the offending $ref (remote $refs are not permitted)"
	}
	return errfmt.Newf("import spec", fix, "", "%s (%s)", spec.Message, spec.Code)
}
