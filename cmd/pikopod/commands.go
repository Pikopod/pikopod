package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pikopod/pikopod/internal/agent"
	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/demo"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/scenario/nl"
	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/pikopod/pikopod/internal/specwatch"
	"github.com/pikopod/pikopod/internal/store"
	"github.com/spf13/cobra"
)

// loadConfig resolves the inherited --config flag. Every command reads the
// configuration through here so the flag works the same everywhere.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	path, _ := cmd.Flags().GetString("config")
	return config.Load(path)
}

// newLLMClient builds the BYOK client. Model precedence is explicit override,
// then llm.model, then the package default; the base URL override is test-only.
func newLLMClient(cfg *config.Config, model string) *nl.Client {
	if model == "" {
		model = cfg.LLM.Model
	}
	c := nl.NewClient(cfg.LLM.OpenRouterKey, model)
	if base := os.Getenv("PIKOPOD_OPENROUTER_BASE"); base != "" {
		c.BaseURL = base
	}
	return c
}

func newDemoCmd() *cobra.Command {
	return &cobra.Command{Use: "demo", Short: "Self-contained drift demo: fake provider drifts, alert prints (<5 min, zero config)",
		RunE: func(cmd *cobra.Command, args []string) error { return demo.Run(cmd.OutOrStdout()) }}
}

const initTemplate = `# pikopod.yaml — see docs/config-reference.md for every key + default
# listen: 127.0.0.1        # non-loopback requires PIKOPOD_TOKEN (env) or token_file
# data_dir: pikopod-data

upstreams:
  # name each provider; your app's base URL points at http://127.0.0.1:4700/<name>
  # (any HTTP API works — the name and target below are placeholders)
  examplepay:
    target: https://api.examplepay.com
    # volatile_fields: [request_ref]   # extra per-provider normalization strips
    # mute: ["/transaction/verify/{id}"]

slack:
  # webhook_url: https://hooks.slack.com/services/…   # alerts land here

llm:
  # openrouter_key: sk-or-…   # BYOK for plain-English scenarios and pikopod fix; or PIKOPOD_OPENROUTER_KEY

# warmup:                    # eval-only overrides; defaults 50 samples / 48h
#   min_samples: 50
#   min_hours: 48

# refine:                    # contract refinement from observed traffic (off by default)
#   enabled: true            # traffic grows an overlay beside the spec contract
#   prefer_spec: false       # default: sustained traffic WINS type conflicts

# sampling:                  # thin what recordings PERSIST — learning always sees everything
#   rate: 0.1                # keep ~10% of routine records; errors/drift/pre-warmup always kept

# retention:                 # age recordings + the drift-event log out of disk
#   max_age_hours: 168       # kept >= this, deleted by ~2x this; 0 (default) = size-only rotation
`

func newInitCmd() *cobra.Command {
	return &cobra.Command{Use: "init", Short: "Scaffold pikopod.yaml (providers, Slack webhook, optional BYOK LLM key)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat("pikopod.yaml"); err == nil {
				return errfmt.New("pikopod.yaml already exists", "refusing to overwrite your configuration", "edit it directly, or remove it first to re-scaffold", "docs/config-reference.md")
			}
			if err := os.WriteFile("pikopod.yaml", []byte(initTemplate), 0o600); err != nil {
				return errfmt.Newf("cannot write pikopod.yaml", "check directory permissions", "docs/config-reference.md", "%v", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "wrote pikopod.yaml — edit the upstreams block, then: pikopod doctor && pikopod up")
			return nil
		}}
}

// newImportCmd is the front-door spelling of `sandbox add`: import a spec, get a
// sandbox. Same registry; it adds --update, which re-imports an existing one.
func newImportCmd() *cobra.Command {
	c := &cobra.Command{Use: "import <provider>", Short: "Import an API spec → a deterministic sandbox at /<provider>/", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			spec, _ := cmd.Flags().GetString("spec")
			if update, _ := cmd.Flags().GetBool("update"); update {
				return sandboxUpdate(cfg, args[0], spec, cmd.OutOrStdout())
			}
			if spec == "" {
				return errfmt.New("no spec given", "import needs the provider's OpenAPI document", "pass --spec <file-or-url>", "")
			}
			seed, _ := cmd.Flags().GetString("seed")
			webhookURL, _ := cmd.Flags().GetString("webhook-url")
			upstreamLink, _ := cmd.Flags().GetString("upstream")
			recFallback, _ := cmd.Flags().GetBool("recordings-fallback")
			return sandboxAdd(cfg, args[0], spec, seed, webhookURL, upstreamLink, recFallback, cmd.OutOrStdout())
		}}
	c.Flags().String("spec", "", "spec source (local file or http(s) URL): OpenAPI 3.x, Swagger 2.0, Postman collection, or GraphQL schema")
	c.Flags().String("seed", "", "run seed (default: random; pin one for reproducible transcripts)")
	c.Flags().String("webhook-url", "", "optional HTTP(S) sink: webhook deliveries POST here (signed)")
	c.Flags().String("upstream", "", "link to a drift-agent upstream so its traffic refines this contract (auto when names match)")
	c.Flags().Bool("recordings-fallback", false, "serve the linked upstream's recordings for requests neither the spec nor admitted traffic can answer (final tier; X-Pikopod-Replay-Tier)")
	c.Flags().Bool("update", false, "re-import an existing provider's spec and refresh the declared-drift pin (accepts the standing spec-diff findings)")
	return c
}

