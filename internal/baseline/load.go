package baseline

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type FamilySet struct {
	byKey map[string]*Family
}

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

func (fs *FamilySet) Find(method, template, statusClass string) *Family {
	if f, ok := fs.byKey[key(method, template, statusClass)]; ok {
		return f
	}

	for _, f := range fs.byKey {
		if f.Method == method && f.Template == template && f.StatusClass == statusClass {
			return f
		}
	}
	return nil
}
