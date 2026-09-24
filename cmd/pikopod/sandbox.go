package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/docimport"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/mode"
	"github.com/pikopod/pikopod/internal/replay"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario/nl"
	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/pikopod/pikopod/internal/specwatch"
	"github.com/pikopod/pikopod/internal/store"
	"github.com/spf13/cobra"
)

type sandboxEntry struct {
	ID string `json:"id"`

	Name string `json:"name"`
	Seed string `json:"seed"`
	Mode string `json:"mode"`

	CreatedClockMs int64 `json:"createdClockMs"`

	IRFile     string `json:"irFile"`
	SpecSource string `json:"specSource"`

	Origin string `json:"origin,omitempty"`

	Upstream string `json:"upstream,omitempty"`

	WebhookURL string `json:"webhookUrl,omitempty"`

	RecordingsFallback bool `json:"recordingsFallback,omitempty"`
}

func registryPath(dataDir string) string { return filepath.Join(dataDir, "sandboxes.json") }

func loadRegistry(dataDir string) ([]sandboxEntry, error) {
	raw, err := os.ReadFile(registryPath(dataDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errfmt.Newf("cannot read sandbox registry", "check permissions on "+registryPath(dataDir), "docs/config-reference.md#data_dir", "%v", err)
	}
	var entries []sandboxEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, errfmt.Newf("sandbox registry is corrupt", "fix or remove "+registryPath(dataDir)+" and re-add sandboxes", "docs/config-reference.md#data_dir", "%v", err)
	}
	return entries, nil
}

func saveRegistry(dataDir string, entries []sandboxEntry) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return errfmt.Newf("cannot create data dir", "check permissions on "+dataDir, "docs/config-reference.md#data_dir", "%v", err)
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	return store.WriteFileAtomic(registryPath(dataDir), append(raw, '\n'))
}

func findEntry(entries []sandboxEntry, name string) *sandboxEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

func newSandboxID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "sbx_" + hex.EncodeToString(b[:])
}

func loadSpec(cfg *config.Config, source string, out io.Writer) ([]byte, string, error) {
	raw, err := loadSpecOnce(source)
	if err != nil {
		return nil, "", err
	}
	isURL := strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
	head := strings.ToLower(string(raw[:min(len(raw), 512)]))
	if isURL && (strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")) {
		var llm *nl.Client
		if cfg != nil && cfg.LLM.APIKey != "" {
			llm = newLLMClient(cfg, "")
		}
		res, err := docimport.FromDocsURL(source, raw, loadSpecOnce, llm)
		if err != nil {
			return nil, "", err
		}
		fmt.Fprintf(out, "documentation page → spec via %s (%s)\n", res.Method, res.Source)
		if res.Method == "llm-extracted" {
			fmt.Fprintln(out, "note: this contract was EXTRACTED BY YOUR LLM from prose — it imports as DRAFT with LLM_EXTRACTED provenance; `pikopod sandbox list` shows the marking")
		}
		if len(res.Skipped) > 0 {
			fmt.Fprintf(out, "  ⚠ %d indexed page(s) did not fit the extraction budget, so the spec is partial:\n", len(res.Skipped))
			for _, p := range res.Skipped {
				fmt.Fprintf(out, "    %s\n", p)
			}
		}
		return res.Spec, res.Method, nil
	}
	if emittedSpecRe.Match(raw[:min(len(raw), 4096)]) {
		return raw, "llm-extracted", nil
	}
	return raw, "spec", nil
}

var emittedSpecRe = regexp.MustCompile(`"?x-pikopod-origin"?\s*:\s*"?llm-extracted`)

func normalizeByOrigin(raw []byte, origin string) (*ir.ApiDefinition, error) {
	if origin == "llm-extracted" {
		return importer.NormalizeLLMExtracted(raw)
	}
	return importer.NormalizeOpenAPI(raw)
}

