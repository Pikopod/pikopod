// Package volatile is pikopod's noise control: curated, deliberately
// asymmetric field lists, matched exact-name only and never by substring.
package volatile

import "strings"

// requestFields: names that change per call, stripped from replay request
// hashing. Large is fine — the request side carries no drift coverage.
var requestFields = map[string]bool{
	// Idempotency / dedupe
	"idempotency_key": true, "idempotency-key": true, "idempotencykey": true,
	"nonce": true, "dedupe_key": true,
	// Request identity / correlation
	"request_id": true, "requestid": true, "request-id": true, "correlation_id": true,
	"correlationid": true, "trace_id": true, "traceid": true, "span_id": true, "spanid": true,
	"traceparent": true, "tracestate": true,
	// Signatures / auth material that rotates per call
	"signature": true, "sig": true, "hmac": true, "client_assertion": true,
	"webidentitytoken": true, "csrf_token": true, "xsrf_token": true,
	// Timestamps
	"timestamp": true, "timestamps": true, "ts": true, "sent_at": true, "created_at": true,
	"updated_at": true, "request_time": true, "reference": true, "request_ref": true,
}

// responseFields: names whose VALUES churn legitimately. Deliberately smaller,
// and a value-tracking suppression only — presence and type stay asserted.
var responseFields = map[string]bool{
	"request_id": true, "requestid": true, "request-id": true, "correlation_id": true,
	"trace_id": true, "traceid": true,
	"timestamp": true, "created_at": true, "updated_at": true, "processed_at": true,
	"etag": true, "nonce": true,
}

// requestHeaders / responseHeaders: wire header names (lowercase) for
// surfaces that compare headers (closest-miss diagnostics). Same asymmetry.
var requestHeaders = map[string]bool{
	"authorization": true, "x-request-id": true, "x-correlation-id": true,
	"traceparent": true, "tracestate": true, "b3": true, "x-b3-traceid": true,
	"x-b3-spanid": true, "x-b3-sampled": true, "x-datadog-trace-id": true,
	"x-datadog-parent-id": true, "x-amz-date": true, "x-amz-security-token": true,
	"x-amz-content-sha256": true, "idempotency-key": true, "x-idempotency-key": true,
	"x-signature": true, "x-hub-signature": true, "x-hub-signature-256": true,
	"stripe-signature": true, "x-slack-signature": true, "x-twilio-signature": true,
	"webhook-signature": true, "webhook-id": true, "webhook-timestamp": true,
	"x-csrf-token": true, "x-xsrf-token": true, "user-agent": true, "date": true,
	"cookie": true, "baggage": true, "sentry-trace": true,
}

var responseHeaders = map[string]bool{
	"date": true, "x-request-id": true, "x-correlation-id": true, "etag": true,
	"set-cookie": true, "cf-ray": true, "x-amzn-requestid": true, "x-amz-request-id": true,
	"x-runtime": true, "x-response-time": true, "server-timing": true,
}

// IsRequestField reports request-side field volatility (exact, case-insens).
func IsRequestField(name string) bool { return requestFields[strings.ToLower(name)] }

// IsResponseField reports response-side VALUE volatility (exact, case-insens).
func IsResponseField(name string) bool { return responseFields[strings.ToLower(name)] }

// IsResponseHeader reports wire-header volatility (exact, case-insens).
func IsResponseHeader(name string) bool { return responseHeaders[strings.ToLower(name)] }

// RequestFieldNames returns a copy of the request list (replay merges it
// into its hash-stripping set).
func RequestFieldNames() []string {
	out := make([]string, 0, len(requestFields))
	for n := range requestFields {
		out = append(out, n)
	}
	return out
}

// ResponseFieldNames returns a copy of the response list (the agent wires
// it into the learner's value-volatile set).
func ResponseFieldNames() []string {
	out := make([]string, 0, len(responseFields))
	for n := range responseFields {
		out = append(out, n)
	}
	return out
}
