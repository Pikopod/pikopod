package fix

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/errfmt"
)

const (
	maxWalkFiles      = 20000
	maxFileBytes      = 1 << 20
	maxImpactFiles    = 12
	maxMatchesPerFile = 5
	contextLines      = 4
	maxEdits          = 20
	maxEditBytes      = 8 << 10
)

var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"out": true, "target": true, "venv": true, "__pycache__": true,
	"testdata": true, "pikopod-data": true, "coverage": true,
}

var sourceExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true, ".py": true, ".rb": true, ".java": true,
	".kt": true, ".cs": true, ".php": true, ".rs": true, ".swift": true,
	".scala": true, ".ex": true, ".exs": true, ".dart": true,
}

type Impact struct {
	File    string `json:"file"`
	Excerpt string `json:"excerpt"`
}

func Terms(ev *alert.DriftEvent) []string {
	var terms []string
	if ev.Field != "" {
		segs := strings.FieldsFunc(ev.Field, func(r rune) bool { return r == '/' || r == '.' })
		if len(segs) > 0 {
			leaf := segs[len(segs)-1]
			if len(leaf) >= 3 {
				terms = append(terms, leaf)
			}
		}
	}
	if len(terms) == 0 {
		for _, seg := range strings.Split(ev.Endpoint, "/") {
			if len(seg) >= 4 && !strings.ContainsAny(seg, "{}") {
				terms = append(terms, seg)
			}
		}
	}
	return terms
}

