package baseline

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Families is a read-only view of persisted baselines, for offline consumers
// (the replay CI gate, reports) that must not mutate learner state.
type FamilySet struct {
	byKey map[string]*Family
}

// LoadFamilies reads the persisted baselines for an upstream.
func LoadFamilies(dataDir, upstream string) (*FamilySet, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "baselines", upstream+".json"))
	if err != nil {
		return nil, err
	}
	var fams map[string]*Family
	if err := json.Unmarshal(raw, &fams); err != nil {
		return nil, err
	}
	return &FamilySet{byKey: fams}, nil
}

// Find returns the family for (method, template, statusClass), nil if absent.
func (fs *FamilySet) Find(method, template, statusClass string) *Family {
	if f, ok := fs.byKey[key(method, template, statusClass)]; ok {
		return f
	}
	// Persisted keys were written from the map keys; fall back to a scan in
	// case key formatting evolves.
	for _, f := range fs.byKey {
		if f.Method == method && f.Template == template && f.StatusClass == statusClass {
			return f
		}
	}
	return nil
}