func emitSpec(path string, raw []byte, origin string, out io.Writer) error {
	body := raw
	if origin == "llm-extracted" {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) == nil {
			doc["x-pikopod-origin"] = "llm-extracted"
			if pretty, err := json.MarshalIndent(doc, "", "  "); err == nil {
				body = append(pretty, '\n')
			}
		}
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return errfmt.Newf("cannot write --emit-spec file", "check the path", "docs/config-reference.md", "%v", err)
	}
	fmt.Fprintf(out, "spec written to %s\n", path)
	if origin == "llm-extracted" {
		fmt.Fprintf(out, "  review it, add x-pikopod-trigger to each webhook event, commit it, and import from the file next time (no model needed):\n"+
			"    pikopod import <name> --update --spec %s\n"+
			"  it re-imports as DRAFT until you delete the x-pikopod-origin line, which says the facts are now yours\n", path)
	}
	return nil
}

type addOptions struct {
	SpecSource         string
	Seed               string
	WebhookURL         string
	UpstreamLink       string
	RecordingsFallback bool
	EmitSpec           string
	Webhooks           string
}

func loadSpecOnce(source string) ([]byte, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		client := &http.Client{
			Timeout: 30 * time.Second,

			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects (%d) fetching the spec", len(via))
				}
				return nil
			},
		}
		resp, err := client.Get(source)
		if err != nil {
			return nil, errfmt.Newf("cannot fetch the spec", "check the URL and your network", "docs/config-reference.md", "%v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errfmt.New("cannot fetch the spec", fmt.Sprintf("%s answered %d", source, resp.StatusCode), "check the URL serves the raw OpenAPI document", "")
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return nil, errfmt.Newf("cannot read the spec response", "retry; the connection dropped mid-download", "", "%v", err)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return nil, errfmt.Newf("cannot read the spec file", "check the path passed to --spec", "docs/config-reference.md", "%v", err)
	}
	return raw, nil
}

func sandboxAdd(cfg *config.Config, name, specSource, seed, webhookURL, upstreamLink string, recordingsFallback bool, out io.Writer) error {
	return sandboxAddOpts(cfg, name, addOptions{SpecSource: specSource, Seed: seed, WebhookURL: webhookURL, UpstreamLink: upstreamLink, RecordingsFallback: recordingsFallback}, out)
}