func newUpCmd() *cobra.Command {
	c := &cobra.Command{Use: "up", Short: "Serve the drift agent (:4700) and registered sandboxes (:4600)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if len(cfg.Upstreams) == 0 {
				return errfmt.New("no upstreams configured", "pikopod.yaml has an empty upstreams block", "add at least one provider (see the scaffold comments), then retry", "docs/config-reference.md#upstreams")
			}
			a, err := agent.New(cfg, alert.Options{
				Retention:    cfg.RetentionTTL(),
				MinLevel:     cfg.Slack.MinLevel,
				SummaryEvery: time.Duration(cfg.Slack.DigestHours) * time.Hour,
			})
			if err != nil {
				return err
			}
			if cfg.Refine.Enabled {
				contracts, err := contractsForUpstreams(cfg)
				if err != nil {
					return err
				}
				a.SetContracts(contracts)
				fmt.Fprintf(out2(cmd), "contract refinement ON: traffic refines %d linked contract(s)\n", len(contracts))
			}
			if w, n := buildSpecWatcher(cfg, a); w != nil {
				a.SetWatcher(w)
				fmt.Fprintf(out2(cmd), "spec watch ON: %d declared source(s), re-checked every %s\n", n, cfg.SpecWatchInterval())
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "pikopod agent on %s://%s:%d  (upstreams: %s)\n",
				cfg.Scheme(), cfg.Listen, cfg.AgentPort, strings.Join(cfg.UpstreamNames(), ", "))
			fmt.Fprintf(out, "point your app's provider base URL at %s://%s:%d/<upstream>; /healthz shows progress\n", cfg.Scheme(), cfg.Listen, cfg.AgentPort)

			// Sandbox server: its own port, its own panic boundary. A fault in
			// one subsystem never takes down the other.
			sbx, err := newSandboxServer(cfg)
			if err != nil {
				return err
			}
			if wallclock, _ := cmd.Flags().GetBool("wallclock-faults"); wallclock {
				sbx.wallclockFaults = true
				fmt.Fprintln(out, "wallclock faults ON: armed faults really delay/hang on the wire")
			}
			defer sbx.Close()
			sbxAddr := net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.SandboxPort))
			// Header-only deadline: armed hang/slow_body faults act on the
			// RESPONSE and are unaffected by it.
			sbxSrv := &http.Server{Addr: sbxAddr, Handler: sbx, ReadHeaderTimeout: 20 * time.Second}
			sbxErr := make(chan error, 1)
			go func() {
				if cfg.TLS.Enabled() {
					sbxErr <- sbxSrv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
					return
				}
				sbxErr <- sbxSrv.ListenAndServe()
			}()
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := sbxSrv.Shutdown(shutdownCtx); err != nil {
					// Same rule as the agent server: after the drain grace, stragglers
					// are hard-closed so a stop never hangs on a client's conn pool.
					sbxSrv.Close()
				}
			}()
			if names := sbx.Names(); len(names) > 0 {
				fmt.Fprintf(out, "pikopod sandbox on %s://%s  (sandboxes: %s)\n", cfg.Scheme(), sbxAddr, strings.Join(names, ", "))
			} else {
				fmt.Fprintf(out, "pikopod sandbox on %s://%s  (none registered — `pikopod sandbox add <name> --spec …`)\n", cfg.Scheme(), sbxAddr)
			}

			go func() {
				if err := <-sbxErr; err != nil && err != http.ErrServerClosed {
					// The sandbox listener failing is loud but non-fatal: the
					// drift agent keeps running.
					fmt.Fprintf(cmd.ErrOrStderr(), "sandbox server stopped: %v\n", err)
				}
			}()
			return a.Run(ctx)
		}}
	c.Flags().Bool("wallclock-faults", false, "every armed fault delays/hangs on the REAL wire (default: virtualized, instant)")
	return c
}