func Scan(root string, terms []string) ([]Impact, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	var paths []string
	walked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		walked++
		if walked > maxWalkFiles {
			return fs.SkipAll
		}
		if !sourceExts[filepath.Ext(name)] {
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > maxFileBytes {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	var impacts []Impact
	for _, path := range paths {
		if len(impacts) >= maxImpactFiles {
			break
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		content := string(raw)
		hit := false
		for _, t := range terms {
			if strings.Contains(content, t) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		rel, rerr2 := filepath.Rel(root, path)
		if rerr2 != nil {
			continue
		}
		impacts = append(impacts, Impact{
			File:    filepath.ToSlash(rel),
			Excerpt: excerpt(content, terms),
		})
	}
	return impacts, nil
}

func excerpt(content string, terms []string) string {
	lines := strings.Split(content, "\n")
	var regions [][2]int
	matches := 0
	for i, line := range lines {
		if matches >= maxMatchesPerFile {
			break
		}
		for _, t := range terms {
			if strings.Contains(line, t) {
				lo, hi := i-contextLines, i+contextLines
				if lo < 0 {
					lo = 0
				}
				if hi >= len(lines) {
					hi = len(lines) - 1
				}
				if n := len(regions); n > 0 && lo <= regions[n-1][1]+1 {
					regions[n-1][1] = hi
				} else {
					regions = append(regions, [2]int{lo, hi})
				}
				matches++
				break
			}
		}
	}
	var b strings.Builder
	for ri, r := range regions {
		if ri > 0 {
			b.WriteString("…\n")
		}
		for i := r[0]; i <= r[1]; i++ {
			fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
		}
	}
	return b.String()
}

type Edit struct {
	File    string `json:"file"`
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

type Proposal struct {
	Edits   []Edit `json:"edits"`
	Summary string `json:"summary"`
}

const Instruction = "You repair a developer's client code after a third-party API contract drift. " +
	"The payload contains the drift event (what changed in the provider's API) and numbered " +
	"excerpts from the impacted source files. Propose the SMALLEST code change that adapts the " +
	"client to the drifted contract. Respond with JSON: {\"edits\": [{\"file\": string, " +
	"\"find\": string, \"replace\": string}], \"summary\": string}. Each find must be copied " +
	"VERBATIM from an excerpt (exact bytes, without the line-number prefix and tab) and must be " +
	"unique within its file; each file must be one of the listed impacted files. If no safe " +
	"change exists, return {\"edits\": [], \"summary\": \"<why>\"}."

func ParseProposal(v any, impacts []Impact) (*Proposal, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errfmt.New("the model's fix was not a JSON object", "expected {edits, summary}", "retry, or try another llm.model", "docs/config-reference.md#llm")
	}
	allowed := map[string]bool{}
	for _, im := range impacts {
		allowed[im.File] = true
	}
	p := &Proposal{}
	if s, ok := obj["summary"].(string); ok {
		p.Summary = s
	}
	rawEdits, _ := obj["edits"].([]any)
	if len(rawEdits) > maxEdits {
		return nil, errfmt.Newf("the model proposed too many edits", "re-run; a drift fix should be small", "docs/config-reference.md#fix", "%d edits (max %d)", len(rawEdits), maxEdits)
	}
	for i, re := range rawEdits {
		eo, ok := re.(map[string]any)
		if !ok {
			return nil, errfmt.Newf("the model's edit list is malformed", "retry, or try another llm.model", "docs/config-reference.md#llm", "edit %d is not an object", i)
		}
		e := Edit{}
		e.File, _ = eo["file"].(string)
		e.Find, _ = eo["find"].(string)
		e.Replace, _ = eo["replace"].(string)
		if !allowed[e.File] {
			return nil, errfmt.Newf("the model edited a file outside the impact set", "this is refused by design — the scan bounds the blast radius", "docs/config-reference.md#fix", "file %q is not among the scanned impacts", e.File)
		}
		if e.Find == "" || e.Find == e.Replace {
			return nil, errfmt.New("the model produced an empty or no-op edit", "retry, or try another llm.model", "", "docs/config-reference.md#llm")
		}
		if len(e.Find) > maxEditBytes || len(e.Replace) > maxEditBytes {
			return nil, errfmt.New("the model's edit is oversized", "a drift fix should be a small change", "retry, or try another llm.model", "docs/config-reference.md#llm")
		}
		p.Edits = append(p.Edits, e)
	}
	return p, nil
}

func Apply(root string, edits []Edit) (changed []string, revert func() error, err error) {
	type backup struct {
		path string
		data []byte
		mode os.FileMode
	}
	var backups []backup
	restore := func() error {
		var first error
		for _, b := range backups {
			if werr := os.WriteFile(b.path, b.data, b.mode); werr != nil && first == nil {
				first = werr
			}
		}
		return first
	}

	files := map[string]string{}
	order := []string{}
	for _, e := range edits {
		abs := filepath.Join(root, filepath.FromSlash(e.File))
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil || strings.HasPrefix(rel, "..") {
			return nil, nil, errfmt.Newf("edit escapes the repository root", "refused by design", "docs/config-reference.md#fix", "path %q", e.File)
		}
		content, seen := files[abs]
		if !seen {
			raw, rerr2 := os.ReadFile(abs)
			if rerr2 != nil {
				return nil, nil, errfmt.Newf("cannot read the file to edit", "the impact scan saw it; check permissions", "", "%v", rerr2)
			}
			content = string(raw)
			order = append(order, abs)
		}
		if n := strings.Count(content, e.Find); n != 1 {
			return nil, nil, errfmt.Newf("an edit's find text is not unique in "+e.File, "the model must anchor on unique text; re-run to redraft", "docs/config-reference.md#fix", "%d occurrences (need exactly 1)", n)
		}
		files[abs] = strings.Replace(content, e.Find, e.Replace, 1)
	}

	for _, abs := range order {
		info, serr := os.Stat(abs)
		if serr != nil {
			restore()
			return nil, nil, serr
		}
		orig, rerr := os.ReadFile(abs)
		if rerr != nil {
			restore()
			return nil, nil, rerr
		}
		backups = append(backups, backup{path: abs, data: orig, mode: info.Mode().Perm()})
		if werr := os.WriteFile(abs, []byte(files[abs]), info.Mode().Perm()); werr != nil {
			restore()
			return nil, nil, werr
		}
		rel, _ := filepath.Rel(root, abs)
		changed = append(changed, filepath.ToSlash(rel))
	}
	return changed, restore, nil
}
