// The raw response a sandbox returns. Never the platform error envelope
// (structural errors are a neutral `{ message }`), and always deterministic.
package sandbox

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// RawResponse is the serialized response shape (body nil means no body).
type RawResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

const jsonContentType = "application/json; charset=utf-8"

var threeDigits = regexp.MustCompile(`^\d{3}$`)

// statusIsSuccess maps a declared code to its numeric 2xx value, nil otherwise.
// "2xx"/"2XX" counts as 200.
func statusIsSuccess(code string) *int {
	if threeDigits.MatchString(code) {
		n, _ := strconv.Atoi(code)
		if n >= 200 && n < 300 {
			return &n
		}
		return nil
	}
	if strings.EqualFold(code, "2xx") {
		n := 200
		return &n
	}
	return nil
}

// pickSuccessStatus returns the lowest declared 2xx (falls back to fallback).
func pickSuccessStatus(endpoint *ir.Endpoint, fallback int) int {
	var best *int
	for _, r := range endpoint.Responses {
		status := statusIsSuccess(r.StatusCode)
		if status != nil && (best == nil || *status < *best) {
			best = status
		}
	}
	if best != nil {
		return *best
	}
	return fallback
}

func jsonContent(response *ir.ResponseDef) *ir.IrSchemaNode {
	if response == nil {
		return nil
	}
	for i := range response.Content {
		if strings.Contains(strings.ToLower(response.Content[i].MediaType), "json") {
			return &response.Content[i].Schema
		}
	}
	return nil
}

// successSchema matches the exact status code first, then a range ("2XX").
func successSchema(endpoint *ir.Endpoint, status int) *ir.IrSchemaNode {
	code := strconv.Itoa(status)
	for i := range endpoint.Responses {
		if endpoint.Responses[i].StatusCode == code {
			return jsonContent(&endpoint.Responses[i])
		}
	}
	for i := range endpoint.Responses {
		if s := statusIsSuccess(endpoint.Responses[i].StatusCode); s != nil && *s == status {
			return jsonContent(&endpoint.Responses[i])
		}
	}
	return nil
}

// errorSchema matches exact, then NXX range, then default.
func errorSchema(endpoint *ir.Endpoint, status int) *ir.IrSchemaNode {
	code := strconv.Itoa(status)
	rangeCode := code[:1] + "XX"
	for i := range endpoint.Responses {
		if endpoint.Responses[i].StatusCode == code {
			return jsonContent(&endpoint.Responses[i])
		}
	}
	for i := range endpoint.Responses {
		if strings.ToUpper(endpoint.Responses[i].StatusCode) == rangeCode {
			return jsonContent(&endpoint.Responses[i])
		}
	}
	for i := range endpoint.Responses {
		if endpoint.Responses[i].StatusCode == "default" {
			return jsonContent(&endpoint.Responses[i])
		}
	}
	return nil
}

func jsonResponse(status int, value any, extraHeaders map[string]string) *RawResponse {
	headers := map[string]string{}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	if status == 204 {
		return &RawResponse{Status: status, Headers: headers, Body: nil}
	}
	headers["content-type"] = jsonContentType
	body, err := marshalJSValue(value)
	if err != nil {
		// Catch-all: an internal fault becomes a mirrored 500.
		return buildErrorResponse(500, "Internal Server Error", nil)
	}
	return &RawResponse{Status: status, Headers: headers, Body: body}
}

// buildSuccessResponse: the lowest declared 2xx, empty-bodied. For passthrough.
func buildSuccessResponse(endpoint *ir.Endpoint) *RawResponse {
	status := pickSuccessStatus(endpoint, 200)
	if status == 204 {
		return &RawResponse{Status: status, Headers: map[string]string{}, Body: nil}
	}
	body := "{}"
	if schema := successSchema(endpoint, status); schema != nil && schema.Type.Value == "array" {
		body = "[]"
	}
	return &RawResponse{Status: status, Headers: map[string]string{"content-type": jsonContentType}, Body: []byte(body)}
}

// buildErrorResponse is a neutral, non-platform `{ "message": ... }` body.
func buildErrorResponse(status int, message string, extraHeaders map[string]string) *RawResponse {
	headers := map[string]string{"content-type": jsonContentType}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	obj := NewJSONObject()
	obj.Set("message", message)
	body, _ := marshalJSValue(obj)
	return &RawResponse{Status: status, Headers: headers, Body: body}
}
