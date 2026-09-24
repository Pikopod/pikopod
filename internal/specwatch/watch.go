package specwatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pikopod/pikopod/internal/store"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/specdiff"
)

type Source struct {
	Upstream   string
	SpecSource string

	Pinned *ir.ApiDefinition
}

type Reporter func(upstream string, f specdiff.Finding)

type Result struct {
	Upstream string
	Checked  bool
	Changed  bool
	Findings int
	Err      error
}

type sourceState struct {
	ETag        string    `json:"etag,omitempty"`
	Hash        string    `json:"hash,omitempty"`
	LastChecked time.Time `json:"last_checked"`
	LastChanged time.Time `json:"last_changed,omitempty"`
	SpecVersion string    `json:"spec_version,omitempty"`
	Findings    int       `json:"findings,omitempty"`

	LastError string `json:"last_error,omitempty"`
}

type FetchFunc func(source, etag string) (raw []byte, newETag string, notModified bool, err error)

type Watcher struct {
	mu       sync.Mutex
	dataDir  string
	sources  []Source
	interval time.Duration
	report   Reporter
	fetch    FetchFunc
	now      func() time.Time
	state    map[string]*sourceState
	checking atomic.Bool
}

func New(dataDir string, sources []Source, interval time.Duration, report Reporter) *Watcher {
	w := &Watcher{
		dataDir:  dataDir,
		sources:  sources,
		interval: interval,
		report:   report,
		fetch:    Fetch,
		now:      time.Now,
		state:    map[string]*sourceState{},
	}
	w.load()
	return w
}

func (w *Watcher) SetFetch(f FetchFunc)          { w.fetch = f }
func (w *Watcher) SetClock(now func() time.Time) { w.now = now }

type SourceHealth struct {
	Upstream    string    `json:"upstream"`
	LastChecked time.Time `json:"last_checked"`
	LastChanged time.Time `json:"last_changed,omitempty"`
	SpecVersion string    `json:"spec_version,omitempty"`
	Findings    int       `json:"findings"`
	LastError   string    `json:"last_error,omitempty"`
}

func (w *Watcher) Health() []SourceHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]SourceHealth, 0, len(w.sources))
	for i := range w.sources {
		h := SourceHealth{Upstream: w.sources[i].Upstream}
		if st, ok := w.state[w.sources[i].Upstream]; ok {
			h.LastChecked, h.LastChanged = st.LastChecked, st.LastChanged
			h.SpecVersion, h.Findings, h.LastError = st.SpecVersion, st.Findings, st.LastError
		}
		out = append(out, h)
	}
	return out
}

func (w *Watcher) Check() []Result {
	if !w.checking.CompareAndSwap(false, true) {
		return nil
	}
	defer w.checking.Store(false)

	var results []Result
	for i := range w.sources {
		results = append(results, w.checkOne(&w.sources[i]))
	}
	w.persist()
	return results
}

