// `pikopod contract <sandbox>` — traffic-admitted additions, spec-vs-traffic
// disagreements (both sides kept), and pin staleness (reported, never "fixed").
package main

import (
	"fmt"
	"io"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/spf13/cobra"
)

func newContractCmd() *cobra.Command {
	return &cobra.Command{Use: "contract <sandbox>", Short: "Show the effective contract: traffic-admitted additions, spec-vs-traffic contradictions, pin staleness", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return contractReport(cfg, args[0], cmd.OutOrStdout())
		}}
}

func contractReport(cfg *config.Config, sandboxName string, out io.Writer) error {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return err
	}
	entry := findEntry(entries, sandboxName)
	if entry == nil {
		return errfmt.New("unknown sandbox: "+sandboxName, "nothing registered under that name", "see `pikopod sandbox list`", "")
	}
	upstream := entry.Upstream
	if upstream == "" {
		upstream = entry.Name
	}
	ov, err := contract.LoadOverlay(cfg.DataDir, upstream)
	if err != nil {
		return err
	}
	if ov == nil || ov.Version == 0 {
		fmt.Fprintf(out, "%s: spec contract only — no traffic admissions yet\n", sandboxName)
		fmt.Fprintln(out, "enable `refine.enabled: true` and run real traffic through the agent; matured observations are admitted on persist ticks (see docs/config-reference.md#refine)")
		return nil
	}

	fmt.Fprintf(out, "%s — effective contract = spec + traffic overlay (upstream %q)\n", sandboxName, upstream)
	fmt.Fprintf(out, "overlay version: %d (sandboxes serve the latest; every response carries x-pikopod-contract-version)\n\n", ov.Version)

	fmt.Fprintln(out, "OBSERVED additions (traffic-admitted; the sandbox renders these):")
	for _, a := range ov.Admissions {
		loc := fmt.Sprintf("%s %s %s", a.Method, a.Template, a.StatusClass)
		switch a.Kind {
		case contract.AdmitField:
			typ := a.Type
			if typ == "" {
				// Presence came from redaction pointers; the sanitizer removed
				// the value before the refiner saw it, so the type is unknown.
				typ = "type unknown (value sanitized)"
			}
			fmt.Fprintf(out, "  v%-3d field     %-40s %s (%s, presence %.2f)\n", a.Version, loc, a.Field, typ, a.Presence)
		case contract.AdmitValue:
			fmt.Fprintf(out, "  v%-3d value     %-40s %s += %q\n", a.Version, loc, a.Field, a.Value)
		case contract.AdmitType:
			fmt.Fprintf(out, "  v%-3d type      %-40s %s → %s (traffic won; see contradictions)\n", a.Version, loc, a.Field, a.Type)
		case contract.AdmitStatus:
			fmt.Fprintf(out, "  v%-3d status    %-40s += %s\n", a.Version, loc, a.Value)
		case contract.AdmitEndpoint:
			fmt.Fprintf(out, "  v%-3d endpoint  %-40s (spec never declared this route)\n", a.Version, loc)
		}
	}
	if len(ov.Admissions) == 0 {
		fmt.Fprintln(out, "  (none — observations exist but nothing has cleared the admission gates yet)")
	}

	if len(ov.Contradictions) > 0 {
		fmt.Fprintln(out, "\ncontradictions (spec vs traffic — both sides kept, never erased):")
		for _, c := range ov.Contradictions {
			fmt.Fprintf(out, "  %s %s %s  %s: spec says %s, traffic says %s (%.0f%% of clean samples) — winner: %s\n",
				c.Method, c.Template, c.StatusClass, c.Field, c.SpecClaim, c.Observed, c.Rate*100, c.Winner)
		}
	}

	// From-drift pins resolve at their pin-time version forever; report how
	// far the live contract has moved past each so staleness is actionable.
	packs, packFails := scenario.ListPacks(packDirs(cfg)...)
	for path, ferr := range packFails {
		fmt.Fprintf(out, "\nwarning: could not read pack %s (%v) — its pin, if any, is not shown\n", path, ferr)
	}
	var pinned []*scenario.Pack
	for _, pk := range packs {
		if pk.ContractVersion > 0 {
			pinned = append(pinned, pk)
		}
	}
	if len(pinned) > 0 {
		fmt.Fprintln(out, "\npinned scenarios (immovable — each runs at its pin-time contract):")
		for _, pk := range pinned {
			behind := ov.Version - pk.ContractVersion
			state := "current"
			if behind > 0 {
				state = fmt.Sprintf("%d version(s) behind", behind)
			}
			fmt.Fprintf(out, "  %-30s pinned at v%d — %s\n", pk.Name, pk.ContractVersion, state)
		}
	}
	fmt.Fprintln(out, "\nadmissions happen automatically on agent persist ticks; accepted drift re-freezes via `pikopod accept <fingerprint>`")
	return nil
}
