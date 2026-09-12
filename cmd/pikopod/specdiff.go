package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/pikopod/pikopod/internal/specwatch"
	"github.com/spf13/cobra"
)

// newSpecDiffCmd diffs two spec versions as IR-vs-IR findings. Exit codes are the
// API: 0 = nothing at/above the --fail-on floor, 1 = findings, 2 = config error.
func newSpecDiffCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "spec-diff <old> <new>",
		Short: "Diff two API spec versions — declared drift, severity derived by law (exit 0 clean / 1 breaking / 2 error)",
		Long: "Sources: a local file, an http(s) URL, or git:<ref>:<path> (read via `git show`, no checkout).\n" +
			"Both sides are normalized to pikopod's IR first, so OpenAPI 3.x, Swagger 2.0 and Postman\n" +
			"collections all diff — even against each other.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, _ := cmd.Flags().GetString("format")
			failOn, _ := cmd.Flags().GetString("fail-on")
			floor, err := parseLevelFloor(failOn)
			if err != nil {
				return err
			}
			oldDef, err := loadSpecIR(args[0])
			if err != nil {
				return err
			}
			newDef, err := loadSpecIR(args[1])
			if err != nil {
				return err
			}
			findings := specdiff.Diff(oldDef, newDef)
			report := specdiff.BuildReport(args[0], args[1], findings)
			if handoff, _ := cmd.Flags().GetString("handoff"); handoff != "" {
				f, hErr := os.Create(handoff)
				if hErr != nil {
					return hErr
				}
				if hErr := report.WriteJSON(f); hErr != nil {
					f.Close()
					return hErr
				}
				f.Close()
			}
			out := cmd.OutOrStdout()
			switch format {
			case "json":
				if err := report.WriteJSON(out); err != nil {
					return err
				}
			case "markdown":
				report.WriteMarkdown(out)
			case "text", "":
				report.WriteText(out)
			default:
				return errfmt.New("unknown --format "+format, "spec-diff renders text (default), json, or markdown", "e.g. pikopod spec-diff old.yaml new.yaml --format markdown", "")
			}
			if specdiff.Breaking(findings, floor) {
				// The report above IS the explanation; CI keys on the code.
				if format == "text" || format == "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "\nbreaking declared drift at/above %s — failing the gate (exit 1)\n", floor)
				}
				os.Exit(1) // 1 = drift, distinct from 2 = tool error (docs/exit-codes.md)
			}
			return nil
		}}
	c.Flags().String("format", "text", "output format: text | json | markdown")
	c.Flags().String("fail-on", "ERR", "exit 1 when findings at/above this level exist: ERR | WARN | INFO")
	c.Flags().String("handoff", "", "also write the JSON report here (for `pikopod pr comment`)")
	return c
}

func parseLevelFloor(s string) (specdiff.Level, error) {
	switch strings.ToUpper(s) {
	case "ERR":
		return specdiff.Err, nil
	case "WARN":
		return specdiff.Warn, nil
	case "INFO":
		return specdiff.Info, nil
	}
	return "", errfmt.New("unknown --fail-on "+s, "the floor is one of ERR, WARN, INFO", "e.g. --fail-on WARN", "")
}

// loadSpecIR normalizes a spec to IR, loading it through specwatch.Fetch — ONE
// implementation of the git-show/URL/file dispatch and its flag-smuggling guard.
func loadSpecIR(source string) (*ir.ApiDefinition, error) {
	raw, _, _, err := specwatch.Fetch(source, "")
	if err != nil {
		return nil, err
	}
	def, err := importer.NormalizeOpenAPI(raw)
	if err != nil {
		return nil, errfmt.Newf("cannot normalize "+source, "the document did not parse as a supported spec format", "docs/config-reference.md", "%v", err)
	}
	return def, nil
}