func (w *Watcher) checkOne(s *Source) Result {
	res := Result{Upstream: s.Upstream}
	now := w.now()

	w.mu.Lock()
	st, ok := w.state[s.Upstream]
	if !ok {
		st = &sourceState{}
		w.state[s.Upstream] = st
	}
	due := st.LastChecked.IsZero() || now.Sub(st.LastChecked) >= w.interval
	etag, lastHash := st.ETag, st.Hash
	w.mu.Unlock()

	if !due {
		return res
	}
	res.Checked = true

	raw, newETag, notModified, err := w.fetch(s.SpecSource, etag)
	if err != nil {

		w.mu.Lock()
		st.LastChecked = now
		st.LastError = err.Error()
		w.mu.Unlock()
		res.Err = err
		return res
	}

	w.mu.Lock()
	st.LastChecked = now
	if notModified {
		st.LastError = ""
		w.mu.Unlock()
		return res
	}
	hash := hashOf(raw)
	sameBytes := hash == lastHash
	w.mu.Unlock()
	if sameBytes {

		w.mu.Lock()
		st.ETag, st.LastError = newETag, ""
		w.mu.Unlock()
		return res
	}

	def, err := importer.NormalizeOpenAPI(raw)
	if err != nil {

		w.mu.Lock()
		st.LastError = err.Error()
		w.mu.Unlock()
		res.Err = errfmt.Newf("watched spec for "+s.Upstream+" no longer parses",
			"the provider's published document at "+s.SpecSource+" failed normalization",
			"docs/config-reference.md#spec_watch", "%v", err)
		return res
	}

	w.mu.Lock()
	st.ETag, st.Hash, st.LastError = newETag, hash, ""
	w.mu.Unlock()

	pin := s.Pinned
	if pin == nil {
		pin = w.loadPin(s.Upstream)
	}
	if pin == nil {

		if err := w.savePin(s.Upstream, def); err != nil {
			res.Err = err
			return res
		}
		w.recordChange(s.Upstream, def, 0, now)
		return res
	}

	findings := specdiff.Diff(pin, def)
	res.Changed = true
	res.Findings = len(findings)
	w.journalDocumented(s.Upstream, findings, now)
	for _, f := range findings {
		w.report(s.Upstream, f)
	}
	w.recordChange(s.Upstream, def, len(findings), now)
	return res
}

func (w *Watcher) recordChange(upstream string, def *ir.ApiDefinition, findings int, now time.Time) {
	version := ""
	if def.Metadata.Version != nil {
		version = def.Metadata.Version.Value
	}
	w.mu.Lock()
	st := w.state[upstream]
	st.LastChanged = now
	st.SpecVersion = version
	st.Findings = findings
	w.mu.Unlock()
}

func pinPath(dataDir, upstream string) string {
	return filepath.Join(dataDir, "specwatch", upstream+".pin.json")
}

func (w *Watcher) loadPin(upstream string) *ir.ApiDefinition {
	raw, err := os.ReadFile(pinPath(w.dataDir, upstream))
	if err != nil {
		return nil
	}
	var def ir.ApiDefinition
	if json.Unmarshal(raw, &def) != nil {
		return nil
	}
	return &def
}

func (w *Watcher) savePin(upstream string, def *ir.ApiDefinition) error {
	p := pinPath(w.dataDir, upstream)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(p, raw)
}

func ResetPin(dataDir, upstream string) error {
	statePath := filepath.Join(dataDir, "specwatch", "state.json")

	return store.WithFileLock(statePath, func() error { return resetPinLocked(dataDir, upstream, statePath) })
}

func resetPinLocked(dataDir, upstream, statePath string) error {
	if err := os.Remove(pinPath(dataDir, upstream)); err != nil && !os.IsNotExist(err) {
		return err
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return nil
	}
	var state map[string]*sourceState
	if json.Unmarshal(raw, &state) != nil {
		return nil
	}
	delete(state, upstream)
	out, err := json.MarshalIndent(state, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, out, 0o600)
}

func (w *Watcher) statePath() string {
	return filepath.Join(w.dataDir, "specwatch", "state.json")
}

func (w *Watcher) persist() {
	_ = store.WithFileLock(w.statePath(), func() error {
		w.mu.Lock()

		for name, st := range w.state {
			if st.Hash == "" {
				continue
			}
			if w.sourceHasOwnPin(name) {
				if _, err := os.Stat(pinPath(w.dataDir, name)); os.IsNotExist(err) {
					delete(w.state, name)
				}
			}
		}
		raw, err := json.MarshalIndent(w.state, "", " ")
		w.mu.Unlock()
		if err != nil {
			return err
		}
		return store.WriteFileAtomic(w.statePath(), raw)
	})
}

func (w *Watcher) sourceHasOwnPin(upstream string) bool {
	for i := range w.sources {
		if w.sources[i].Upstream == upstream {
			return w.sources[i].Pinned == nil
		}
	}
	return false
}

func (w *Watcher) load() {
	raw, err := os.ReadFile(w.statePath())
	if err != nil {
		return
	}
	var state map[string]*sourceState
	if json.Unmarshal(raw, &state) == nil && state != nil {
		w.state = state
	}
}

