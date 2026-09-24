package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/replay"
	"github.com/spf13/cobra"
)

func runReplay(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	ci, _ := cmd.Flags().GetBool("ci")
	out := cmd.OutOrStdout()

	if !ci {
		return errfmt.New("replay needs --ci", "--ci is the only mode: it diffs recordings offline against frozen baselines", "run `pikopod replay --ci [upstreams...]`", "docs/exit-codes.md")
	}

	upstreams := args
	if len(upstreams) == 0 {
		upstreams = cfg.UpstreamNames()
	}
	totalFindings, totalRecords := 0, 0
	type handoffFinding struct {
		Upstream string `json:"upstream"`
		Method   string `json:"method"`
		Template string `json:"template"`
		Kind     string `json:"kind"`
		Field    string `json:"field,omitempty"`
		Detail   string `json:"detail,omitempty"`
	}
	var handoffFindings []handoffFinding
	for _, name := range upstreams {
		res, err := replay.Gate(cfg.DataDir, name, cfg.Upstreams[name].VolatileFields)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %d recordings gated (%d pre-warmup skipped) — %d finding(s)\n", name, res.Records, res.Skipped, len(res.Findings))
		for _, f := range res.Findings {
			fmt.Fprintf(out, "  DRIFT %-16s %s %s  %s %s\n", f.Kind, f.Method, f.Template, f.Field, f.Detail)
			handoffFindings = append(handoffFindings, handoffFinding{Upstream: name, Method: f.Method, Template: f.Template, Kind: f.Kind, Field: f.Field, Detail: f.Detail})
		}
		totalFindings += len(res.Findings)
		totalRecords += res.Records
	}
	if handoff, _ := cmd.Flags().GetString("handoff"); handoff != "" {
		raw, hErr := json.MarshalIndent(map[string]any{
			"source": "replay-ci", "records": totalRecords, "findings": handoffFindings,
		}, "", "  ")
		if hErr != nil {
			return hErr
		}
		if hErr := os.WriteFile(handoff, append(raw, '\n'), 0o600); hErr != nil {
			return hErr
		}
	}
	if totalFindings > 0 {
		fmt.Fprintf(out, "\ndrift found — failing the gate (exit 1)\n")
		os.Exit(1)
	}
	fmt.Fprintln(out, "clean — no drift against frozen baselines")
	return nil
}
