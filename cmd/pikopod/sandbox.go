// Sandbox CLI wiring: a small on-disk registry (<data_dir>/sandboxes.json plus
// apis/<name>.ir.json) that `pikopod up` serves on cfg.SandboxPort.
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

// sandboxEntry is one registered sandbox in <data_dir>/sandboxes.json.
type sandboxEntry struct {
	ID string `json:"id"` // sbx_<hex>
	// Name is the route prefix on the sandbox server: /<name>/...
	Name string `json:"name"`
	Seed string `json:"seed"`
	Mode string `json:"mode"` // "deterministic"
	// CreatedClockMs is the sandbox's pinned virtual clock (base epoch).
	CreatedClockMs int64 `json:"createdClockMs"`
	// IRFile is the persisted ApiDefinition, relative to data_dir.
	IRFile     string `json:"irFile"`
	SpecSource string `json:"specSource"`
	// Origin is how the spec was obtained: "spec" | "postman-documenter" |
	// "spec-link" | "readme-embedded" | "llm-extracted".
	Origin string `json:"origin,omitempty"`
	// Upstream links this sandbox to a drift-agent upstream so its traffic refines
	// this IR. Auto-matched when the sandbox name equals an upstream name.
	Upstream string `json:"upstream,omitempty"`
	// WebhookURL is an optional HTTP(S) sink that receives the sandbox's
	// webhook deliveries as signed POSTs (--webhook-url on add/import).
	WebhookURL string `json:"webhookUrl,omitempty"`
	// RecordingsFallback opts into the recordings tier: recorded traffic answers
	// requests neither the spec nor an admitted observed endpoint can.
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
	// Atomic: the registry's seeds derive credentials and webhook signing secrets,
	// which are unrecoverable if a crash or ENOSPC tears this file.
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
		panic(err) // crypto/rand failing means the host is broken
	}
	return "sbx_" + hex.EncodeToString(b[:])
}

// loadSpec reads a spec from a file or URL, sending HTML doc pages through the
// docimport ladder. It also returns the ORIGIN, so provenance stays honest.
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
		return res.Spec, res.Method, nil
	}
	return raw, "spec", nil
}

