package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/sanitize"
)

func TestQueryKeysSanitized(t *testing.T) {
	rec := NewRecorder(t.TempDir(), sanitize.NewTokenizer("test-salt-0123456789abcdef", "t", 1), &Metrics{})
	redactions := 0
	out := rec.sanitizePath("/verify?tok_live_abc12345=1&customerId=cus_9zK21abc&page=2", &redactions)

	if strings.Contains(out, "tok_live_abc12345") {
		t.Fatalf("credential-shaped KEY persisted raw: %s", out)
	}
	if !strings.Contains(out, "customerId=") {
		t.Fatalf("ordinary camelCase key must survive untouched: %s", out)
	}
	if !strings.Contains(out, "page=") {
		t.Fatalf("benign key altered: %s", out)
	}
	if strings.Contains(out, "cus_9zK21abc") {
		t.Fatalf("prefixed-id VALUE must still tokenize: %s", out)
	}
	if redactions < 2 {
		t.Fatalf("redactions counted: %d (%s)", redactions, out)
	}

	redactions = 0
	out = rec.sanitizePath("/cb?tok_live_zz9x8y7w6v", &redactions)
	if strings.Contains(out, "tok_live_zz9x8y7w6v") || redactions == 0 {
		t.Fatalf("bare credential key persisted: %s (redactions %d)", out, redactions)
	}
}

func TestLargeIntegerPrecisionPreserved(t *testing.T) {
	rec := NewRecorder(t.TempDir(), sanitize.NewTokenizer("test-salt-0123456789abcdef", "t", 1), &Metrics{})
	body := []byte(`{"amount_minor":90071992547409934,"currency":"NGN"}`)
	h := make(map[string][]string)
	h["Content-Type"] = []string{"application/json"}
	record, err := rec.sanitizeExchange(&Exchange{
		Upstream: "pay", Method: "POST", Path: "/charge", Status: 200,
		RespHeader: h, RespBody: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(record.RespBody)
	if !strings.Contains(string(out), "90071992547409934") {
		t.Fatalf("17-digit amount lost precision through float64: %s", out)
	}
}

func TestPaymentPartyNamesDropped(t *testing.T) {
	rec := NewRecorder(t.TempDir(), sanitize.NewTokenizer("test-salt-0123456789abcdef", "t", 1), &Metrics{})
	body := []byte(`{"beneficiary":"olumide","sender":"adaeze","status":"pending"}`)
	h := map[string][]string{"Content-Type": {"application/json"}}
	record, err := rec.sanitizeExchange(&Exchange{
		Upstream: "pay", Method: "POST", Path: "/transfer", Status: 200,
		RespHeader: h, RespBody: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(record.RespBody)
	for _, raw := range []string{"olumide", "adaeze"} {
		if strings.Contains(string(out), raw) {
			t.Fatalf("payment-party name persisted raw: %s", out)
		}
	}
	if !strings.Contains(string(out), "pending") {
		t.Fatalf("enum-ish status must survive: %s", out)
	}
}
