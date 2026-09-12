package pathtmpl

import (
	"fmt"
	"testing"
)

// Golden classification over realistic provider paths — the shapes payment
// APIs actually serve, including tokenized forms (recorder output).
func TestTemplatizeGoldens(t *testing.T) {
	cases := map[string]string{
		"/transaction/tx_8f3a91b2c4d5":                    "/transaction/tx_{id}",
		"/transaction/tx_mq4kf8zu2n1p":                    "/transaction/tx_{id}", // tokenized form, same template
		"/customers/cus_9s6XKzkNRiz8i3/payment_methods":   "/customers/cus_{id}/payment_methods",
		"/v1/charges/ch_3MtwBwLkdIwHu7ix0snN0B15/refunds": "/v1/charges/ch_{id}/refunds",
		"/transfers/550e8400-e29b-41d4-a716-446655440000": "/transfers/{uuid}",
		"/payouts/2024-06-01":                             "/payouts/{date}",
		"/payouts/2024-06-01T10:00:00Z":                   "/payouts/{date}",
		"/bank/resolve/0690000031":                        "/bank/resolve/{id}",
		"/balance":                                        "/balance",
		"/v2/checkout/sessions":                           "/v2/checkout/sessions",
		"/statements/9f8e7d6c5b4a3210":                    "/statements/{id}",
		"/refunds/RF20240601XYZ":                          "/refunds/{id}",
	}
	for in, want := range cases {
		if got := Templatize(in); got != want {
			t.Errorf("Templatize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Cardinality guard: a bare-word id scheme (classifies static) must be
// force-promoted once distinct values pass the threshold — the "endpoint
// family won't converge" killer.
func TestGuardPromotesRunawayPosition(t *testing.T) {
	g := NewGuard(10)
	// "summary" is a real static route seen repeatedly BEFORE the blowup —
	// frequency-pinning must keep it static after promotion.
	for i := 0; i < 3; i++ {
		g.Apply("/orders/summary")
	}
	var promo *Promotion
	for i := 0; i < 12; i++ {
		// /orders/customerworda... word-like ids that dodge classification
		_, p := g.Apply(fmt.Sprintf("/orders/customerword%c/items", 'a'+rune(i)))
		if p != nil {
			promo = p
		}
	}
	if promo == nil {
		t.Fatal("expected promotion after exceeding threshold")
	}
	got, _ := g.Apply("/orders/anotherword/items")
	if got != "/orders/{id}/items" {
		t.Fatalf("post-promotion template = %q, want /orders/{id}/items", got)
	}
	// The pinned literal survives the promotion.
	tpl, _ := g.Apply("/orders/summary")
	if tpl != "/orders/summary" {
		t.Fatalf("pinned static literal must survive promotion, got %q", tpl)
	}
}

func TestGuardMergeKey(t *testing.T) {
	g := NewGuard(2)
	g.Apply("/x/aaa")
	g.Apply("/x/bbb")
	g.Apply("/x/ccc") // promotion at position 2
	if got := g.MergeKey("/x/bbb"); got != "/x/{id}" {
		t.Fatalf("MergeKey should re-templatize old families, got %q", got)
	}
}
