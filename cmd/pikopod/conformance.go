package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/conformance"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/spf13/cobra"
)

func newConformanceCmd() *cobra.Command {
	c := &cobra.Command{Use: "conformance <upstream>", Short: "Validate recorded traffic against the spec: does the provider obey its own docs?", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			strict, _ := cmd.Flags().GetBool("strict")
			upstream := args[0]

			def, err := irForUpstream(cfg, upstream)
			if err != nil {
				return err
			}
			records, err := readRecordings(cfg.DataDir, upstream)
			if err != nil {
				return err
			}
			if len(records) == 0 {
				return errfmt.New("no recordings for "+upstream, "the agent has not recorded traffic yet", "run `pikopod up`, send traffic through it, then re-run", "docs/config-reference.md#data_dir")
			}

			report := conformance.Check(def, records)
			if handoff, _ := cmd.Flags().GetString("handoff"); handoff != "" {
				raw, hErr := json.MarshalIndent(map[string]any{
					"source": "conformance", "upstream": upstream,
					"records": report.Records, "violations": report.Violations,
				}, "", "  ")
				if hErr != nil {
					return hErr
				}
				if hErr := os.WriteFile(handoff, append(raw, '\n'), 0o600); hErr != nil {
					return hErr
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s — %d JSON response(s) checked against the spec (%d skipped: non-JSON or no declared endpoint)\n",
				upstream, report.Records, report.Skipped)
			if report.Unverifiable > 0 {
				fmt.Fprintf(out, "%d check(s) unverifiable: the sanitizer redacted the evidence — reported as neither pass nor violation\n", report.Unverifiable)
			}
			if len(report.Violations) == 0 {
				fmt.Fprintln(out, "no spec violations found")
				return nil
			}
			fmt.Fprintf(out, "\n%d violation(s) — the provider disagrees with its own documentation:\n", len(report.Violations))
			errors := 0
			for _, v := range report.Violations {
				if v.Severity == "error" {
					errors++
				}
				loc := v.Pointer
				if loc == "" {
					loc = "-"
				}
				fmt.Fprintf(out, "  %-7s %-9s %s %s %d  %-24s %s (%d occurrence(s))\n",
					strings.ToUpper(v.Severity), v.Code, v.Method, v.Template, v.Status, loc, v.Message, v.Occurrences)
			}
			fmt.Fprintln(out, "\nthese are SPEC-relative findings; `pikopod contract` shows what traffic has taught the sandbox instead")
			if strict && errors > 0 {
				fmt.Fprintf(out, "%d error-severity violation(s) — failing (--strict, exit 1)\n", errors)
				os.Exit(1)
			}
			return nil
		}}
	c.Flags().Bool("strict", false, "exit 1 when any error-severity violation is found (CI gate)")
	c.Flags().String("handoff", "", "also write the JSON report here (for `pikopod pr comment`)")
	return c
}

func irForUpstream(cfg *config.Config, upstream string) (*ir.ApiDefinition, error) {
	entries, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		e := &entries[i]
		if e.Upstream != upstream && e.Name != upstream {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(cfg.DataDir, e.IRFile))
		if err != nil {
			return nil, errfmt.Newf("cannot read the persisted IR for "+e.Name, "re-add the sandbox with `pikopod sandbox add`", "docs/config-reference.md#data_dir", "%v", err)
		}
		var def ir.ApiDefinition
		if err := json.Unmarshal(raw, &def); err != nil {
			return nil, errfmt.Newf("persisted IR for "+e.Name+" is corrupt", "re-add the sandbox", "docs/config-reference.md#data_dir", "%v", err)
		}
		return &def, nil
	}
	return nil, errfmt.New("no sandbox linked to upstream "+upstream, "conformance needs the spec IR of a registered sandbox with the same name (or an explicit --upstream link)", "import one: `pikopod import "+upstream+" --spec …`", "docs/config-reference.md#refine")
}

func readRecordings(dataDir, upstream string) ([]*proxy.Record, error) {
	var out []*proxy.Record
	base := filepath.Join(dataDir, "recordings", upstream+".ndjson")
	for _, path := range []string{base + ".1", base} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var rec proxy.Record
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.UseNumber()
			if dec.Decode(&rec) == nil {
				out = append(out, &rec)
			}
		}
		f.Close()
	}
	return out, nil
}