func sandboxAddOpts(cfg *config.Config, name string, o addOptions, out io.Writer) error {
	specSource, seed, webhookURL, upstreamLink, recordingsFallback := o.SpecSource, o.Seed, o.WebhookURL, o.UpstreamLink, o.RecordingsFallback
	if strings.ContainsAny(name, "/\\ \t") || name == "" {
		return errfmt.New("invalid sandbox name", fmt.Sprintf("%q cannot contain slashes or whitespace", name), "pick a short slug like `payments` — it becomes the route /<name>/ on the sandbox server", "")
	}
	if webhookURL != "" && !strings.HasPrefix(webhookURL, "http://") && !strings.HasPrefix(webhookURL, "https://") {
		return errfmt.New("invalid webhook URL", fmt.Sprintf("%q is not an http(s) URL", webhookURL), "pass --webhook-url http://localhost:<port>/<path> — the endpoint your app listens on", "docs/config-reference.md")
	}
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return err
	}
	if findEntry(entries, name) != nil {
		return errfmt.New("sandbox already exists", fmt.Sprintf("%q is already registered", name), "use `pikopod sandbox reset "+name+"` to clear its state, or pick another name", "")
	}
	raw, origin, err := loadSpec(cfg, specSource, out)
	if err != nil {
		return err
	}
	if o.EmitSpec != "" {
		if err := emitSpec(o.EmitSpec, raw, origin, out); err != nil {
			return err
		}
	}
	def, err := normalizeByOrigin(raw, origin)
	if err != nil {
		return err
	}
	irRel := filepath.Join("apis", name+".ir.json")
	irAbs := filepath.Join(cfg.DataDir, irRel)
	if err := os.MkdirAll(filepath.Dir(irAbs), 0o700); err != nil {
		return errfmt.Newf("cannot create the apis dir", "check permissions on "+cfg.DataDir, "docs/config-reference.md#data_dir", "%v", err)
	}
	irRaw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(irAbs, irRaw); err != nil {
		return errfmt.Newf("cannot persist the IR", "check permissions on "+irAbs, "docs/config-reference.md#data_dir", "%v", err)
	}
	if seed == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		seed = hex.EncodeToString(b[:])
	}
	if upstreamLink == "" {
		if _, isUpstream := cfg.Upstreams[name]; isUpstream {
			upstreamLink = name
		}
	}
	entry := sandboxEntry{
		ID:                 newSandboxID(),
		Name:               name,
		Origin:             origin,
		Upstream:           upstreamLink,
		Seed:               seed,
		Mode:               "deterministic",
		CreatedClockMs:     sandbox.SandboxBaseEpochMs,
		IRFile:             irRel,
		SpecSource:         specSource,
		WebhookURL:         webhookURL,
		RecordingsFallback: recordingsFallback,
	}
	entries = append(entries, entry)
	if err := saveRegistry(cfg.DataDir, entries); err != nil {
		return err
	}
	fmt.Fprintf(out, "sandbox %s registered (%s, %d endpoints)\n", name, entry.ID, len(def.Endpoints))
	if len(def.Webhooks) > 0 {
		if webhookURL != "" {
			fmt.Fprintf(out, "webhooks: %d declared event(s); deliveries POST to %s (signed; secret below)\n  webhook secret: %s\n", len(def.Webhooks), webhookURL, sandbox.IssuedWebhookSecret(entry.Seed))
		} else {
			fmt.Fprintf(out, "webhooks: %d declared event(s) — pass --webhook-url to receive deliveries over HTTP\n", len(def.Webhooks))
		}
		if _, _, _, untriggered := webhookCounts(def); untriggered > 0 {
			fmt.Fprintf(out, "  ⚠ %d declared event(s) have no trigger and are not emit-only, so they will never fire.\n"+
				"    add x-pikopod-trigger {method, path} to bind an event to the call that causes it,\n"+
				"    or x-pikopod-emit-only: true for an event no API call causes (fire it with `pikopod webhook emit`)\n", untriggered)
		}
		printEnvelope(out, def)
	}
	fmt.Fprintf(out, "serve it with `pikopod up` → http://%s:%d/%s/...\n", cfg.Listen, cfg.SandboxPort, name)
	fmt.Fprintf(out, "test credential (send it the way the spec's auth scheme expects, e.g. the Authorization header):\n  %s\n", sandbox.IssuedCredential(entry.Seed))
	if o.Webhooks != "" {
		return applyWebhookSidecar(cfg, name, o.Webhooks, out)
	}
	return nil
}

func sandboxUpdate(cfg *config.Config, name, specSource string, out io.Writer) error {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return err
	}
	entry := findEntry(entries, name)
	if entry == nil {
		return errfmt.New("no such sandbox", fmt.Sprintf("%q is not registered — --update refreshes an existing import", name), "run `pikopod import "+name+" --spec …` (without --update) first", "")
	}
	if specSource == "" {
		specSource = entry.SpecSource
	}
	if specSource == "" {
		return errfmt.New("no spec source", "the sandbox was registered without a recorded source", "pass --spec <file-or-url>", "")
	}
	raw, origin, err := loadSpec(cfg, specSource, out)
	if err != nil {
		return err
	}
	def, err := normalizeByOrigin(raw, origin)
	if err != nil {
		return err
	}

	irAbs := filepath.Join(cfg.DataDir, entry.IRFile)
	if oldRaw, rErr := os.ReadFile(irAbs); rErr == nil {
		var oldDef ir.ApiDefinition
		if json.Unmarshal(oldRaw, &oldDef) == nil {
			if def.WebhookEnvelope == nil {
				def.WebhookEnvelope = oldDef.WebhookEnvelope
			}
			carryEventBindings(def, &oldDef)
			findings := specdiff.Diff(&oldDef, def)
			if len(findings) > 0 {
				fmt.Fprintf(out, "accepting %d declared change(s):\n", len(findings))
				specdiff.BuildReport("pinned", specSource, findings).WriteText(out)
			} else {
				fmt.Fprintln(out, "no declared changes vs the pinned contract")
			}
		}
	}

	irRaw, err := json.Marshal(def)
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(irAbs, irRaw); err != nil {
		return errfmt.Newf("cannot persist the IR", "check permissions on "+irAbs, "docs/config-reference.md#data_dir", "%v", err)
	}
	entry.SpecSource = specSource
	entry.Origin = origin
	if err := saveRegistry(cfg.DataDir, entries); err != nil {
		return err
	}

	watchName := entry.Upstream
	if watchName == "" {
		watchName = name
	}
	if err := specwatch.ResetPin(cfg.DataDir, watchName); err != nil {
		return err
	}
	fmt.Fprintf(out, "pin refreshed for %s (%d endpoints) — restart `pikopod up` to serve the updated contract\n", name, len(def.Endpoints))
	return nil
}

