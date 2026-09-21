package specdiff

import "fmt"

type Scope string

const (
	Guaranteed Scope = "guaranteed"
	Optional   Scope = "optional"
)

type Check struct {
	ID        string
	Variant   string
	Effect    Effect
	Direction Direction
	Scope     Scope
	Tolerated bool
}

var Checks = []Check{
	{ID: "endpoint-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "endpoint-added", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "endpoint-deprecated", Effect: Unchanged, Direction: Request, Scope: Guaranteed},
	{ID: "param-removed", Effect: Narrows, Direction: Request, Scope: Optional},
	{ID: "param-renamed", Effect: Unchanged, Direction: Request, Scope: Guaranteed},
	{ID: "param-became-required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "param-became-optional", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "param-added-required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "param-added-optional", Effect: Widens, Direction: Request, Scope: Optional},
	{ID: "request-body-added-required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "request-body-added-optional", Effect: Widens, Direction: Request, Scope: Optional},
	{ID: "request-body-removed", Variant: "required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "request-body-removed", Variant: "optional", Effect: Narrows, Direction: Request, Scope: Optional},
	{ID: "request-body-became-required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "request-body-became-optional", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-status-removed", Variant: "success", Effect: Narrows, Direction: Response, Scope: Guaranteed},
	{ID: "response-status-removed", Variant: "error", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "response-status-added", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-media-type-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "response-media-type-removed", Effect: Narrows, Direction: Response, Scope: Guaranteed},
	{ID: "request-media-type-added", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-media-type-added", Effect: Widens, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "endpoint-security-added", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "endpoint-security-scheme-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "endpoint-security-removed", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "auth-scheme-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "auth-scheme-changed", Effect: Incomparable, Direction: Request, Scope: Guaranteed},
	{ID: "auth-scheme-added", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "schema-restructured", Effect: Incomparable, Direction: Response, Scope: Guaranteed},
	{ID: "request-variant-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "response-variant-removed", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "request-variant-added", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-variant-added", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-type-changed", Variant: "incomparable", Effect: Incomparable, Direction: Request, Scope: Guaranteed},
	{ID: "request-type-changed", Variant: "narrows", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "request-type-changed", Variant: "widens", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-type-changed", Variant: "incomparable", Effect: Incomparable, Direction: Response, Scope: Guaranteed},
	{ID: "response-type-changed", Variant: "narrows", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "response-type-changed", Variant: "widens", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-nullable-added", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-nullable-added", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-nullable-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "response-nullable-removed", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "request-enum-closed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "response-enum-closed", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "request-enum-opened", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-enum-opened", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-enum-value-removed", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "response-enum-value-removed", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "request-enum-value-added", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-enum-value-added", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-property-removed", Effect: Narrows, Direction: Request, Scope: Optional},
	{ID: "response-required-property-removed", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "response-property-removed", Effect: Narrows, Direction: Response, Scope: Optional},
	{ID: "request-property-became-required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "response-property-became-required", Effect: Narrows, Direction: Response, Scope: Guaranteed, Tolerated: true},
	{ID: "request-property-became-optional", Effect: Widens, Direction: Request, Scope: Guaranteed},
	{ID: "response-property-became-optional", Effect: Widens, Direction: Response, Scope: Guaranteed},
	{ID: "request-property-added-required", Effect: Narrows, Direction: Request, Scope: Guaranteed},
	{ID: "request-property-added-optional", Effect: Widens, Direction: Request, Scope: Optional},
	{ID: "response-property-added", Effect: Widens, Direction: Response, Scope: Guaranteed, Tolerated: true},
}

var checkIndex = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range Checks {
		m[c.ID] = true
	}
	return m
}()

func CheckIDs() []string {
	out := make([]string, 0, len(checkIndex))
	seen := map[string]bool{}
	for _, c := range Checks {
		if !seen[c.ID] {
			seen[c.ID] = true
			out = append(out, c.ID)
		}
	}
	return out
}

func mustCheck(id string) string {
	if !checkIndex[id] {
		panic(fmt.Sprintf("specdiff: check %q is not in the inventory", id))
	}
	return id
}
