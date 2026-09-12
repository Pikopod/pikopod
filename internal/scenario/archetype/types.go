// Package archetype is the parameterized pattern language: catalogue, binder,
// and expander. Behaviour is pinned by committed goldens over real IR.
package archetype

// RoleMatch selects one operation for a role.
type RoleMatch struct {
	Crud                  string `json:"crud,omitempty"` // CREATE READ UPDATE DELETE LIST
	HasSuccessResponse    bool   `json:"hasSuccessResponse,omitempty"`
	HasErrorResponseClass string `json:"hasErrorResponseClass,omitempty"` // 4XX | 5XX
	RequiresAuth          bool   `json:"requiresAuth,omitempty"`
	HasEnumField          bool   `json:"hasEnumField,omitempty"`
	// SameResourceAs constrains to an operation on the same resource as an
	// earlier role.
	SameResourceAs string `json:"sameResourceAs,omitempty"`
}

type RoleRequirement struct {
	Role  string    `json:"role"`
	Bind  string    `json:"bind"` // operation | webhookEvent
	Match RoleMatch `json:"match"`
}

// Archetype is one parameterized pattern. Expands carries RAW step templates
// with `<<role.field>>` tokens, so expansion output stays structurally stable.
type Archetype struct {
	ID               string            `json:"id"`
	ArchetypeVersion int               `json:"archetypeVersion"`
	Title            string            `json:"title"`
	Description      string            `json:"description"`
	Expects          []string          `json:"expects"`
	Requires         []RoleRequirement `json:"requires"`
	RequiresFidelity string            `json:"requiresFidelity"` // L1 | L2 | L3
	Expands          []map[string]any  `json:"expands"`
}