func sandboxList(cfg *config.Config, out io.Writer) error {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errfmt.New("no sandboxes registered", "the registry under "+cfg.DataDir+" is empty", "add one with `pikopod sandbox add <name> --spec <file-or-url>`", "")
	}
	for _, e := range entries {
		marking := ""
		_, def, defErr := loadSandboxDef(cfg, e.Name)
		if defErr == nil && def.Status == "DRAFT" {
			marking = "  ⚠ DRAFT (LLM_EXTRACTED: model-written from prose docs; shapes are best-effort)"
		} else if e.Origin != "" && e.Origin != "spec" {
			marking = "  origin=" + e.Origin
		}
		fmt.Fprintf(out, "%-20s %s  mode=%s seed=%s  route=/%s/  ir=%s%s\n  credential: %s\n", e.Name, e.ID, e.Mode, e.Seed, e.Name, e.IRFile, marking, sandbox.IssuedCredential(e.Seed))
		if defErr == nil && len(def.Webhooks) > 0 {
			declared, triggered, emitOnly, untriggered := webhookCounts(def)
			line := fmt.Sprintf("  webhooks: %d declared, %d triggered, %d emit-only", declared, triggered, emitOnly)
			if untriggered > 0 {
				line += fmt.Sprintf(", %d never fire", untriggered)
			}
			if summary := envelopeSummary(def); summary != "" {
				line += "; " + summary
			}
			fmt.Fprintln(out, line)
		}
	}
	return nil
}

func webhookCounts(def *ir.ApiDefinition) (declared, triggered, emitOnly, untriggered int) {
	for i := range def.Webhooks {
		declared++
		switch {
		case def.Webhooks[i].Trigger != nil:
			triggered++
		case def.Webhooks[i].EmitOnly:
			emitOnly++
		default:
			untriggered++
		}
	}
	return
}

func sandboxReset(cfg *config.Config, name string, out io.Writer) error {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return err
	}
	entry := findEntry(entries, name)
	if entry == nil {
		return errfmt.New("unknown sandbox", fmt.Sprintf("%q is not registered", name), "see `pikopod sandbox list`; add it with `pikopod sandbox add`", "")
	}
	st, err := sandbox.OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Clear(entry.ID); err != nil {
		return errfmt.Newf("cannot clear sandbox state", "check permissions on "+cfg.DataDir, "docs/config-reference.md#data_dir", "%v", err)
	}

	fmt.Fprintf(out, "sandbox %s cleared — stored state is empty (seed %s unchanged)\n", name, entry.Seed)
	fmt.Fprintln(out, "note: a running `pikopod up` keeps in-memory journal/idempotency/webhook state for this sandbox — restart it for a fully fresh slate")
	fmt.Fprintln(out, "note: the id sequence keeps advancing after a reset; for a byte-identical replay from scratch, register a fresh sandbox with the same seed")
	return nil
}

func effectiveFor(cfg *config.Config, entry *sandboxEntry, version int) *contract.Effective {
	upstream := entry.Upstream
	if upstream == "" {
		upstream = entry.Name
	}
	ov, err := contract.LoadOverlay(cfg.DataDir, upstream)
	if err != nil || ov == nil || ov.Version == 0 {
		return nil
	}
	at := ov.Version
	if version > 0 && version < at {
		at = version
	}
	return contract.ResolveAt(ov, at)
}

