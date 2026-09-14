package main

import (
	"fmt"

	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newReproduceCmd() *cobra.Command {
	c := &cobra.Command{Use: "reproduce <fingerprint>",
		Short: "Reproduce a recorded production failure as a scenario against the sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			ev, err := bridge.FindEvent(cfg.DataDir, args[0])
			if err != nil {
				return err
			}
			rec, err := bridge.FindRecording(cfg.DataDir, ev)
			if err != nil {
				return err
			}
			pinVersion := 0
			if ov, _ := contract.LoadOverlay(cfg.DataDir, ev.Upstream); ov != nil {
				pinVersion = ov.Version
			}
			name, pack, err := bridge.BuildFromRecord(ev, rec, pinVersion)
			if err != nil {
				return err
			}
			rendered, err := yaml.Marshal(pack)
			if err != nil {
				return err
			}
			path, err := bridge.Save(cfg.DataDir, name, rendered)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "reproduced %s (%s answered %d on %s %s) as %s\n",
				ev.Fingerprint, ev.Upstream, rec.Status, rec.Method, ev.Endpoint, path)

			sandboxName, _ := cmd.Flags().GetString("sandbox")
			if sandboxName == "" {
				sandboxName = ev.Upstream
			}
			entry, def, err := loadSandboxDef(cfg, sandboxName)
			if err != nil {
				fmt.Fprintf(out, "no sandbox to replay against (%v)\nrun it later: pikopod scenario run <sandbox> %s\n", err, name)
				return nil
			}
			parsed, err := resolveRunnable(cfg, name, def, nil)
			if err != nil {
				return err
			}
			eng, done, err := scenarioEngine(cfg, entry, def, false, pinVersion)
			if err != nil {
				return err
			}
			res, err := scenario.Run(eng, parsed, nil, entry.Seed)
			done()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s — %s\n", res.Status, res.Summary)
			return nil
		}}
	c.Flags().String("sandbox", "", "sandbox to replay against (default: the incident's upstream)")
	return c
}
