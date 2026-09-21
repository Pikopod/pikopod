package main

import (
	"fmt"
	"io"
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
			"collections all diff — even against each other. A spec split across files works when the\n" +
			"source is a file or a git ref: same-repository relative $refs resolve at the same ref; remote\n" +
			"and absolute $refs are always refused.\n\n" +
			"Exit 0 when nothing is at or above --fail-on, 1 when something is, 2 when a document cannot be\n" +
			"loaded or normalized (never 1: a spec that fails to load is a tool problem, not drift).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, _ := cmd.Flags().GetString("format")
			failOn, _ := cmd.Flags().GetString("fail-on")
			handoff, _ := cmd.Flags().GetString("handoff")
			breaking, err := specDiff(args[0], args[1], specDiffOptions{Format: format, FailOn: failOn, Handoff: handoff}, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if breaking {
				os.Exit(1)
			}
			return nil
		}}
	c.Flags().String("format", "text", "output format: text | json | markdown | githubactions")
	c.Flags().String("fail-on", "ERR", "exit 1 when findings at/above this level exist: ERR | WARN | INFO")
	c.Flags().String("handoff", "", "also write the JSON report here (for `pikopod pr comment`)")
	return c
}

type specDiffOptions struct {
	Format  string
	FailOn  string
	Handoff string
}

func specDiff(oldSrc, newSrc string, o specDiffOptions, out, errOut io.Writer) (bool, error) {
	floor, err := parseLevelFloor(o.FailOn)
	if err != nil {
		return false, err
	}
	switch o.Format {
	case "json", "markdown", "githubactions", "text", "":
	default:
		return false, errfmt.New("unknown --format "+o.Format, "spec-diff renders text (default), json, markdown, or githubactions", "e.g. pikopod spec-diff old.yaml new.yaml --format githubactions", "")
	}
	oldDef, oldPos, err := loadSpecIR(oldSrc)
	if err != nil {
		return false, err
	}
	newDef, newPos, err := loadSpecIR(newSrc)
	if err != nil {
		return false, err
	}
	findings := specdiff.Diff(oldDef, newDef)
	report := specdiff.BuildReport(oldSrc, newSrc, findings)
	if o.Handoff != "" {
		f, hErr := os.Create(o.Handoff)
		if hErr != nil {
			return false, hErr
		}
		if hErr := report.WriteJSON(f); hErr != nil {
			f.Close()
			return false, hErr
		}
		f.Close()
	}
	switch o.Format {
	case "json":
		if err := report.WriteJSON(out); err != nil {
			return false, err
		}
	case "markdown":
		report.WriteMarkdown(out)
	case "githubactions":
		report.WriteGitHubActions(out, specdiff.Positions{New: newPos, Old: oldPos})
	default:
		report.WriteText(out)
	}
	breaking := specdiff.Breaking(findings, floor)
	if breaking && (o.Format == "text" || o.Format == "") {
		fmt.Fprintf(errOut, "\nbreaking declared drift at/above %s — failing the gate (exit 1)\n", floor)
	}
	return breaking, nil
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

func loadSpecIR(source string) (*ir.ApiDefinition, ir.Positions, error) {
	raw, _, _, err := specwatch.Fetch(source, "")
	if err != nil {
		return nil, nil, err
	}
	def, pos, err := importer.NormalizeOpenAPIFrom(raw, specwatch.OriginOf(source).ImporterSource())
	if err != nil {
		return nil, nil, errfmt.Newf("cannot normalize "+source, "the document did not parse as a supported spec format", "docs/config-reference.md#ref-policy", "%v", err)
	}
	return def, pos, nil
}
