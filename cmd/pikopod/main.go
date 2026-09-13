// pikopod — sandbox, scenario-test and drift-watch API integrations. Everything
// runs locally: no accounts, no egress except Slack and your own LLM key.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:           "pikopod",
		Short:         "Sandbox, scenario-test, and drift-watch your third-party API integrations — locally",
		Version:       currentVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Persistent so every subcommand inherits it: a user who keeps
	// pikopod.yaml outside the working directory should say so once.
	root.PersistentFlags().String("config", "", "path to pikopod.yaml (default: ./pikopod.yaml)")

	root.AddCommand(
		newDemoCmd(),
		newInitCmd(),
		newImportCmd(),
		newUpCmd(),
		newSandboxCmd(),
		newScenarioCmd(),
		newContractCmd(),
		newWhyCmd(),
		newConformanceCmd(),
		newSpecDiffCmd(),
		newSpecUpdateCmd(),
		newPRCmd(),
		newFixCmd(),
		newVolatileCmd(),
		newReplayCmd(),
		newChaosCmd(),
		newBaselineCmd(),
		newAckCmd(),
		newAcceptCmd(),
		newReportCmd(),
		newDoctorCmd(),
		newInspectCmd(),
		newStatusCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2) // 2 = pikopod/config error; drift gates use 1 (see docs/exit-codes.md)
	}
}