func loadSpecOnce(source string) ([]byte, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		client := &http.Client{
			Timeout: 30 * time.Second,
			// Cap redirect hops: a pasted spec URL must not walk the fetcher through a
			// long chain. (Go refuses non-http(s) schemes, so file:// is unreachable.)
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

// sandboxAdd imports the spec, persists its IR, and registers the sandbox.
func sandboxAdd(cfg *config.Config, name, specSource, seed, webhookURL, upstreamLink string, recordingsFallback bool, out io.Writer) error {
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
	var def *ir.ApiDefinition
	if origin == "llm-extracted" {
		def, err = importer.NormalizeLLMExtracted(raw)
	} else {
		def, err = importer.NormalizeOpenAPI(raw)
	}
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
			upstreamLink = name // the common case: same slug both sides
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
	return nil
}

// sandboxUpdate re-imports a spec and resets the declared-drift pin — the
// operator's "reviewed, accept" step. It prints what it absorbs first.
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
	var def *ir.ApiDefinition
	if origin == "llm-extracted" {
		def, err = importer.NormalizeLLMExtracted(raw)
	} else {
		def, err = importer.NormalizeOpenAPI(raw)
	}
	if err != nil {
		return err
	}

	// Show what this update absorbs (old pin vs new spec) before overwriting.
	irAbs := filepath.Join(cfg.DataDir, entry.IRFile)
	if oldRaw, rErr := os.ReadFile(irAbs); rErr == nil {
		var oldDef ir.ApiDefinition
		if json.Unmarshal(oldRaw, &oldDef) == nil {
			if def.WebhookEnvelope == nil {
				def.WebhookEnvelope = oldDef.WebhookEnvelope
			}
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
	// Reset the watcher-owned pin so the next check re-seeds against the refreshed
	// contract instead of re-reporting absorbed findings.
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
		if e.Origin == "llm-extracted" {
			marking = "  ⚠ DRAFT (LLM_EXTRACTED: model-written from prose docs; shapes are best-effort)"
		} else if e.Origin != "" && e.Origin != "spec" {
			marking = "  origin=" + e.Origin
		}
		fmt.Fprintf(out, "%-20s %s  mode=%s seed=%s  route=/%s/  ir=%s%s\n  credential: %s\n", e.Name, e.ID, e.Mode, e.Seed, e.Name, e.IRFile, marking, sandbox.IssuedCredential(e.Seed))
		if _, def, err := loadSandboxDef(cfg, e.Name); err == nil && len(def.Webhooks) > 0 {
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

// webhookCounts splits declared events into the ones that can fire and the
// ones that cannot, which import and list both report.
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
	// The id sequence is monotonic and never reused, so a reset continues
	// numbering rather than replaying old ids.
	fmt.Fprintf(out, "sandbox %s cleared — stored state is empty (seed %s unchanged)\n", name, entry.Seed)
	fmt.Fprintln(out, "note: a running `pikopod up` keeps in-memory journal/idempotency/webhook state for this sandbox — restart it for a fully fresh slate")
	fmt.Fprintln(out, "note: the id sequence keeps advancing after a reset; for a byte-identical replay from scratch, register a fresh sandbox with the same seed")
	return nil
}

// effectiveFor resolves the linked upstream's traffic overlay (nil when none).
// version 0 means latest; a positive version pins, for from-drift runs.
func effectiveFor(cfg *config.Config, entry *sandboxEntry, version int) *contract.Effective {
	upstream := entry.Upstream
	if upstream == "" {
		upstream = entry.Name // auto-link by name (the common case)
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

// recordingsFor loads the linked upstream's recordings for the recordings tier.
// Recordings only exist after traffic flows, so an empty load notes, never fails.
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

// sandboxServer serves /<name>/... for every registered sandbox. Engines share
// one SQLite store, so a sandbox at rest is exactly its committed rows.
type sandboxServer struct {
	cfg   *config.Config
	store *sandbox.Store
	// wallclockFaults makes every engine's faults act on the real wire
	// (`up --wallclock-faults`).
	wallclockFaults bool

	mu      sync.Mutex
	entries map[string]sandboxEntry
	// modes is the standing state each sandbox was put into, by name.
	modes map[string]*mode.Spec
	// handlers caches built engines. Tests may pre-register any http.Handler
	// (e.g. a panicking one) to exercise the recovery boundary.
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

func (s *sandboxServer) Close() error { return s.store.Close() }

func (s *sandboxServer) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	return names
}

// handlerFor lazily builds (and caches) the engine for one sandbox.
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
	// Self-referential URLs (pagination Link headers) must carry the mount.
	engine.SetMountPrefix("/" + name)
	s.handlers[name] = engine
	return engine, nil
}

// ServeHTTP routes /<name>/... to that sandbox's engine. PER-SUBSYSTEM PANIC
// RECOVERY: a panic 500s this one caller and never takes the process down.
func (s *sandboxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			w.Header().Set("content-type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"Internal Server Error"}`))
		}
	}()

	// Same token gate as the agent proxy (listen-safety invariant);
	// constant-time so a non-loopback bind can't leak the token via timing.
	if token := s.cfg.Token(); token != "" {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Pikopod-Token")), []byte(token)) != 1 {
			http.Error(w, "pikopod: missing or wrong X-Pikopod-Token", http.StatusUnauthorized)
			return
		}
		r.Header.Del("X-Pikopod-Token")
	}

	// Admin surface (chaos), reachable only through this server — chaos can never
	// target anything but a sandbox, which IS the default-deny allowlist.
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
		// Config/IO faults are the operator's problem, never the caller's:
		// mirror a neutral 500 (details go nowhere near the data plane).
		writeSandboxJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if h == nil {
		writeSandboxJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	// Strip the /<name> prefix keeping BOTH URL forms consistent: escaped remainder
	// to RawPath, decoded to Path, or EscapedPath() re-escapes literal '%' bytes.
	r2 := r.Clone(r.Context())
	_, decodedRest := splitSandboxPath(r.URL.Path)
	r2.URL.Path = decodedRest
	r2.URL.RawPath = rest
	if decodedRest == rest {
		r2.URL.RawPath = "" // no escaping in play — canonical form
	}
	h.ServeHTTP(w, r2)
}

// serveAdmin handles /_pikopod/sandboxes/<name>/faults. Rules live on the served
// engine, so chaos applies to the traffic your app is sending RIGHT NOW.
func (s *sandboxServer) serveAdmin(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// _pikopod / sandboxes / <name> / faults|requests|mode|webhooks/emit
	admin := len(parts) == 4 && (parts[3] == "faults" || parts[3] == "requests" || parts[3] == "mode")
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
	if parts[3] == "requests" {
		// The request journal (P1): what the CLIENT sent this sandbox —
		// GET lists (newest last, ?limit=), DELETE resets.
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
		// Webhook rules match on the event, never method/path.
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
			seed, _ := cmd.Flags().GetString("seed")
			webhookURL, _ := cmd.Flags().GetString("webhook-url")
			upstreamLink, _ := cmd.Flags().GetString("upstream")
			recFallback, _ := cmd.Flags().GetBool("recordings-fallback")
			if err := sandboxAdd(cfg, args[0], spec, seed, webhookURL, upstreamLink, recFallback, cmd.OutOrStdout()); err != nil {
				return err
			}
			if sidecar, _ := cmd.Flags().GetString("webhooks"); sidecar != "" {
				return applyWebhookSidecar(cfg, args[0], sidecar, cmd.OutOrStdout())
			}
			return nil
		}}
	c.Flags().String("spec", "", "spec source (local file or http(s) URL): OpenAPI 3.x, Swagger 2.0, Postman collection, or GraphQL schema")
	c.Flags().String("seed", "", "run seed (default: random; pin one for reproducible transcripts)")
	c.Flags().String("webhook-url", "", "optional HTTP(S) sink: webhook deliveries POST here (signed)")
	c.Flags().String("webhooks", "", "YAML/JSON file describing how the provider wraps and signs deliveries (for specs without x-pikopod-webhook-envelope)")
	c.Flags().String("upstream", "", "link to a drift-agent upstream so its traffic refines this contract (auto when names match)")
	c.Flags().Bool("recordings-fallback", false, "serve the linked upstream's recordings for requests neither the spec nor admitted traffic can answer (final tier; X-Pikopod-Replay-Tier)")
	return c
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

// newSandboxRequestsCmd inspects a RUNNING sandbox's request journal —
// what the client actually sent.
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
			// The sandbox server gates EVERY request on the token when one is resolved,
			// so this command must send it or it 401s in tokenized setups.
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
