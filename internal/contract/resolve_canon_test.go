package contract

import (
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

func canonDef(templates ...string) *ir.ApiDefinition {
	def := &ir.ApiDefinition{}
	for _, tpl := range templates {
		def.Endpoints = append(def.Endpoints, ir.Endpoint{
			ID:           "GET " + tpl,
			Method:       ir.Explicit("GET", ""),
			PathTemplate: ir.Explicit(tpl, ""),
		})
	}
	return def
}

// Document order must not decide attribution: a literal segment is a closer
// match than a parameter, whichever the spec lists first.
func TestCanonicalPrefersLiteralOverParam(t *testing.T) {
	canon := newTemplateCanon(canonDef("/v1/charges/{charge}", "/v1/charges/search"))
	if got := canon.canonical("GET", "/v1/charges/search"); got != "/v1/charges/search" {
		t.Fatalf("search traffic attributed to %q", got)
	}
}

func TestCanonicalOrderIndependent(t *testing.T) {
	a := newTemplateCanon(canonDef("/v1/charges/{charge}", "/v1/charges/search"))
	b := newTemplateCanon(canonDef("/v1/charges/search", "/v1/charges/{charge}"))
	for _, traffic := range []string{"/v1/charges/search", "/v1/charges/{charge}"} {
		if a.canonical("GET", traffic) != b.canonical("GET", traffic) {
			t.Fatalf("%s canonicalises differently depending on spec order: %q vs %q",
				traffic, a.canonical("GET", traffic), b.canonical("GET", traffic))
		}
	}
}

// Specificity ordering must not start inventing matches.
func TestCanonicalUnknownFallsThrough(t *testing.T) {
	canon := newTemplateCanon(canonDef("/v1/charges/{charge}", "/v1/charges/search"))
	if got := canon.canonical("GET", "/v1/charges/xyz"); got != "/v1/charges/{charge}" {
		t.Fatalf("an id segment should match the parameter template, got %q", got)
	}
	if got := canon.canonical("GET", "/v1/unknown/thing"); got != "/v1/unknown/thing" {
		t.Fatalf("an unknown path must pass through unchanged, got %q", got)
	}
}
