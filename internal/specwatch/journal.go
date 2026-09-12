// The documented-changes journal records ADDITIVE declared changes so the
// OBSERVED-drift path can downgrade a matching traffic finding instead of paging.
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

// DocumentedChange is one declared additive change, keyed loosely enough to
// join against traffic findings (canonical positional templates).
type DocumentedChange struct {
	ID       string    `json:"id"` // specdiff check id
	Method   string    `json:"method"`
	Template string    `json:"template"`
	Path     string    `json:"path,omitempty"`  // response field path
	Value    string    `json:"value,omitempty"` // enum value or status code
	At       time.Time `json:"at"`
}

// Documented is the per-upstream journal of declared additive changes.
type Documented struct {
	Changes []DocumentedChange `json:"changes"`
}

// documentedCap bounds the journal (a hostile/churny spec must not grow it
// without bound; oldest entries fall off).
const documentedCap = 5000

// CanonicalTemplate reduces any template spelling to positional form — the
// same identity rule the IR's CanonicalPath uses.
func CanonicalTemplate(t string) string {
	segs := strings.Split(t, "/")
	for i, s := range segs {
		if strings.Contains(s, "{") {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// CanonicalFieldPath gives dot-joined (specdiff/overlay) and slash-joined
// (baseline learner) field paths one identity for the join.
func CanonicalFieldPath(p string) string {
	return strings.ReplaceAll(p, ".", "/")
}

// HasFieldAdded reports whether the journal documents this response field
// appearing.
func (d *Documented) HasFieldAdded(method, template, fieldPath string) bool {
	return d.has(func(c DocumentedChange) bool {
		return (c.ID == "response-property-added" || c.ID == "endpoint-added") &&
			c.Method == method && CanonicalTemplate(c.Template) == CanonicalTemplate(template) &&
			(c.ID == "endpoint-added" || CanonicalFieldPath(c.Path) == CanonicalFieldPath(fieldPath))
	})
}

// HasEnumValueAdded reports whether the journal documents this value joining
// the field's enum.
func (d *Documented) HasEnumValueAdded(method, template, fieldPath, value string) bool {
	return d.has(func(c DocumentedChange) bool {
		return c.ID == "response-enum-value-added" && c.Method == method &&
			CanonicalTemplate(c.Template) == CanonicalTemplate(template) &&
			CanonicalFieldPath(c.Path) == CanonicalFieldPath(fieldPath) && c.Value == value
	})
}

// HasStatusAdded reports whether the journal documents a new status; exact
// codes match directly, and a code documents its class ("429" ⇒ "4xx").
func (d *Documented) HasStatusAdded(method, template, statusOrClass string) bool {
	return d.has(func(c DocumentedChange) bool {
		if c.ID != "response-status-added" || c.Method != method ||
			CanonicalTemplate(c.Template) != CanonicalTemplate(template) {
			return false
		}
		if c.Value == statusOrClass {
			return true
		}
		// Class match: declared "429" documents observed class "4xx".
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

// LoadDocumented reads the journal (nil-safe: absent file → empty journal).
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

// journalDocumented appends the additive declared changes among findings to
// the upstream's journal (deduped; capped oldest-out).
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
