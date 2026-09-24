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
		newModeCmd(),
		newWebhookCmd(),
		newMCPCmd(),
		newBaselineCmd(),
		newAckCmd(),
		newAcceptCmd(),
		newReportCmd(),
		newIncidentsCmd(),
		newDoctorCmd(),
		newInspectCmd(),
		newStatusCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}
