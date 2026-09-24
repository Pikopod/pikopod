package archetype

type RoleMatch struct {
	Crud                  string `json:"crud,omitempty"`
	HasSuccessResponse    bool   `json:"hasSuccessResponse,omitempty"`
	HasErrorResponseClass string `json:"hasErrorResponseClass,omitempty"`
	RequiresAuth          bool   `json:"requiresAuth,omitempty"`
	HasEnumField          bool   `json:"hasEnumField,omitempty"`

	SameResourceAs string `json:"sameResourceAs,omitempty"`
}

type RoleRequirement struct {
	Role  string    `json:"role"`
	Bind  string    `json:"bind"`
	Match RoleMatch `json:"match"`
}

type Archetype struct {
	ID               string            `json:"id"`
	ArchetypeVersion int               `json:"archetypeVersion"`
	Title            string            `json:"title"`
	Description      string            `json:"description"`
	Expects          []string          `json:"expects"`
	Requires         []RoleRequirement `json:"requires"`
	RequiresFidelity string            `json:"requiresFidelity"`
	Expands          []map[string]any  `json:"expands"`
}