func hashOf(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

type Origin struct {
	Kind string
	Ref  string
	Path string
	Dir  string
}

func (o Origin) ImporterSource() *importer.Source {
	switch o.Kind {
	case "git":
		ref := o.Ref
		return &importer.Source{File: o.Path, Dir: path.Dir(o.Path), Load: func(rel string) ([]byte, error) {
			raw, err := gitShow(ref + ":" + rel)
			if err == nil && int64(len(raw)) > maxSpecBytes {
				return nil, errfmt.New("referenced spec file exceeds the size cap", rel+" is larger than 32 MiB", "split or trim the referenced file", "docs/config-reference.md#ref-policy")
			}
			return raw, err
		}}
	case "file":
		dir := o.Dir
		return &importer.Source{File: o.Path, Load: func(rel string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		}}
	}
	return &importer.Source{File: o.Path}
}

func OriginOf(source string) Origin {
	switch {
	case strings.HasPrefix(source, "git:"):
		ref, p, _ := strings.Cut(strings.TrimPrefix(source, "git:"), ":")
		return Origin{Kind: "git", Ref: ref, Path: path.Clean(p)}
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return Origin{Kind: "url", Path: source}
	default:
		return Origin{Kind: "file", Path: source, Dir: filepath.Dir(source)}
	}
}

func Fetch(source, etag string) ([]byte, string, bool, error) {
	switch {
	case strings.HasPrefix(source, "git:"):
		raw, err := gitShow(strings.TrimPrefix(source, "git:"))
		if err == nil && int64(len(raw)) > maxSpecBytes {
			return nil, "", false, errfmt.New("watched spec exceeds the size cap", source+" is larger than 32 MiB", "check spec_source", "docs/config-reference.md#spec_watch")
		}
		return raw, "", false, err
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return fetchHTTP(source, etag)
	default:
		raw, err := os.ReadFile(source)
		if err != nil {
			return nil, "", false, errfmt.Newf("cannot read watched spec file", "check spec_source in pikopod.yaml", "docs/config-reference.md#spec_watch", "%v", err)
		}
		if int64(len(raw)) > maxSpecBytes {
			return nil, "", false, errfmt.New("watched spec exceeds the size cap", source+" is larger than 32 MiB", "check spec_source", "docs/config-reference.md#spec_watch")
		}
		return raw, "", false, nil
	}
}

func fetchHTTP(source, etag string) ([]byte, string, bool, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errfmt.New("too many redirects fetching the watched spec", source+" redirected more than 3 times", "point spec_source at the final URL", "")
			}
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, errfmt.New("watched spec fetch failed",
			source+" answered "+resp.Status,
			"check spec_source serves the raw document", "docs/config-reference.md#spec_watch")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, "", false, err
	}
	if int64(len(raw)) > maxSpecBytes {

		return nil, "", false, errfmt.New("watched spec exceeds the size cap",
			fmt.Sprintf("%s served more than %d MiB", source, maxSpecBytes>>20),
			"a document this size is almost certainly not the spec; check spec_source", "docs/config-reference.md#spec_watch")
	}
	return raw, resp.Header.Get("ETag"), false, nil
}

const maxSpecBytes = 32 << 20

func gitShow(refspec string) ([]byte, error) {
	if !strings.Contains(refspec, ":") || strings.HasPrefix(refspec, "-") {
		return nil, errfmt.New("invalid git spec_source", "expected git:<ref>:<path>", "e.g. git:origin/main:openapi.yaml", "")
	}
	cmd := exec.Command("git", "show", refspec)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "dubious ownership") {
			return nil, errfmt.New("git refuses to read this checkout", "the working tree is owned by another user, which git treats as unsafe by default (common in CI containers)", "run `git config --global --add safe.directory \"$GITHUB_WORKSPACE\"` (or the checkout path) before pikopod", "docs/config-reference.md#ref-policy")
		}
		return nil, errfmt.New("git show failed for "+refspec, msg, "check the ref and path exist", "")
	}
	return out.Bytes(), nil
}
