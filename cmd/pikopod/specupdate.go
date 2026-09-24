package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pikopod/pikopod/internal/conformance"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/specupdate"
	"github.com/spf13/cobra"
)

func newSpecUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "spec-update <upstream>",
		Short: "Write a traffic-evidenced spec update: additive patches applied in place, narrowings as suggestions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			upstream := args[0]
			outPath, _ := cmd.Flags().GetString("out")
			handoffPath, _ := cmd.Flags().GetString("handoff")
			specOverride, _ := cmd.Flags().GetString("spec")

			def, err := irForUpstream(cfg, upstream)
			if err != nil {
				return err
			}

			var report *conformance.Report
			if records, _ := readRecordings(cfg.DataDir, upstream); len(records) > 0 {
				report = conformance.Check(def, records)
			}

			overlay, _ := contract.LoadOverlay(cfg.DataDir, upstream)

			changes := specupdate.DeriveChanges(report, overlay)
			if len(changes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no traffic-evidenced changes — the spec and the wire agree")
				return nil
			}

			specSource := specOverride
			if specSource == "" {
				entries, rErr := loadRegistry(cfg.DataDir)
				if rErr != nil {
					return rErr
				}
				if e := findEntry(entries, upstream); e != nil {
					specSource = e.SpecSource
				} else {
					for i := range entries {
						if entries[i].Upstream == upstream {
							specSource = entries[i].SpecSource
							break
						}
					}
				}
			}
			if specSource == "" {
				return errfmt.New("no spec source for "+upstream, "spec-update patches the ORIGINAL document and needs its location", "pass --spec <file-or-url>, or re-import with a recorded source", "")
			}
			raw, err := loadSpecOnce(specSource)
			if err != nil {
				return err
			}

			res, err := specupdate.Apply(raw, changes)
			if err != nil {
				return err
			}

			stderr := cmd.ErrOrStderr()
			fmt.Fprintf(stderr, "%d change(s): %d applied (additive), %d suggestion(s) (narrowing — never auto-applied), %d skipped (no document anchor)\n",
				len(changes), len(res.Applied), len(res.Suggestions), len(res.Skipped))
			for _, p := range res.Applied {
				fmt.Fprintf(stderr, "  APPLIED    %-14s %s %s  %s\n", p.Kind, p.Method, p.Template, p.Reason)
			}
			for _, p := range res.Suggestions {
				fmt.Fprintf(stderr, "  SUGGESTION %-14s %s %s  %s\n", p.Kind, p.Method, p.Template, p.Reason)
			}
			for _, p := range res.Skipped {
				fmt.Fprintf(stderr, "  SKIPPED    %-14s %s %s  %s\n", p.Kind, p.Method, p.Template, p.Reason)
			}

			if handoffPath != "" {
				handoff, mErr := json.MarshalIndent(map[string]any{
					"source": "spec-update", "upstream": upstream, "spec_source": specSource,
					"applied": res.Applied, "suggestions": res.Suggestions, "skipped": res.Skipped,
				}, "", "  ")
				if mErr != nil {
					return mErr
				}
				if wErr := os.WriteFile(handoffPath, append(handoff, '\n'), 0o600); wErr != nil {
					return wErr
				}
				fmt.Fprintf(stderr, "handoff written to %s (consume with `pikopod pr comment/open`)\n", handoffPath)
			}

			if outPath != "" {
				if wErr := os.WriteFile(outPath, res.Out, 0o600); wErr != nil {
					return wErr
				}
				fmt.Fprintf(stderr, "patched spec written to %s — diff it against the original before committing\n", outPath)
				return nil
			}

			_, err = cmd.OutOrStdout().Write(res.Out)
			return err
		}}
	c.Flags().String("out", "", "write the patched spec here (default: stdout)")
	c.Flags().String("handoff", "", "write the JSON patch handoff here (for `pikopod pr comment/open`)")
	c.Flags().String("spec", "", "document to patch (default: the source the sandbox was imported from)")
	return c
}
