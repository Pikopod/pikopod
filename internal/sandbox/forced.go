// Forced responses, peek semantics: no store write, webhook, or idempotency
// record. An undeclared status is REFUSED; auth is enforced BEFORE forcing.
package sandbox

import (
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// ForcedStatusHeader requests a specific declared response status.
const ForcedStatusHeader = "x-pikopod-status"

// forcedMarkerHeader keeps a forced response from ever being mistaken for
// organic sandbox behavior.
const forcedMarkerHeader = "x-pikopod-forced"

// forcedResponse answers a request carrying ForcedStatusHeader, else nil.
// Called after auth, before faults.
func (e *Engine) forcedResponse(endpoint *ir.Endpoint, req *ingressRequest) *RawResponse {
	raw := req.header(ForcedStatusHeader)
	if raw == nil {
		return nil
	}
	status, err := strconv.Atoi(strings.TrimSpace(*raw))
	if err != nil || status < 100 || status > 599 {
		resp := buildErrorResponse(400, "invalid "+ForcedStatusHeader+" value "+strconv.Quote(*raw)+" — pass a declared status code like 402", nil)
		resp.Headers[forcedMarkerHeader] = "refused"
		return resp
	}
	schema, declared := declaredResponseSchema(endpoint, status)
	if !declared {
		resp := buildErrorResponse(400,
			"status "+strconv.Itoa(status)+" is not declared for "+endpoint.Method.Value+" "+endpoint.PathTemplate.Value+
				" — declared: "+strings.Join(declaredStatusCodes(endpoint), ", "), nil)
		resp.Headers[forcedMarkerHeader] = "refused"
		return resp
	}
	var resp *RawResponse
	if schema != nil {
		synth := e.synthCtx("forced", strconv.Itoa(status))
		if body := synthesize(schema, synth, 0, ""); body != nil {
			resp = jsonResponse(status, body, nil)
		}
	}
	if resp == nil {
		// Declared but bodyless (204-style, or no JSON content): honest empty.
		resp = &RawResponse{Status: status, Headers: map[string]string{}}
	}
	resp.Headers[forcedMarkerHeader] = "true"
	return resp
}

// declaredResponseSchema never falls through to "default" as errorSchema does —
// forcing must name a code the contract actually claims.
func declaredResponseSchema(endpoint *ir.Endpoint, status int) (*ir.IrSchemaNode, bool) {
	code := strconv.Itoa(status)
	rangeCode := code[:1] + "XX"
	for i := range endpoint.Responses {
		if endpoint.Responses[i].StatusCode == code {
			return jsonContent(&endpoint.Responses[i]), true
		}
	}
	for i := range endpoint.Responses {
		if strings.EqualFold(endpoint.Responses[i].StatusCode, rangeCode) {
			return jsonContent(&endpoint.Responses[i]), true
		}
	}
	return nil, false
}

func declaredStatusCodes(endpoint *ir.Endpoint) []string {
	out := make([]string, 0, len(endpoint.Responses))
	for i := range endpoint.Responses {
		out = append(out, endpoint.Responses[i].StatusCode)
	}
	return out
}
