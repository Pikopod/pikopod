package main

import (
	"fmt"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/volatile"
	"github.com/spf13/cobra"
)

func newVolatileCmd() *cobra.Command {
	c := &cobra.Command{Use: "volatile", Short: "Manage volatile-field noise control (suggest entries, lint dead ones)"}

	suggest := &cobra.Command{
		Use:   "suggest <upstream>",
		Short: "Suggest volatile_fields entries from recorded churn — or refuse with a typed reason",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			upstream := args[0]
			records, err := readRecordings(cfg.DataDir, upstream)
			if err != nil || len(records) == 0 {
				return errfmt.New("no recordings for "+upstream, "the agent has not recorded traffic yet", "run `pikopod up`, send traffic through it, then re-run", "docs/config-reference.md#data_dir")
			}
			an := volatile.Analyze(records, cfg.Upstreams[upstream].VolatileFields)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%d recording(s) analyzed\n", an.Records)
			m, compileRefusals, err := volatile.Compile(cfg.Upstreams[upstream].VolatileFields)
			if err != nil {
				return err
			}
			an.Refusals = append(an.Refusals, compileRefusals...)
			an.Refusals = append(an.Refusals, volatile.Collateral(baseline.NewLearner(upstream, cfg.DataDir, baseline.Warmup{}), m)...)

			if len(an.Suggestions) == 0 {
				fmt.Fprintln(out, "\nno suggestions — nothing churns that a volatile_fields entry could soundly silence")
			} else {
				fmt.Fprintf(out, "\n%d suggestion(s) — every field carrying the name is proven churn:\n", len(an.Suggestions))
				for _, s := range an.Suggestions {
					fmt.Fprintf(out, "  %-24s %d/%d distinct values across %v\n", s.Name, s.Distinct, s.Samples, s.Paths)
				}
				fmt.Fprintf(out, "add the ones you agree with under upstreams.%s.volatile_fields in pikopod.yaml\n", upstream)
			}

			if len(an.Refusals) > 0 {
				fmt.Fprintf(out, "\n%d refusal(s) — churn that did NOT become a suggestion, with why:\n", len(an.Refusals))
				for _, r := range an.Refusals {
					fmt.Fprintf(out, "  %-24s %-20s %s (%s)\n", r.Name, r.Reason, r.Detail, r.Path)
				}
				fmt.Fprintln(out, "a refused case stays visible with a reason; an over-broad entry goes green while quietly asserting less")
			}

			if len(an.DeadEntries) > 0 {
				fmt.Fprintf(out, "\n%d DEAD configured entr(ies) — matching nothing in %d recent recording(s):\n", len(an.DeadEntries), an.Records)
				for _, d := range an.DeadEntries {
					fmt.Fprintf(out, "  %s\n", d.Name)
				}
				fmt.Fprintln(out, "a volatile_fields entry that matches nothing means you believe a field is excluded while it is still asserted — remove or fix it")
			}
			return nil
		}}

	c.AddCommand(suggest)
	return c
}