func out2(cmd *cobra.Command) io.Writer { return cmd.OutOrStdout() }

// buildSpecWatcher arms the declared-drift watcher for every upstream with a
// spec_source. Pins come from the linked sandbox's IR, else the first fetch.
func buildSpecWatcher(cfg *config.Config, a *agent.Agent) (*specwatch.Watcher, int) {
	var sources []specwatch.Source
	pins, _ := contractsForUpstreams(cfg) // best-effort: nil map on error is fine
	for _, name := range cfg.UpstreamNames() {
		u := cfg.Upstreams[name]
		if u.SpecSource == "" {
			continue
		}
		sources = append(sources, specwatch.Source{Upstream: name, SpecSource: u.SpecSource, Pinned: pins[name]})
	}
	if len(sources) == 0 {
		return nil, 0
	}
	w := specwatch.New(cfg.DataDir, sources, cfg.SpecWatchInterval(), func(upstream string, f specdiff.Finding) {
		// The declared×observed join: traffic evidence can raise the level
		// and sharpen the detail; the fingerprint (identity) never moves.
		a.EnrichDeclared(upstream, &f)
		a.Alerter.ReportDeclared(upstream, f.Fingerprint(), alert.DriftEvent{
			Method:   f.Method,
			Endpoint: f.Template,
			Kind:     drift.Kind("declared:" + f.ID),
			Level:    string(f.Level),
			Detail:   f.Detail,
		})
	})
	return w, len(sources)
}

// contractsForUpstreams loads the IR for every upstream a registered sandbox
// links to (explicit `upstream` link, or name equality — the common case).
func contractsForUpstreams(cfg *config.Config) (map[string]*ir.ApiDefinition, error) {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	out := map[string]*ir.ApiDefinition{}
	for name := range cfg.Upstreams {
		for i := range entries {
			e := &entries[i]
			if e.Upstream != name && e.Name != name {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(cfg.DataDir, e.IRFile))
			if err != nil {
				continue
			}
			var def ir.ApiDefinition
			if json.Unmarshal(raw, &def) == nil {
				out[name] = &def
			}
			break
		}
	}
	return out, nil
}

func newSandboxCmd() *cobra.Command {
	c := &cobra.Command{Use: "sandbox", Short: "Manage provider sandboxes (add, list, reset, requests)"}
	c.AddCommand(newSandboxAddCmd(), newSandboxListCmd(), newSandboxResetCmd(), newSandboxRequestsCmd())
	return c
}

func newScenarioCmd() *cobra.Command {
	c := &cobra.Command{Use: "scenario", Short: "List, create (archetypes or plain English via BYOK), run, and derive scenarios from drift or recorded traffic"}

	list := &cobra.Command{Use: "list <sandbox>", Short: "Show which archetypes bind to this sandbox's API, plus saved packs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			verbose, _ := cmd.Flags().GetBool("verbose")
			return scenarioList(cfg, args[0], verbose, cmd.OutOrStdout())
		}}
	list.Flags().BoolP("verbose", "v", false, "show which operations each archetype bound to")

	run := &cobra.Command{Use: "run <sandbox> <scenarios...>", Short: "Bind, expand, and run scenarios (exit 0 pass / 1 fail / 2 error)", Args: cobra.MinimumNArgs(2),
		RunE: scenarioRun}
	run.Flags().Bool("persist", false, "run against the sandbox's real store: SEEDED STATE stands for the served sandbox; armed faults do NOT outlive the run (arm the daemon with `pikopod chaos` instead)")
	run.Flags().String("seed", "", "run seed (default: the sandbox's seed)")
	run.Flags().StringArray("input", nil, "scenario input as name=value (repeatable)")
	run.Flags().StringArray("bind", nil, "override a role binding as role=operationId (repeatable)")
	run.Flags().String("target", "", "run against a REAL endpoint (base URL) instead of the local sandbox — REQUEST/NOTE steps only")
	run.Flags().StringArray("target-header", nil, "header sent on every remote request as 'Name:value' (repeatable; e.g. Authorization)")

	create := &cobra.Command{Use: "create <sandbox> <description...>", Short: "Compile a plain-English scenario, grounded against your API (BYOK LLM)", Args: cobra.MinimumNArgs(2),
		RunE: scenarioCreate}
	create.Flags().Bool("yes", false, "save without the confirmation prompt")
	create.Flags().String("model", "", "OpenRouter model (default "+nl.DefaultModel+")")

	c.AddCommand(list, run, create, newFromDriftCmd(), newFromRecordingsCmd())
	return c
}

