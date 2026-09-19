// from-drift: DriftEvent → pinned-baseline scenario → immediate replay. The pack
// lands in <data_dir>/scenarios so it keeps running in CI after this one-shot.
package main

import (
	"fmt"
	"os"

	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newFromDriftCmd() *cobra.Command {
	c := &cobra.Command{Use: "from-drift <fingerprint>", Short: "Pin a drift's baseline contract as a scenario and replay it against the sandbox", Args: cobra.ExactArgs(1),
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
			pinVersion := 0
			if ov, _ := contract.LoadOverlay(cfg.DataDir, ev.Upstream); ov != nil {
				pinVersion = ov.Version
			}
			name, pack, err := bridge.Build(ev, pinVersion)
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
			fmt.Fprintf(out, "pinned %s (%s %s) as %s\n", ev.Fingerprint, ev.Method, ev.Endpoint, path)

			sandboxName, _ := cmd.Flags().GetString("sandbox")
			if sandboxName == "" {
				sandboxName = ev.Upstream
			}
			entry, def, err := loadSandboxDef(cfg, sandboxName)
			if err != nil {
				fmt.Fprintf(out, "no sandbox to replay against (%v)\nrun it later: pikopod scenario run <sandbox> %s\n", err, name)
				return nil
			}
			parsed, _, err := resolveRunnable(cfg, name, def, nil)
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
			switch res.Status {
			case scenario.RunFailed:
				fmt.Fprintln(out, "the sandbox already reproduces the drifted behavior — your integration breaks HERE, not in production (exit 1)")
				os.Exit(1)
			case scenario.RunErrored:
				return fmt.Errorf("replay errored: %s", res.Summary)
			default:
				fmt.Fprintf(out, "baseline pinned and green — a future re-import that adopts this change will fail `pikopod scenario run %s %s`\n", sandboxName, name)
			}
			return nil
		}}
	c.Flags().String("sandbox", "", "sandbox to replay against (default: the event's upstream name)")
	return c
}
