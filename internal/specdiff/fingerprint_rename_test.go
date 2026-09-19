package specdiff

import "testing"

// A provider renaming {charge} to {charge_id} changes nothing a consumer can
// observe, so every acknowledged finding on the endpoint must keep its key.
func TestFingerprintStableAcrossPathParamRename(t *testing.T) {
	a := Finding{ID: "response-required-property-removed", Method: "GET", Template: "/v1/charges/{charge}", Args: []string{"amount"}}
	b := a
	b.Template = "/v1/charges/{charge_id}"
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("a path-parameter rename minted a new fingerprint:\n  %s\n  %s", a.Fingerprint(), b.Fingerprint())
	}
}

func TestFingerprintDistinguishesDifferentPaths(t *testing.T) {
	base := Finding{ID: "response-required-property-removed", Method: "GET", Args: []string{"amount"}}
	charges, refunds, search := base, base, base
	charges.Template = "/v1/charges/{id}"
	refunds.Template = "/v1/refunds/{id}"
	search.Template = "/v1/charges/search"
	if charges.Fingerprint() == refunds.Fingerprint() {
		t.Fatal("different resources collided")
	}
	if charges.Fingerprint() == search.Fingerprint() {
		t.Fatal("a literal segment collided with a parameter segment")
	}
}

// End to end: the same removed property produces the same fingerprint whether
// or not the path parameter was renamed alongside it.
func TestRequiredPropertyRemovedFingerprintSurvivesParamRename(t *testing.T) {
	full := with200(objSchema(prop("amount", true, strSchema())))
	trimmed := with200(objSchema())

	same := Diff(def(ep("GET", "/v1/charges/{charge}", full)), def(ep("GET", "/v1/charges/{charge}", trimmed)))
	renamed := Diff(def(ep("GET", "/v1/charges/{charge}", full)), def(ep("GET", "/v1/charges/{charge_id}", trimmed)))

	want := find(t, same, "response-required-property-removed").Fingerprint()
	got := find(t, renamed, "response-required-property-removed").Fingerprint()
	if got != want {
		t.Fatalf("fingerprint moved with the parameter name: %s vs %s", got, want)
	}
}