func recordingsFor(cfg *config.Config, entry *sandboxEntry) *replay.Set {
	if !entry.RecordingsFallback {
		return nil
	}
	upstream := entry.Upstream
	if upstream == "" {
		upstream = entry.Name
	}
	set, err := replay.Load(cfg.DataDir, upstream, cfg.Upstreams[upstream].VolatileFields)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox %s: recordings fallback is ON but no recordings loaded for upstream %q (%v) — unmatched requests will 404 until traffic is recorded\n", entry.Name, upstream, err)
		return nil
	}
	return set
}

type sandboxServer struct {
	cfg   *config.Config
	store *sandbox.Store

	wallclockFaults bool

	mu      sync.Mutex
	entries map[string]sandboxEntry

	modes map[string]*mode.Spec

	handlers map[string]http.Handler
}

func newSandboxServer(cfg *config.Config) (*sandboxServer, error) {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := sandbox.OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := checkSigningKeys(cfg, entries); err != nil {
		st.Close()
		return nil, err
	}
	byName := make(map[string]sandboxEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	return &sandboxServer{cfg: cfg, store: st, entries: byName, handlers: map[string]http.Handler{}}, nil
}

func (s *sandboxServer) Close() error {
	s.mu.Lock()
	handlers := make([]http.Handler, 0, len(s.handlers))
	for _, h := range s.handlers {
		handlers = append(handlers, h)
	}
	s.mu.Unlock()
	for _, h := range handlers {
		if eng, ok := h.(*sandbox.Engine); ok {
			eng.Close()
		}
	}
	return s.store.Close()
}

func (s *sandboxServer) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	return names
}

func (s *sandboxServer) handlerFor(name string) (http.Handler, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.handlers[name]; ok {
		return h, nil
	}
	entry, ok := s.entries[name]
	if !ok {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, entry.IRFile))
	if err != nil {
		return nil, errfmt.Newf("cannot read the persisted IR for "+name, "re-add the sandbox with `pikopod sandbox add`", "docs/config-reference.md#data_dir", "%v", err)
	}
	var def ir.ApiDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		return nil, errfmt.Newf("persisted IR for "+name+" is corrupt", "re-add the sandbox with `pikopod sandbox add`", "docs/config-reference.md#data_dir", "%v", err)
	}
	signingKey, err := webhookSigningKey(&def)
	if err != nil {
		return nil, err
	}
	engine, err := sandbox.NewEngine(&def, sandbox.Config{
		ID:                entry.ID,
		Seed:              entry.Seed,
		Mode:              entry.Mode,
		VirtualClockMs:    entry.CreatedClockMs,
		WallclockFaults:   s.wallclockFaults,
		Effective:         effectiveFor(s.cfg, &entry, 0),
		Recordings:        recordingsFor(s.cfg, &entry),
		WebhookURL:        entry.WebhookURL,
		WebhookSigningKey: signingKey,
	}, s.store)
	if err != nil {
		return nil, err
	}

	engine.SetMountPrefix("/" + name)
	s.handlers[name] = engine
	return engine, nil
}

