package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/conformance"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/mcp"
	"github.com/pikopod/pikopod/internal/replay"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/pikopod/pikopod/internal/scenario/resolve"
	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	VerdictClean        = "CLEAN"
	VerdictFindings     = "FINDINGS"
	VerdictUnverifiable = "UNVERIFIABLE"
	VerdictError        = "ERROR"
)

const mcpDocs = "docs/config-reference.md#mcp"

type toolError struct {
	What string `json:"what"`
	Why  string `json:"why"`
	Fix  string `json:"fix"`
	Docs string `json:"docs,omitempty"`
}

type toolResult struct {
	Verdict string         `json:"verdict"`
	Reason  string         `json:"reason,omitempty"`
	Warmup  *warmupReport  `json:"warmup,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
	Error   *toolError     `json:"error,omitempty"`
}

type warmupFamily struct {
	Method      string `json:"method"`
	Template    string `json:"template"`
	StatusClass string `json:"status_class"`
	Samples     int    `json:"samples"`
	WarmedUp    bool   `json:"warmed_up"`
}

type warmupReport struct {
	Upstream        string         `json:"upstream"`
	GateMinSamples  int            `json:"gate_min_samples"`
	GateMinHours    int            `json:"gate_min_hours"`
	Families        []warmupFamily `json:"families"`
	WarmedUp        int            `json:"warmed_up"`
	StillWarming    int            `json:"still_warming"`
	NoBaselinesYet  bool           `json:"no_baselines_yet"`
	BlindSpotNotice string         `json:"blind_spot"`
}

func errorResult(err error) any {
	var e *errfmt.E
	if errors.As(err, &e) {
		return toolResult{Verdict: VerdictError, Error: &toolError{What: e.What, Why: e.Why, Fix: e.Fix, Docs: e.Docs}}
	}
	return toolResult{Verdict: VerdictError, Error: &toolError{What: "pikopod failed", Why: err.Error(), Fix: "see the message; run the equivalent CLI command for the full context", Docs: mcpDocs}}
}

func warmupFor(cfg *config.Config, upstream string) *warmupReport {
	rep := &warmupReport{Upstream: upstream, GateMinSamples: cfg.Warmup.MinSamples, Families: []warmupFamily{},
		BlindSpotNotice: "only endpoints that received traffic through the agent are known; an endpoint with no traffic is invisible, and one still warming up cannot alert"}
	if cfg.Warmup.MinHours != nil {
		rep.GateMinHours = *cfg.Warmup.MinHours
	}
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "baselines", upstream+".json"))
	if err != nil {
		rep.NoBaselinesYet = true
		return rep
	}
	var fams map[string]*baseline.Family
	if json.Unmarshal(raw, &fams) != nil {
		rep.NoBaselinesYet = true
		return rep
	}
	for _, f := range fams {
		rep.Families = append(rep.Families, warmupFamily{Method: f.Method, Template: f.Template, StatusClass: f.StatusClass, Samples: f.Samples, WarmedUp: f.Frozen})
		if f.Frozen {
			rep.WarmedUp++
		} else {
			rep.StillWarming++
		}
	}
	sort.Slice(rep.Families, func(i, j int) bool {
		a, b := rep.Families[i], rep.Families[j]
		return a.Method+a.Template+a.StatusClass < b.Method+b.Template+b.StatusClass
	})
	rep.NoBaselinesYet = len(rep.Families) == 0
	return rep
}

func decodeArgs(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return errfmt.Newf("bad tool arguments", "pass only the arguments the tool's schema declares", mcpDocs, "%v", err)
	}
	return nil
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func readOnly() map[string]any {
	return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
}

func controlsFake() map[string]any {
	return map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
}

func mcpServer(cfg *config.Config) *mcp.Server {
	s := mcp.NewServer("pikopod", version)
	s.OnError = errorResult

	s.Register(mcp.Tool{Name: "spec_diff", Annotations: readOnly(),
		Description: "Diff two API spec versions (file path, http(s) URL, or git:<ref>:<path>) as declared drift with a severity per finding. Needs both documents; returns FINDINGS when anything at or above fail_on (ERR default; WARN, INFO) exists, else CLEAN with lower findings attached. It sees only what the documents declare: behaviour a provider changed without updating its spec is invisible here (use drift_events for that).",
		InputSchema: schema([]string{"old", "new"}, map[string]any{"old": prop("string", "older spec source"), "new": prop("string", "newer spec source"), "fail_on": prop("string", "ERR | WARN | INFO (default ERR)")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Old    string `json:"old"`
				New    string `json:"new"`
				FailOn string `json:"fail_on"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			if args.FailOn == "" {
				args.FailOn = "ERR"
			}
			floor, err := parseLevelFloor(args.FailOn)
			if err != nil {
				return nil, err
			}
			oldDef, _, err := loadSpecIR(args.Old)
			if err != nil {
				return nil, err
			}
			newDef, _, err := loadSpecIR(args.New)
			if err != nil {
				return nil, err
			}
			findings := specdiff.Diff(oldDef, newDef)
			report := specdiff.BuildReport(args.Old, args.New, findings)
			verdict := VerdictClean
			if specdiff.Breaking(findings, floor) {
				verdict = VerdictFindings
			}
			return toolResult{Verdict: verdict, Data: map[string]any{"fail_on": string(floor), "summary": report.Summary, "findings": report.Items}}, nil
		}})

	s.Register(mcp.Tool{Name: "drift_events", Annotations: readOnly(),
		Description: "Drift events and incidents the agent recorded for an upstream, from the local event log. Reports only changes observed in traffic that passed warmup (default 50 samples and 48 hours per endpoint): until then the verdict is UNVERIFIABLE with the samples seen and the gate, never an empty CLEAN. An endpoint that receives no traffic through the agent is invisible. Values are redacted; identifiers appear as tokens.",
		InputSchema: schema([]string{"upstream"}, map[string]any{"upstream": prop("string", "upstream name from pikopod.yaml"), "since": prop("string", "window like 24h (optional)"), "level": prop("string", "ERR | WARN | INFO (optional)"), "limit": prop("integer", "max events (default 50)")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Upstream string `json:"upstream"`
				Since    string `json:"since"`
				Level    string `json:"level"`
				Limit    int    `json:"limit"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			if _, ok := cfg.Upstreams[args.Upstream]; !ok {
				return nil, errfmt.New("unknown upstream", args.Upstream+" is not in pikopod.yaml", "use one of: "+strings.Join(cfg.UpstreamNames(), ", "), "docs/config-reference.md#upstreams")
			}
			evs, err := loadEvents(cfg.DataDir)
			if err != nil {
				return nil, err
			}
			cutoff := time.Time{}
			if args.Since != "" {
				d, err := time.ParseDuration(args.Since)
				if err != nil {
					return nil, errfmt.Newf("bad since", "use a Go duration like 24h", mcpDocs, "%v", err)
				}
				cutoff = time.Now().Add(-d)
			}
			var kept []alert.DriftEvent
			for _, ev := range evs {
				if ev.Upstream != args.Upstream || (args.Level != "" && !strings.EqualFold(ev.Level, args.Level)) || (!cutoff.IsZero() && ev.LastSeen.Before(cutoff)) {
					continue
				}
				kept = append(kept, ev)
			}
			sort.Slice(kept, func(i, j int) bool { return kept[i].LastSeen.After(kept[j].LastSeen) })
			limit := args.Limit
			if limit <= 0 {
				limit = defaultIncidentLimit
			}
			total, truncated := len(kept), false
			if total > limit {
				kept, truncated = kept[:limit], true
			}
			if kept == nil {
				kept = []alert.DriftEvent{}
			}
			w := warmupFor(cfg, args.Upstream)
			data := map[string]any{"events": kept, "total_matching": total, "truncated": truncated}
			switch {
			case len(kept) > 0:
				return toolResult{Verdict: VerdictFindings, Warmup: w, Data: data}, nil
			case w.NoBaselinesYet:
				return toolResult{Verdict: VerdictUnverifiable, Warmup: w, Data: data,
					Reason: fmt.Sprintf("%s has no baselines yet: the agent has not observed traffic for it (or data_dir differs), so nothing could have been detected; gate is %d samples and %dh per endpoint", args.Upstream, w.GateMinSamples, w.GateMinHours)}, nil
			case w.WarmedUp == 0:
				return toolResult{Verdict: VerdictUnverifiable, Warmup: w, Data: data,
					Reason: fmt.Sprintf("warmup incomplete: %d endpoint famil(ies) seen, none past the gate of %d samples and %dh, so no drift could have been reported yet", w.StillWarming, w.GateMinSamples, w.GateMinHours)}, nil
			default:
				return toolResult{Verdict: VerdictClean, Warmup: w, Data: data,
					Reason: fmt.Sprintf("no events for %d warmed-up endpoint famil(ies); %d still warming and cannot alert yet", w.WarmedUp, w.StillWarming)}, nil
			}
		}})

	s.Register(mcp.Tool{Name: "replay_ci", Annotations: readOnly(),
		Description: "The CI gate: diff recorded traffic offline against frozen baselines for one or more upstreams. FINDINGS lists drift per endpoint; CLEAN means every gated recording matched. UNVERIFIABLE with a reason when an upstream has no baselines or no recordings, which is not a pass. Recordings made before an endpoint warmed up are skipped and counted.",
		InputSchema: schema(nil, map[string]any{"upstreams": map[string]any{"type": "array", "items": prop("string", ""), "description": "upstreams to gate (default: all)"}}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Upstreams []string `json:"upstreams"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			ups := args.Upstreams
			if len(ups) == 0 {
				ups = cfg.UpstreamNames()
			}
			results := map[string]any{}
			findings := 0
			var unverifiable []string
			for _, name := range ups {
				res, err := replay.Gate(cfg.DataDir, name, cfg.Upstreams[name].VolatileFields)
				if err != nil {
					var e *errfmt.E
					if errors.As(err, &e) {
						unverifiable = append(unverifiable, name+": "+e.What+": "+e.Why)
						results[name] = map[string]any{"gated": false, "reason": e.What + ": " + e.Why, "fix": e.Fix}
						continue
					}
					return nil, err
				}
				findings += len(res.Findings)
				results[name] = map[string]any{"gated": true, "records": res.Records, "skipped_pre_warmup": res.Skipped, "findings": res.Findings}
			}
			data := map[string]any{"upstreams": results}
			switch {
			case findings > 0:
				return toolResult{Verdict: VerdictFindings, Data: data, Reason: strings.Join(unverifiable, "; ")}, nil
			case len(unverifiable) > 0:
				return toolResult{Verdict: VerdictUnverifiable, Data: data, Reason: strings.Join(unverifiable, "; ")}, nil
			}
			return toolResult{Verdict: VerdictClean, Data: data}, nil
		}})

	s.Register(mcp.Tool{Name: "conformance", Annotations: readOnly(),
		Description: "Validate an upstream's recorded responses against its imported spec: does the provider obey its own documentation? FINDINGS lists violations. Checks whose evidence the sanitizer redacted are counted as unverifiable and never scored as passes; with no violations and some unverifiable checks the verdict is UNVERIFIABLE. Needs recordings and an imported spec for the upstream.",
		InputSchema: schema([]string{"upstream"}, map[string]any{"upstream": prop("string", "upstream name")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Upstream string `json:"upstream"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			def, err := irForUpstream(cfg, args.Upstream)
			if err != nil {
				return nil, err
			}
			records, err := readRecordings(cfg.DataDir, args.Upstream)
			if err != nil {
				return nil, err
			}
			if len(records) == 0 {
				return toolResult{Verdict: VerdictUnverifiable, Reason: "no recordings for " + args.Upstream + ": the agent has not recorded traffic yet, so nothing was checked"}, nil
			}
			rep := conformance.Check(def, records)
			data := map[string]any{"records_checked": rep.Records, "skipped": rep.Skipped, "unverifiable_checks": rep.Unverifiable, "violations": rep.Violations}
			switch {
			case len(rep.Violations) > 0:
				return toolResult{Verdict: VerdictFindings, Data: data}, nil
			case rep.Unverifiable > 0:
				return toolResult{Verdict: VerdictUnverifiable, Data: data, Reason: fmt.Sprintf("%d check(s) could not be proven because the sanitizer redacted the evidence; %d response(s) had no violation", rep.Unverifiable, rep.Records)}, nil
			case rep.Records == 0:
				return toolResult{Verdict: VerdictUnverifiable, Data: data, Reason: "no JSON response matched a declared endpoint, so nothing was checked"}, nil
			}
			return toolResult{Verdict: VerdictClean, Data: data}, nil
		}})

	s.Register(mcp.Tool{Name: "reproduce", Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		Description: "Turn a recorded production incident (by fingerprint from drift_events) into a runnable scenario pack and run it against the local sandbox. FINDINGS with reproduced=true means the failure now happens locally and the pack path is returned. UNVERIFIABLE when the pack ran but the recorded failure did not recur, or when no sandbox exists to run it against. Writes one pack file under data_dir/scenarios; touches nothing else. The request is rebuilt from a redacted recording, so a 4xx that depended on the exact body may not recur.",
		InputSchema: schema([]string{"fingerprint"}, map[string]any{"fingerprint": prop("string", "fp_… from drift_events"), "sandbox": prop("string", "sandbox to run against (default: the incident's upstream)")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Fingerprint string `json:"fingerprint"`
				Sandbox     string `json:"sandbox"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			ev, err := bridge.FindEvent(cfg.DataDir, args.Fingerprint)
			if err != nil {
				return nil, err
			}
			rec, err := bridge.FindRecording(cfg.DataDir, ev)
			if err != nil {
				return nil, err
			}
			pinVersion := 0
			if ov, _ := contract.LoadOverlay(cfg.DataDir, ev.Upstream); ov != nil {
				pinVersion = ov.Version
			}
			name, pack, err := bridge.BuildFromRecord(ev, rec, pinVersion)
			if err != nil {
				return nil, err
			}
			rendered, err := yaml.Marshal(pack)
			if err != nil {
				return nil, err
			}
			path, err := bridge.Save(cfg.DataDir, name, rendered)
			if err != nil {
				return nil, err
			}
			data := map[string]any{"pack": name, "path": path, "incident": ev}
			sandboxName := args.Sandbox
			if sandboxName == "" {
				sandboxName = ev.Upstream
			}
			entry, def, err := loadSandboxDef(cfg, sandboxName)
			if err != nil {
				return toolResult{Verdict: VerdictUnverifiable, Data: data, Reason: "pack written but there is no sandbox " + sandboxName + " to run it against: " + err.Error()}, nil
			}
			parsed, _, err := resolveRunnable(cfg, name, def, nil)
			if err != nil {
				return nil, err
			}
			eng, done, err := scenarioEngine(cfg, entry, def, false, pinVersion)
			if err != nil {
				return nil, err
			}
			res, err := scenario.Run(eng, parsed, nil, entry.Seed)
			done()
			if err != nil {
				return nil, err
			}
			data["run"] = res
			switch res.Status {
			case scenario.RunPassed:
				data["reproduced"] = true
				return toolResult{Verdict: VerdictFindings, Data: data}, nil
			case scenario.RunErrored:
				return nil, errfmt.Newf("the reproduction could not run", "fix the pack or the sandbox, then re-run", "scenarios/README.md", "%s", res.Summary)
			}
			data["reproduced"] = false
			return toolResult{Verdict: VerdictUnverifiable, Data: data, Reason: "the pack ran but the recorded failure did not recur; the recorded body is redacted, and a 4xx usually turns on it"}, nil
		}})

	s.Register(mcp.Tool{Name: "scenario_list", Annotations: readOnly(),
		Description: "Which failure archetypes bind to a sandbox's API, with candidate bindings, and for each one that does not bind the exact reason (missing role, or a docs import whose facts are all extracted, with the --bind assertion that would ground it). Also lists saved packs. CLEAN when at least one archetype binds; UNVERIFIABLE when none does, with every reason attached. Binding is a property of the imported spec, not of any traffic.",
		InputSchema: schema([]string{"sandbox"}, map[string]any{"sandbox": prop("string", "sandbox name")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string `json:"sandbox"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			_, def, err := loadSandboxDef(cfg, args.Sandbox)
			if err != nil {
				return nil, err
			}
			bindings := resolve.ListBindings(def)
			packs, _ := scenario.ListPacks(packDirs(cfg)...)
			packList := []map[string]any{}
			for _, p := range packs {
				packList = append(packList, map[string]any{"name": p.Name, "description": p.Description, "path": p.Path})
			}
			applicable := 0
			var reasons []string
			for _, b := range bindings {
				if b.Applicable {
					applicable++
				} else {
					reasons = append(reasons, b.ID+": "+b.Reason)
				}
			}
			data := map[string]any{"archetypes": bindings, "packs": packList, "endpoints": len(def.Endpoints)}
			if applicable == 0 {
				return toolResult{Verdict: VerdictUnverifiable, Data: data, Reason: "no archetype binds to this API: " + strings.Join(reasons, "; ")}, nil
			}
			return toolResult{Verdict: VerdictClean, Data: data}, nil
		}})

	s.Register(mcp.Tool{Name: "scenario_run", Annotations: readOnly(),
		Description: "Run archetypes or saved packs against a throwaway copy of a sandbox (the served sandbox is untouched) and return each scenario's verdict with its steps and failed assertions. CLEAN when all pass, FINDINGS when any fails, ERROR when a step could not execute. This drives pikopod's own requests, not the caller's application; use set_mode to put the running sandbox into a state the caller's own tests then meet.",
		InputSchema: schema([]string{"sandbox", "names"}, map[string]any{"sandbox": prop("string", "sandbox name"), "names": map[string]any{"type": "array", "items": prop("string", ""), "description": "archetype ids or pack names"}, "bind": map[string]any{"type": "object", "additionalProperties": prop("string", ""), "description": "role → operationId overrides"}}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string            `json:"sandbox"`
				Names   []string          `json:"names"`
				Bind    map[string]string `json:"bind"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			if len(args.Names) == 0 {
				return nil, errfmt.New("no scenarios named", "names is empty", "pass archetype ids or pack names from scenario_list", "scenarios/README.md")
			}
			entry, def, err := loadSandboxDef(cfg, args.Sandbox)
			if err != nil {
				return nil, err
			}
			runs := []map[string]any{}
			failed, errored := 0, 0
			for _, name := range args.Names {
				parsed, info, err := resolveRunnable(cfg, name, def, args.Bind)
				if err != nil {
					return nil, err
				}
				pinned := 0
				if pk := packFor(cfg, name); pk != nil {
					pinned = pk.ContractVersion
				}
				eng, done, err := scenarioEngine(cfg, entry, def, false, pinned)
				if err != nil {
					return nil, err
				}
				res, err := scenario.Run(eng, parsed, nil, entry.Seed)
				done()
				if err != nil {
					return nil, err
				}
				run := map[string]any{"name": name, "result": res}
				if note := info.Note(); note != "" {
					run["note"] = name + " " + note
				}
				runs = append(runs, run)
				switch res.Status {
				case scenario.RunFailed:
					failed++
				case scenario.RunErrored:
					errored++
				}
			}
			data := map[string]any{"runs": runs}
			switch {
			case errored > 0:
				return toolResult{Verdict: VerdictError, Data: data, Error: &toolError{What: "scenario run errored", Why: "a step could not execute", Fix: "fix the scenario or sandbox and retry", Docs: "docs/exit-codes.md"}}, nil
			case failed > 0:
				return toolResult{Verdict: VerdictFindings, Data: data}, nil
			}
			return toolResult{Verdict: VerdictClean, Data: data}, nil
		}})

	s.Register(mcp.Tool{Name: "set_mode", Annotations: controlsFake(),
		Description: "Put the RUNNING sandbox (pikopod up) into a scenario's failure state so the caller's own tests, application or plain HTTP requests meet it: for example declines, rate limiting, duplicate webhook deliveries. The mode stands until clear_mode. Controls a local fake, never a provider. Needs pikopod up; scenarios that have no standing state (their first step is a request) are refused with the reason.",
		InputSchema: schema([]string{"sandbox", "name"}, map[string]any{"sandbox": prop("string", "sandbox name"), "name": prop("string", "archetype id or pack name"), "bind": map[string]any{"type": "object", "additionalProperties": prop("string", ""), "description": "role → operationId overrides"}}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string            `json:"sandbox"`
				Name    string            `json:"name"`
				Bind    map[string]string `json:"bind"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			body, err := controlCall(cfg, http.MethodPost, args.Sandbox, "mode", modeRequest{Name: args.Name, Binds: args.Bind})
			if err != nil {
				return nil, err
			}
			return toolResult{Verdict: VerdictClean, Data: body}, nil
		}})

	s.Register(mcp.Tool{Name: "clear_mode", Annotations: controlsFake(),
		Description: "Clear the standing mode on a RUNNING sandbox and every fault it armed, returning it to baseline behaviour.",
		InputSchema: schema([]string{"sandbox"}, map[string]any{"sandbox": prop("string", "sandbox name")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string `json:"sandbox"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			body, err := controlCall(cfg, http.MethodDelete, args.Sandbox, "mode", nil)
			if err != nil {
				return nil, err
			}
			return toolResult{Verdict: VerdictClean, Data: body}, nil
		}})

	s.Register(mcp.Tool{Name: "arm_fault", Annotations: controlsFake(),
		Description: "Arm one fault on a RUNNING sandbox: error (with status), latency, hang, slow_body, rate_limit, connection_reset, empty_response, random_data_then_close, malformed_response, wrong_content_length, or a webhook fault (duplicate_webhook, drop_webhook, reorder_webhook, delay_webhook, matched by event). HTTP faults need method and path template; webhook faults do not. Stands until clear_faults. Controls a local fake, never a provider.",
		InputSchema: schema([]string{"sandbox", "kind"}, map[string]any{"sandbox": prop("string", "sandbox name"), "kind": prop("string", strings.Join(sandbox.FaultKinds(), " | ")), "method": prop("string", "HTTP method of the target operation"), "path": prop("string", "path template as in the spec"), "status": prop("integer", "status for kind=error (default 500)"), "event": prop("string", "webhook event for webhook kinds (default any)"), "probability": prop("number", "0..1 (default 1)"), "delay_ms": prop("integer", "for latency / delay_webhook")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox     string  `json:"sandbox"`
				Kind        string  `json:"kind"`
				Method      string  `json:"method"`
				Path        string  `json:"path"`
				Status      int     `json:"status"`
				Event       string  `json:"event"`
				Probability float64 `json:"probability"`
				DelayMs     int64   `json:"delay_ms"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			if !sandbox.ValidFaultKind(args.Kind) {
				return nil, errfmt.New("unknown fault kind", fmt.Sprintf("%q is not a fault kind", args.Kind), "use one of: "+strings.Join(sandbox.FaultKinds(), ", "), "scenarios/README.md")
			}
			if !sandbox.IsWebhookFaultKind(args.Kind) && (args.Method == "" || args.Path == "") {
				return nil, errfmt.New("fault needs a target operation", "pass method and path (the endpoint's path template)", "e.g. kind=error status=503 method=POST path=/charges", "scenarios/README.md")
			}
			if args.Probability == 0 {
				args.Probability = 1
			}
			rule := sandbox.FaultRule{Method: strings.ToUpper(args.Method), Path: args.Path, Probability: args.Probability, Event: args.Event, DelayMs: args.DelayMs}
			rule.Kind, rule.Status = sandbox.ResolveFaultKind(args.Kind, args.Status)
			if args.Kind == "latency" && rule.DelayMs == 0 {
				rule.DelayMs = 30000
			}
			body, err := controlCall(cfg, http.MethodPost, args.Sandbox, "faults", rule)
			if err != nil {
				return nil, err
			}
			return toolResult{Verdict: VerdictClean, Data: body}, nil
		}})

	s.Register(mcp.Tool{Name: "clear_faults", Annotations: controlsFake(),
		Description: "Clear standing faults on a RUNNING sandbox: those matching method and path, or every fault when both are omitted.",
		InputSchema: schema([]string{"sandbox"}, map[string]any{"sandbox": prop("string", "sandbox name"), "method": prop("string", ""), "path": prop("string", "")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string `json:"sandbox"`
				Method  string `json:"method"`
				Path    string `json:"path"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			q := url.Values{}
			if args.Method != "" {
				q.Set("method", strings.ToUpper(args.Method))
			}
			if args.Path != "" {
				q.Set("path", args.Path)
			}
			sub := "faults"
			if len(q) > 0 {
				sub += "?" + q.Encode()
			}
			body, err := controlCall(cfg, http.MethodDelete, args.Sandbox, sub, nil)
			if err != nil {
				return nil, err
			}
			return toolResult{Verdict: VerdictClean, Data: body}, nil
		}})

	s.Register(mcp.Tool{Name: "get_requests", Annotations: readOnly(),
		Description: "What the caller's application actually sent to a RUNNING sandbox: the request journal (method, path, status, redacted headers and query, body), newest last. This is the only way to check what code under test sent, as opposed to what it was meant to send. The journal is bounded; when older entries were evicted the result says so and cannot prove ordering claims across the gap.",
		InputSchema: schema([]string{"sandbox"}, map[string]any{"sandbox": prop("string", "sandbox name"), "last": prop("integer", "how many newest entries (default 50)")}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string `json:"sandbox"`
				Last    int    `json:"last"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			if args.Last <= 0 {
				args.Last = 50
			}
			body, err := controlCall(cfg, http.MethodGet, args.Sandbox, "requests?limit="+strconv.Itoa(args.Last), nil)
			if err != nil {
				return nil, err
			}
			res := toolResult{Verdict: VerdictClean, Data: body}
			if evicted, _ := body["evicted"].(float64); evicted > 0 {
				res.Reason = fmt.Sprintf("%d older request(s) were evicted from the journal; claims spanning them cannot be proven", int(evicted))
			}
			return res, nil
		}})

	s.Register(mcp.Tool{Name: "emit_webhook", Annotations: controlsFake(),
		Description: "Fire a DECLARED webhook event on a RUNNING sandbox, for events no API call causes (money landing, a chargeback, a KYC decision). The delivery is signed and shaped the way the sandbox's spec declares and POSTed to the sandbox's configured webhook URL. Undeclared events are refused; pikopod never invents one.",
		InputSchema: schema([]string{"sandbox", "event"}, map[string]any{"sandbox": prop("string", "sandbox name"), "event": prop("string", "declared event name"), "data": map[string]any{"type": "object", "description": "fields overlaid onto the documented payload"}}),
		Handler: func(_ context.Context, raw json.RawMessage) (any, error) {
			var args struct {
				Sandbox string          `json:"sandbox"`
				Event   string          `json:"event"`
				Data    json.RawMessage `json:"data"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			body, err := controlCall(cfg, http.MethodPost, args.Sandbox, "webhooks/emit", emitRequest{Event: args.Event, Data: args.Data})
			if err != nil {
				return nil, err
			}
			return toolResult{Verdict: VerdictClean, Data: body}, nil
		}})

	return s
}

func controlCall(cfg *config.Config, method, sandboxName, subpath string, body any) (map[string]any, error) {
	resp, err := adminReq(cfg, method, sandboxName, subpath, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out map[string]any
	json.Unmarshal(raw, &out)
	if resp.StatusCode >= 400 {
		msg, _ := out["message"].(string)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		why, _, _ := strings.Cut(msg, " → ")
		return nil, errfmt.New("the sandbox refused the request", why, "check the sandbox name with `pikopod sandbox list` and the arguments against the tool description", mcpDocs)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func newMCPCmd() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Serve pikopod's checks and sandbox controls to a coding agent over MCP (stdio)",
		Long: `Serve pikopod over the Model Context Protocol on stdin/stdout.

The tools are the checks an agent cannot perform by reading files: what a
provider actually sent, what the caller's code actually sent, and whether a
change breaks either. Every result carries a verdict from a closed set
(CLEAN, FINDINGS, UNVERIFIABLE, ERROR); UNVERIFIABLE always says why.
fix is deliberately absent: it edits code with a model and stays a
human-operated command.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return mcpServer(cfg).Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		}}
}