func newReplayCmd() *cobra.Command {
	c := &cobra.Command{Use: "replay [upstreams...]", Short: "Replay recorded traffic; --ci gates builds (exit 0 clean / 1 drift / 2 error)",
		RunE: runReplay}
	c.Flags().Bool("ci", false, "CI gate: diff recordings offline against frozen baselines")
	c.Flags().String("handoff", "", "also write the JSON findings here (for `pikopod pr comment`)")
	c.Flags().String("serve", "", "RETIRED: use `sandbox add --recordings-fallback` — recordings now serve through the sandbox")
	return c
}

func newBaselineCmd() *cobra.Command {
	c := &cobra.Command{Use: "baseline", Short: "Manage learned baselines"}
	reset := &cobra.Command{Use: "reset <upstream>", Short: "Re-learn baselines (atomic swap)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			path := filepath.Join(cfg.DataDir, "baselines", args[0]+".json")
			if len(args) == 1 {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return errfmt.Newf("cannot reset baselines", "check permissions on "+path, "docs/config-reference.md#data_dir", "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "baselines for %s reset — warmup restarts on next traffic\n", args[0])
				return nil
			}
			return errfmt.New("per-template reset needs a running agent", "the file-level CLI can only reset a whole upstream today", "restart `pikopod up` after resetting, or reset the whole upstream", "docs/config-reference.md#baselines")
		}}
	c.AddCommand(reset)
	return c
}

func newReportCmd() *cobra.Command {
	return &cobra.Command{Use: "report", Short: "Endpoint inventory + learned-baseline report (the 48h artifact)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			found := false
			for _, name := range cfg.UpstreamNames() {
				raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "baselines", name+".json"))
				if err != nil {
					continue
				}
				found = true
				var fams map[string]struct {
					Method      string `json:"method"`
					Template    string `json:"template"`
					StatusClass string `json:"status_class"`
					Samples     int    `json:"samples"`
					Frozen      bool   `json:"frozen"`
					Fields      map[string]struct {
						Count int            `json:"count"`
						Types map[string]int `json:"types"`
					} `json:"fields"`
				}
				if err := json.Unmarshal(raw, &fams); err != nil {
					continue
				}
				fmt.Fprintf(out, "\n%s — endpoint inventory (what your app ACTUALLY calls)\n", name)
				for _, f := range fams {
					state := "warming up"
					if f.Frozen {
						state = "baseline frozen"
					}
					fmt.Fprintf(out, "  %-6s %-45s %s  %4d samples  %d fields  [%s]\n", f.Method, f.Template, f.StatusClass, f.Samples, len(f.Fields), state)
				}
			}
			if !found {
				return errfmt.New("no baselines yet", "the agent has not observed traffic (or data_dir differs)", "run `pikopod up` and point traffic at it; the report is worth reading after ~48h", "docs/config-reference.md#baselines")
			}
			return nil
		}}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Verify wiring end to end (config, ports, upstreams, data dir)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			ok := true
			check := func(name string, err error) {
				if err != nil {
					ok = false
					fmt.Fprintf(out, "  ✗ %s\n      %v\n", name, err)
					return
				}
				fmt.Fprintf(out, "  ✓ %s\n", name)
			}

			cfg, err := loadConfig(cmd)
			check("pikopod.yaml loads", err)
			if err != nil {
				return errfmt.New("doctor stopped early", "the configuration must load before anything else can be checked", "fix the error above and re-run", "docs/config-reference.md")
			}
			check("data dir writable", func() error {
				if mkErr := os.MkdirAll(cfg.DataDir, 0o700); mkErr != nil {
					return mkErr
				}
				probe := filepath.Join(cfg.DataDir, ".doctor")
				if wErr := os.WriteFile(probe, []byte("ok"), 0o600); wErr != nil {
					return wErr
				}
				return os.Remove(probe)
			}())
			check("salt present or creatable", func() error {
				_, sErr := store.LoadOrCreateSalt(cfg.SaltPath())
				return sErr
			}())
			addr := net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.AgentPort))
			check("agent port free or agent already running", func() error {
				conn, dErr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
				if dErr == nil {
					conn.Close()
					resp, hErr := cfg.LocalClient(2 * time.Second).Get(cfg.Scheme() + "://" + addr + "/healthz")
					if hErr == nil {
						resp.Body.Close()
						return nil // running pikopod — fine
					}
					return fmt.Errorf("port %s is taken by something that is not pikopod", addr)
				}
				return nil // free
			}())
			for _, name := range cfg.UpstreamNames() {
				target := cfg.Upstreams[name].Target
				check("upstream "+name+" reachable ("+target+")", func() error {
					client := &http.Client{Timeout: 5 * time.Second}
					req, _ := http.NewRequest(http.MethodHead, target, nil)
					resp, rErr := client.Do(req)
					if rErr != nil {
						return rErr
					}
					resp.Body.Close()
					return nil
				}())
			}
			if !ok {
				return errfmt.New("doctor found problems", "one or more checks failed above", "fix the ✗ items and re-run pikopod doctor", "docs/config-reference.md")
			}
			fmt.Fprintln(out, "all checks passed — point your app at the agent and run `pikopod up`")
			return nil
		}}
}

