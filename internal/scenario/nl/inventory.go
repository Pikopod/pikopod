// Package nl is natural-language scenario authoring: the model NEVER emits steps,
// only a grounded INTENT the archetype expander deterministically expands.
package nl

import (
	"regexp"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
)

// InventoryOperation is one operation the intent may bind.
type InventoryOperation struct {
	ID      string  `json:"id"`
	Method  string  `json:"method"`
	Path    string  `json:"path"`
	Summary *string `json:"summary"`
}

// InventoryArchetypeRole mirrors the archetype role summary in the inventory.
type InventoryArchetypeRole struct {
	Role string `json:"role"`
	Bind string `json:"bind"`
}

// InventoryArchetype is one applicable archetype offered to the model.
type InventoryArchetype struct {
	ID       string                   `json:"id"`
	Expects  []string                 `json:"expects"`
	Requires []InventoryArchetypeRole `json:"requires"`
}

// Inventory is the grounded firewall a hallucinating model cannot cross:
// derived structurally from the pinned ApiDefinition, never from model output.
type Inventory struct {
	Operations       []InventoryOperation `json:"operations"`
	Resources        []string             `json:"resources"`
	WebhookEvents    []string             `json:"webhookEvents"`
	DocumentedErrors []string             `json:"documentedErrors"` // e.g. "createOrder:409"
	Archetypes       []InventoryArchetype `json:"archetypes"`
}

var errorStatusRe = regexp.MustCompile(`^[45]`)

func BuildInventory(apiDef *ir.ApiDefinition, archetypes []archetype.Archetype) *Inventory {
	inv := &Inventory{
		Operations:       []InventoryOperation{},
		Resources:        []string{},
		WebhookEvents:    []string{},
		DocumentedErrors: []string{},
		Archetypes:       []InventoryArchetype{},
	}
	resourceSet := map[string]bool{}
	for i := range apiDef.Endpoints {
		e := &apiDef.Endpoints[i]
		id := e.ID
		if e.OperationID != nil {
			id = e.OperationID.Value
		}
		var summary *string
		if e.Summary != nil {
			s := e.Summary.Value
			summary = &s
		}
		inv.Operations = append(inv.Operations, InventoryOperation{
			ID:      id,
			Method:  strings.ToUpper(e.Method.Value),
			Path:    e.PathTemplate.Value,
			Summary: summary,
		})
		resourceSet[scenario.ResourceTypeOf(e)] = true
	}
	for r := range resourceSet {
		inv.Resources = append(inv.Resources, r)
	}
	sort.Strings(inv.Resources)

	eventSet := map[string]bool{}
	for i := range apiDef.Webhooks {
		eventSet[apiDef.Webhooks[i].Event.Value] = true
	}
	for e := range eventSet {
		inv.WebhookEvents = append(inv.WebhookEvents, e)
	}
	sort.Strings(inv.WebhookEvents)

	errSet := map[string]bool{}
	for i := range apiDef.Endpoints {
		e := &apiDef.Endpoints[i]
		opID := e.ID
		if e.OperationID != nil {
			opID = e.OperationID.Value
		}
		for j := range e.Responses {
			s := e.Responses[j].StatusCode
			if errorStatusRe.MatchString(s) {
				errSet[opID+":"+s] = true
			}
		}
	}
	for e := range errSet {
		inv.DocumentedErrors = append(inv.DocumentedErrors, e)
	}
	sort.Strings(inv.DocumentedErrors)

	for i := range archetypes {
		a := &archetypes[i]
		roles := make([]InventoryArchetypeRole, 0, len(a.Requires))
		for _, r := range a.Requires {
			roles = append(roles, InventoryArchetypeRole{Role: r.Role, Bind: r.Bind})
		}
		inv.Archetypes = append(inv.Archetypes, InventoryArchetype{ID: a.ID, Expects: a.Expects, Requires: roles})
	}
	return inv
}

// KnownBindingRefs is the set of identifiers an intent's bindings may name.
func KnownBindingRefs(inv *Inventory) map[string]bool {
	out := map[string]bool{}
	for _, o := range inv.Operations {
		out[o.ID] = true
	}
	for _, e := range inv.WebhookEvents {
		out[e] = true
	}
	return out
}
