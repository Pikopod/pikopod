package specwatch

import (
	"encoding/json"
	"github.com/pikopod/pikopod/internal/store"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/specdiff"
)

type DocumentedChange struct {
	ID       string    `json:"id"`
	Method   string    `json:"method"`
	Template string    `json:"template"`
	Path     string    `json:"path,omitempty"`
	Value    string    `json:"value,omitempty"`
	At       time.Time `json:"at"`
}

type Documented struct {
	Changes []DocumentedChange `json:"changes"`
}

const documentedCap = 5000

func CanonicalTemplate(t string) string {
	segs := strings.Split(t, "/")
	for i, s := range segs {
		if strings.Contains(s, "{") {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

func CanonicalFieldPath(p string) string {
	return strings.ReplaceAll(p, ".", "/")
}

func (d *Documented) HasFieldAdded(method, template, fieldPath string) bool {
	return d.has(func(c DocumentedChange) bool {
		return (c.ID == "response-property-added" || c.ID == "endpoint-added") &&
			c.Method == method && CanonicalTemplate(c.Template) == CanonicalTemplate(template) &&
			(c.ID == "endpoint-added" || CanonicalFieldPath(c.Path) == CanonicalFieldPath(fieldPath))
	})
}

func (d *Documented) HasEnumValueAdded(method, template, fieldPath, value string) bool {
	return d.has(func(c DocumentedChange) bool {
		return c.ID == "response-enum-value-added" && c.Method == method &&
			CanonicalTemplate(c.Template) == CanonicalTemplate(template) &&
			CanonicalFieldPath(c.Path) == CanonicalFieldPath(fieldPath) && c.Value == value
	})
}

func (d *Documented) HasStatusAdded(method, template, statusOrClass string) bool {
	return d.has(func(c DocumentedChange) bool {
		if c.ID != "response-status-added" || c.Method != method ||
			CanonicalTemplate(c.Template) != CanonicalTemplate(template) {
			return false
		}
		if c.Value == statusOrClass {
			return true
		}

		return len(statusOrClass) == 3 && strings.HasSuffix(statusOrClass, "xx") &&
			len(c.Value) == 3 && c.Value[0] == statusOrClass[0]
	})
}

func (d *Documented) has(match func(DocumentedChange) bool) bool {
	if d == nil {
		return false
	}
	for _, c := range d.Changes {
		if match(c) {
			return true
		}
	}
	return false
}

func documentedPath(dataDir, upstream string) string {
	return filepath.Join(dataDir, "specwatch", upstream+".documented.json")
}

func LoadDocumented(dataDir, upstream string) *Documented {
	raw, err := os.ReadFile(documentedPath(dataDir, upstream))
	if err != nil {
		return &Documented{}
	}
	var d Documented
	if json.Unmarshal(raw, &d) != nil {
		return &Documented{}
	}
	return &d
}

func (w *Watcher) journalDocumented(upstream string, findings []specdiff.Finding, now time.Time) {
	var adds []DocumentedChange
	for _, f := range findings {
		c := DocumentedChange{ID: f.ID, Method: f.Method, Template: f.Template, At: now}
		switch f.ID {
		case "endpoint-added":
		case "response-property-added":
			if len(f.Args) < 2 {
				continue
			}
			c.Path = f.Args[1]
		case "response-enum-value-added":
			if len(f.Args) < 3 {
				continue
			}
			c.Path, c.Value = f.Args[1], f.Args[2]
		case "response-status-added":
			if len(f.Args) < 1 {
				continue
			}
			c.Value = f.Args[0]
		default:
			continue
		}
		adds = append(adds, c)
	}
	if len(adds) == 0 {
		return
	}
	d := LoadDocumented(w.dataDir, upstream)
	seen := map[string]bool{}
	for _, c := range d.Changes {
		seen[c.ID+"\x00"+c.Method+"\x00"+c.Template+"\x00"+c.Path+"\x00"+c.Value] = true
	}
	for _, c := range adds {
		k := c.ID + "\x00" + c.Method + "\x00" + c.Template + "\x00" + c.Path + "\x00" + c.Value
		if !seen[k] {
			d.Changes = append(d.Changes, c)
			seen[k] = true
		}
	}
	if over := len(d.Changes) - documentedCap; over > 0 {
		d.Changes = d.Changes[over:]
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	_ = store.WriteFileAtomic(documentedPath(w.dataDir, upstream), raw)
}
