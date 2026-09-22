package main

import (
	"fmt"
	"io"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newReproduceCmd() *cobra.Command {
	c := &cobra.Command{Use: "reproduce <fingerprint | bundle.json>",
		Short: "Reproduce a recorded production failure as a scenario against the sandbox",
		Long: `Reproduce a recorded production failure as a scenario against the sandbox.

The argument is a fingerprint from the local event log, or the path of a bundle
written by pikopod incidents export on the host that recorded the incident. A
bundle carries the event, the already-redacted recording and the contract
version, so nothing is read from this data_dir; once exported it keeps working
after the origin host's retention has aged the incident out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			ev, rec, pinVersion, err := incidentInputs(cfg, args[0], out)
			if err != nil {
				return err
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
				return errfmt.New("the sandbox did not reproduce this failure",
					"the pack ran but the recorded failure did not recur, so pikopod cannot stand behind it as a reproduction",
					"check the pack against what your code actually sends — the recorded body is redacted, and a 4xx usually turns on it",
					"docs/exit-codes.md")
			case scenario.RunErrored:
				return errfmt.Newf("the reproduction could not run", "fix the pack or the sandbox, then re-run",
					"scenarios/README.md", "%s", res.Summary)
			default:
				fmt.Fprintf(out, "the failure now happens locally — fix it, then re-run: pikopod scenario run %s %s\n", sandboxName, name)
			}
			return nil
		}}
	c.Flags().String("sandbox", "", "sandbox to replay against (default: the incident's upstream)")
	return c
}

func incidentInputs(cfg *config.Config, arg string, out io.Writer) (*alert.DriftEvent, *proxy.Record, int, error) {
	if bridge.IsBundleArg(arg) {
		b, err := bridge.LoadBundle(arg)
		if err != nil {
			return nil, nil, 0, err
		}
		fmt.Fprintf(out, "bundle %s: %s exported %s from %s\n", arg, b.Event.Fingerprint, b.ExportedAt.Format(time.RFC3339), b.Source.Host)
		return &b.Event, &b.Recording, b.ContractVersion, nil
	}
	ev, err := bridge.FindEvent(cfg.DataDir, arg)
	if err != nil {
		return nil, nil, 0, err
	}
	rec, err := bridge.FindRecording(cfg.DataDir, ev)
	if err != nil {
		return nil, nil, 0, err
	}
	version := 0
	if ov, _ := contract.LoadOverlay(cfg.DataDir, ev.Upstream); ov != nil {
		version = ov.Version
	}
	return ev, rec, version, nil
}