func newInspectCmd() *cobra.Command {
	c := &cobra.Command{Use: "inspect", Short: "Show stored records with tokenized fields visible — verify redaction yourself",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			last, _ := cmd.Flags().GetInt("last")
			format, _ := cmd.Flags().GetString("format")
			if format != "" && format != "json" && format != "curl" && format != "har" {
				return errfmt.New("unknown --format "+format, "inspect renders json (default), curl, or har", "e.g. pikopod inspect --format curl", "")
			}
			out := cmd.OutOrStdout()
			shown := 0
			var harRecords []*proxy.Record
			harTarget := ""
			for _, name := range cfg.UpstreamNames() {
				nd, err := store.OpenNDJSON(filepath.Join(cfg.DataDir, "recordings", name+".ndjson"), 0)
				if err != nil {
					continue
				}
				lines, _ := nd.ReadLast(last)
				nd.Close()
				target := cfg.Upstreams[name].Target
				for _, line := range lines {
					switch format {
					case "curl", "har":
						var rec proxy.Record
						if json.Unmarshal(line, &rec) != nil {
							continue
						}
						shown++
						if format == "curl" {
							renderCurl(out, target, &rec)
						} else {
							harRecords = append(harRecords, &rec)
							harTarget = target
						}
					default:
						var pretty map[string]any
						if json.Unmarshal(line, &pretty) == nil {
							enc, _ := json.MarshalIndent(pretty, "", "  ")
							fmt.Fprintf(out, "%s\n", enc)
							shown++
						}
					}
				}
			}
			if format == "har" && shown > 0 {
				enc, err := json.MarshalIndent(harArchive(harTarget, harRecords), "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "%s\n", enc)
				return nil
			}
			if format == "curl" && shown > 0 {
				fmt.Fprintln(out, "# all values are POST-SANITIZER (tokens/placeholders) — safe to share, not raw traffic")
				return nil
			}
			if shown == 0 {
				return errfmt.New("nothing recorded yet", "no recordings exist under "+cfg.DataDir, "run `pikopod up`, send traffic through the agent, then inspect again", "docs/config-reference.md#data_dir")
			}
			fmt.Fprintf(out, "\n%d record(s) — redaction happened BEFORE any byte touched disk:\n"+
				"  secrets, card-security codes (cvv/pin/otp), and credentials became <<SUBSTITUTE:…>> placeholders — string or number;\n"+
				"  identifiers, emails, phones, card/account numbers, and expiry dates became format-preserving tokens;\n"+
				"  names and free text were dropped. Values shown raw were classified safe (enums, amounts, booleans).\n"+
				"Residual: a NUMERIC secret under a field name pikopod does not recognize passes through —\n"+
				"numbers carry no shape signal, so field names are the only evidence. Check your provider's\n"+
				"field names above; report gaps at https://github.com/pikopod/pikopod/issues.\n", shown)
			return nil
		}}
	c.Flags().Int("last", 5, "number of records to show per upstream")
	c.Flags().String("format", "json", "output format: json | curl | har")
	return c
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Traffic, warmup progress, baselines, alert delivery health",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			addr := fmt.Sprintf("%s://%s/healthz", cfg.Scheme(), net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.AgentPort)))
			client := cfg.LocalClient(2 * time.Second)
			resp, err := client.Get(addr)
			if err != nil {
				return errfmt.New("agent is not running", "nothing answered on "+addr, "start it with `pikopod up` (or check listen/agent_port in pikopod.yaml)", "docs/config-reference.md")
			}
			defer resp.Body.Close()
			var health map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
				return errfmt.Newf("agent answered strangely", "retry; if it persists the port is taken by something else", "docs/config-reference.md", "%v", err)
			}
			enc, _ := json.MarshalIndent(health, "", "  ")
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", enc)
			return nil
		}}
}
