package sanitize

import "testing"

var (
	benchmarkSanitizeResult Result
	benchmarkClassifyMode   Mode
	benchmarkToken          string
	benchmarkTokenFormat    TokenFormat
)

func benchmarkTokenizer() *Tokenizer {
	return NewTokenizer("benchmark-master-key-0123456789", "local", 1)
}

func benchmarkFlatPayload() map[string]any {
	return map[string]any{
		"status":         "pending",
		"amount":         float64(125000),
		"currency":       "ngn",
		"attempts":       float64(2),
		"captured":       true,
		"user_id":        "usr_7f31a9c2",
		"customer_email": "customer@example.com",
		"transaction_id": "550e8400-e29b-41d4-a716-446655440000",
		"api_key":        "xpay_secret_BENCHMARK0000000000000000",
		"password":       "correct-horse-battery-staple",
		"description":    "Customer requested expedited settlement",
		"metadata":       "merchant-specific free text that is not replay-safe",
		"unexpected":     "unclassified payload content",
	}
}

func benchmarkNestedPayload() map[string]any {
	return map[string]any{
		"order_id": "ord_8c12f0ab",
		"status":   "completed",
		"customer": map[string]any{
			"user_id": "usr_1a2b3c4d",
			"email":   "nested@example.com",
			"profile": map[string]any{
				"tier":   "gold",
				"region": "ng",
			},
		},
		"payment": map[string]any{
			"amount":   float64(87500),
			"currency": "ngn",
			"api_key":  "xpay_public_BENCHMARK0000000000000000",
			"card": map[string]any{
				"card_number": "4111111111111111",
				"expiry":      "12",
			},
		},
		"items": []any{
			map[string]any{
				"product_id": "prod_1234abcd",
				"quantity":   float64(2),
				"name":       "Wireless keyboard",
			},
			map[string]any{
				"product_id": "prod_9876wxyz",
				"quantity":   float64(1),
				"notes":      "Gift wrap requested for recipient",
			},
		},
	}
}

func BenchmarkSanitize(b *testing.B) {
	tok := benchmarkTokenizer()
	payload := benchmarkFlatPayload()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSanitizeResult = Sanitize(payload, tok, nil, false)
	}
}

func BenchmarkSanitizeNested(b *testing.B) {
	tok := benchmarkTokenizer()
	payload := benchmarkNestedPayload()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSanitizeResult = Sanitize(payload, tok, nil, false)
	}
}

func BenchmarkClassify(b *testing.B) {
	cases := []struct {
		name     string
		key      string
		value    any
		isHeader bool
	}{
		{name: "allowed", key: "status", value: "pending"},
		{name: "identifier", key: "user_id", value: "usr_7f31a9c2"},
		{name: "secret", key: "api_key", value: "xpay_secret_BENCHMARK0000000000000000"},
		{name: "free-text", key: "description", value: "a free-form customer message"},
		{name: "safe-header", key: "content-type", value: "application/json", isHeader: true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkClassifyMode = Classify(tc.key, tc.value, tc.isHeader)
			}
		})
	}
}

func BenchmarkTokenizerTokenize(b *testing.B) {
	tok := benchmarkTokenizer()
	values := []string{
		"customer@example.com",
		"usr_7f31a9c2",
		"550e8400-e29b-41d4-a716-446655440000",
		"1234567890123456",
		"opaqueValue123",
		"free text with spaces",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkToken, benchmarkTokenFormat = tok.Tokenize(values[i%len(values)])
	}
}