func (s *sandboxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			w.Header().Set("content-type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"Internal Server Error"}`))
		}
	}()

	if token := s.cfg.Token(); token != "" {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Pikopod-Token")), []byte(token)) != 1 {
			http.Error(w, "pikopod: missing or wrong X-Pikopod-Token", http.StatusUnauthorized)
			return
		}
		r.Header.Del("X-Pikopod-Token")
	}

	if strings.HasPrefix(r.URL.Path, "/_pikopod/") {
		s.serveAdmin(w, r)
		return
	}

	name, rest := splitSandboxPath(r.URL.EscapedPath())
	if name == "" {
		writeSandboxJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	h, err := s.handlerFor(name)
	if err != nil {

		writeSandboxJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if h == nil {
		writeSandboxJSONError(w, http.StatusNotFound, "Not Found")
		return
	}

	r2 := r.Clone(r.Context())
	_, decodedRest := splitSandboxPath(r.URL.Path)
	r2.URL.Path = decodedRest
	r2.URL.RawPath = rest
	if decodedRest == rest {
		r2.URL.RawPath = ""
	}
	h.ServeHTTP(w, r2)
}

func (s *sandboxServer) serveAdmin(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	admin := len(parts) == 4 && (parts[3] == "faults" || parts[3] == "requests" || parts[3] == "mode" || parts[3] == "webhooks")
	emit := len(parts) == 5 && parts[3] == "webhooks" && parts[4] == "emit"
	if len(parts) < 4 || parts[1] != "sandboxes" || (!admin && !emit) {
		writeSandboxJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	h, err := s.handlerFor(parts[2])
	if err != nil || h == nil {
		writeSandboxJSONError(w, http.StatusNotFound, "unknown sandbox "+parts[2])
		return
	}
	engine, ok := h.(*sandbox.Engine)
	if !ok {
		writeSandboxJSONError(w, http.StatusNotFound, "unknown sandbox "+parts[2])
		return
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	if emit {
		s.serveWebhookEmit(w, r, engine)
		return
	}
	if parts[3] == "mode" {
		s.serveMode(w, r, parts[2], engine)
		return
	}
	if parts[3] == "webhooks" {
		if r.Method != http.MethodGet {
			writeSandboxJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"deliveries": engine.Deliveries(""), "sink": engine.WebhookSinkStats()})
		return
	}
	if parts[3] == "requests" {

		switch r.Method {
		case http.MethodGet:
			limit := 0
			if v := r.URL.Query().Get("limit"); v != "" {
				limit, _ = strconv.Atoi(v)
			}
			entries, evicted := engine.JournalEntries(limit)
			json.NewEncoder(w).Encode(map[string]any{"requests": entries, "evicted": evicted})
		case http.MethodDelete:
			engine.ResetJournal()
			json.NewEncoder(w).Encode(map[string]any{"reset": true})
		default:
			writeSandboxJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{"faults": engine.Faults()})
	case http.MethodPost:
		var rule sandbox.FaultRule
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rule); err != nil {
			writeSandboxJSONError(w, http.StatusBadRequest, "body must be a FaultRule JSON object")
			return
		}
		if !sandbox.ValidFaultKind(rule.Kind) {
			writeSandboxJSONError(w, http.StatusBadRequest, "kind must be one of "+strings.Join(sandbox.FaultKinds(), ", "))
			return
		}

		if !sandbox.IsWebhookFaultKind(rule.Kind) && (rule.Method == "" || rule.Path == "") {
			writeSandboxJSONError(w, http.StatusBadRequest, "method and path are required")
			return
		}
		rule.Kind, rule.Status = sandbox.ResolveFaultKind(rule.Kind, rule.Status)
		if rule.Probability == 0 {
			rule.Probability = 1
		}
		engine.ArmFault(rule)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"armed": rule})
	case http.MethodDelete:
		n := engine.ClearFaults(r.URL.Query().Get("method"), r.URL.Query().Get("path"))
		json.NewEncoder(w).Encode(map[string]any{"cleared": n})
	default:
		writeSandboxJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func writeSandboxJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"message":%q}`, message)
}

func splitSandboxPath(path string) (string, string) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "/"
	}
	if i := strings.Index(trimmed, "/"); i >= 0 {
		return trimmed[:i], trimmed[i:]
	}
	return trimmed, "/"
}

func newSandboxAddCmd() *cobra.Command {
	c := &cobra.Command{Use: "add <name>", Short: "Import a spec and register a deterministic sandbox served at /<name>/", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			spec, _ := cmd.Flags().GetString("spec")
			if spec == "" {
				return errfmt.New("no spec given", "sandbox add needs the provider's OpenAPI document", "pass --spec <file-or-url>", "")
			}
			return sandboxAddOpts(cfg, args[0], addOptionsFrom(cmd, spec), cmd.OutOrStdout())
		}}
	addImportFlags(c)
	return c
}

func addImportFlags(c *cobra.Command) {
	c.Flags().String("spec", "", "spec source (local file or http(s) URL): OpenAPI 3.x, Swagger 2.0, Postman collection, GraphQL schema, or a documentation page")
	c.Flags().String("seed", "", "run seed (default: random; pin one for reproducible transcripts)")
	c.Flags().String("webhook-url", "", "optional HTTP(S) sink: webhook deliveries POST here (signed)")
	c.Flags().String("webhooks", "", "YAML/JSON file describing how the provider wraps and signs deliveries, and which calls fire which events")
	c.Flags().String("emit-spec", "", "write the spec the import used (extracted or fetched) to this path so it can be reviewed, corrected and committed")
	c.Flags().String("upstream", "", "link to a drift-agent upstream so its traffic refines this contract (auto when names match)")
	c.Flags().Bool("recordings-fallback", false, "serve the linked upstream's recordings for requests neither the spec nor admitted traffic can answer (final tier; X-Pikopod-Replay-Tier)")
}

func addOptionsFrom(cmd *cobra.Command, spec string) addOptions {
	o := addOptions{SpecSource: spec}
	o.Seed, _ = cmd.Flags().GetString("seed")
	o.WebhookURL, _ = cmd.Flags().GetString("webhook-url")
	o.UpstreamLink, _ = cmd.Flags().GetString("upstream")
	o.RecordingsFallback, _ = cmd.Flags().GetBool("recordings-fallback")
	o.EmitSpec, _ = cmd.Flags().GetString("emit-spec")
	o.Webhooks, _ = cmd.Flags().GetString("webhooks")
	return o
}

func newSandboxListCmd() *cobra.Command {
	c := &cobra.Command{Use: "list", Short: "List registered sandboxes",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return sandboxList(cfg, cmd.OutOrStdout())
		}}
	return c
}

func newSandboxResetCmd() *cobra.Command {
	c := &cobra.Command{Use: "reset <name>", Short: "Clear a sandbox's stored state (same seed replays identically)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return sandboxReset(cfg, args[0], cmd.OutOrStdout())
		}}
	return c
}

func newSandboxRequestsCmd() *cobra.Command {
	c := &cobra.Command{Use: "requests <name>", Short: "Show the requests a running sandbox has received (the client-behavior journal)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			limit, _ := cmd.Flags().GetInt("last")
			reset, _ := cmd.Flags().GetBool("reset")
			url := fmt.Sprintf("%s://%s:%d/_pikopod/sandboxes/%s/requests", cfg.Scheme(), cfg.Listen, cfg.SandboxPort, args[0])
			client := cfg.LocalClient(5 * time.Second)

			var resp *http.Response
			method, target := http.MethodGet, url+"?limit="+strconv.Itoa(limit)
			if reset {
				method, target = http.MethodDelete, url
			}
			req, _ := http.NewRequest(method, target, nil)
			if token := cfg.Token(); token != "" {
				req.Header.Set("X-Pikopod-Token", token)
			}
			resp, err = client.Do(req)
			if err != nil {
				return errfmt.New("sandbox server is not running", "nothing answered on "+url, "start it with `pikopod up`", "docs/config-reference.md")
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return errfmt.Newf("sandbox journal unavailable", "check the sandbox name with `pikopod sandbox list`", "", "the server answered %s: %s", resp.Status, raw)
			}
			out := cmd.OutOrStdout()
			if reset {
				fmt.Fprintln(out, "journal reset")
				return nil
			}
			var payload struct {
				Requests []sandbox.JournalEntry `json:"requests"`
				Evicted  int64                  `json:"evicted"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return err
			}
			for _, e := range payload.Requests {
				line := fmt.Sprintf("#%-4d %-6s %-40s → %d", e.Seq, e.Method, e.Path, e.Status)
				if e.Template != "" && e.Template != e.Path {
					line += "  (" + e.Template + ")"
				}
				if e.BodyTruncated {
					line += "  [body truncated]"
				}
				fmt.Fprintln(out, line)
			}
			fmt.Fprintf(out, "%d request(s)", len(payload.Requests))
			if payload.Evicted > 0 {
				fmt.Fprintf(out, " — %d older entries EVICTED (upper-bound verifications fail closed)", payload.Evicted)
			}
			fmt.Fprintln(out)
			return nil
		}}
	c.Flags().Int("last", 50, "number of most recent requests to show")
	c.Flags().Bool("reset", false, "clear the journal (and its eviction taint)")
	return c
}
