// `pikopod scenario from-recordings <upstream>` — the from-drift bridge's
// from-traffic sibling: a scenario pack built from a recorded traffic window.
package main

import (
	"fmt"

	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newFromRecordingsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "from-recordings <upstream>",
		Short: "Generate a scenario pack from recorded traffic (ordered requests, status assertions, value chains)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			upstream := args[0]
			out := cmd.OutOrStdout()
			records, err := readRecordings(cfg.DataDir, upstream)
			if err != nil || len(records) == 0 {
				return errfmt.New("no recordings for "+upstream, "the agent has not recorded traffic yet", "run `pikopod up`, send traffic through it, then re-run", "docs/config-reference.md#data_dir")
			}
			last, _ := cmd.Flags().GetInt("last")
			packName, _ := cmd.Flags().GetString("name")
			name, pack, err := bridge.BuildFromRecordings(upstream, records, bridge.FromRecordingsOptions{Last: last, Name: packName})
			if err != nil {
				return err
			}
			// Validate before saving — a generated pack that does not parse
			// is a bug, never the user's problem.
			if _, errs := scenario.ParseDefinition(pack["definition"]); len(errs) > 0 {
				return errfmt.Newf("generated pack failed validation (pikopod bug)", "please report this with the output", "https://github.com/pikopod/pikopod/issues", "%v", errs[0].Message)
			}
			rendered, err := yaml.Marshal(pack)
			if err != nil {
				return err
			}
			path, err := bridge.Save(cfg.DataDir, name, rendered)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "generated %s from %d recording(s): %s\n", name, len(records), path)
			fmt.Fprintf(out, "run it: pikopod scenario run %s %s\n", upstream, name)
			fmt.Fprintln(out, "chains are conservative (identifier-shaped, produced-before-consumed only) — review the pack before trusting it in CI")
			return nil
		}}
	c.Flags().Int("last", 0, "cap the window to the last N recordings (default 20)")
	c.Flags().String("name", "", "pack slug (default traffic-<upstream>)")
	return c
}
