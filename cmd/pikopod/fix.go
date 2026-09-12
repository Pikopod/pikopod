// `pikopod fix <fingerprint>` — drift becomes a code change: scan → LLM patch →
// check → PR. Without --pr edits stay in the working tree, inspectable first.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/fix"
	"github.com/spf13/cobra"
)

func newFixCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fix <fingerprint>",
		Short: "Turn a drift event into a code change in your repo (scan → LLM patch → check → PR)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			dir, _ := cmd.Flags().GetString("dir")
			check, _ := cmd.Flags().GetString("check")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			openPR, _ := cmd.Flags().GetBool("pr")

			ev, err := bridge.FindEvent(cfg.DataDir, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "drift %s: %s %s — %s %s\n", ev.Fingerprint, ev.Method, ev.Endpoint, ev.Kind, ev.Field)

			terms := fix.Terms(ev)
			impacts, err := fix.Scan(dir, terms)
			if err != nil {
				return errfmt.Newf("impact scan failed", "check --dir points at your repository", "docs/config-reference.md#fix", "%v", err)
			}
			if len(impacts) == 0 {
				// UNVERIFIABLE, never clean: the scan matches source text literally, so
				// exiting 0 here would hand CI a green gate on a silent miss.
				return errfmt.New(
					"impact for "+ev.Fingerprint+" is UNVERIFIABLE",
					"the scan matches source text literally and case-sensitively and found no occurrence of "+strings.Join(terms, ", ")+" under "+dir+" — a client that renames the field (accountNumber for account_number), indexes it dynamically, or forwards the payload untouched is invisible to it",
					"re-run with --dir <path> if the integration lives elsewhere, or search for the field yourself; do not read this as proof the field is unused",
					"docs/config-reference.md#fix")
			}
			fmt.Fprintf(out, "impact: %d file(s) reference %s\n", len(impacts), strings.Join(terms, ", "))
			for _, im := range impacts {
				fmt.Fprintf(out, "  %s\n", im.File)
			}

			key := cfg.LLM.OpenRouterKey
			if key == "" {
				return errfmt.New(
					"drift-to-code fixing needs your LLM key",
					"no OpenRouter key is configured (llm.openrouter_key / PIKOPOD_OPENROUTER_KEY)",
					"add your own key; the impact scan above is the deterministic half and already ran",
					"docs/config-reference.md#llm")
			}
			client := newLLMClient(cfg, "")
			client.MaxTokens = 8192

			payload, err := json.Marshal(map[string]any{"drift": ev, "impacts": impacts})
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			raw, err := client.CompleteJSON(ctx, fix.Instruction, string(payload))
			if err != nil {
				return err
			}
			prop, err := fix.ParseProposal(raw, impacts)
			if err != nil {
				return err
			}
			if len(prop.Edits) == 0 {
				fmt.Fprintf(out, "the model found no safe change: %s\n", prop.Summary)
				return nil
			}

			if dryRun {
				fmt.Fprintf(out, "proposed (dry run — nothing written):\n")
				for _, e := range prop.Edits {
					fmt.Fprintf(out, "--- %s\n-%s\n+%s\n", e.File, indentLines(e.Find), indentLines(e.Replace))
				}
				fmt.Fprintf(out, "summary: %s\n", prop.Summary)
				return nil
			}

			changed, revert, err := fix.Apply(dir, prop.Edits)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "applied %d edit(s) to %s\n", len(prop.Edits), strings.Join(changed, ", "))

			if check != "" {
				fmt.Fprintf(out, "check: %s\n", check)
				if err := runCheck(dir, check, cmd.ErrOrStderr()); err != nil {
					if rerr := revert(); rerr != nil {
						return errfmt.Newf("check failed AND revert failed", "restore the files from git", "docs/config-reference.md#fix", "check: %v; revert: %v", err, rerr)
					}
					return errfmt.Newf("the fix did not pass your check — every edit was reverted", "re-run to redraft, or fix by hand using the impact list above", "docs/config-reference.md#fix", "%v", err)
				}
				fmt.Fprintln(out, "check passed")
			}

			if !openPR {
				fmt.Fprintf(out, "fix is in your working tree: %s\nreview it, then carry it: pikopod fix %s --pr   (or commit it yourself)\n", prop.Summary, ev.Fingerprint)
				return nil
			}

			branch, _ := cmd.Flags().GetString("branch")
			base, _ := cmd.Flags().GetString("base")
			title, _ := cmd.Flags().GetString("title")
			if branch == "" {
				branch = "pikopod/fix-" + ev.Fingerprint
			}
			if base == "" {
				base = defaultBaseBranch()
			}
			if strings.HasPrefix(branch, "-") || strings.HasPrefix(base, "-") {
				return errfmt.New("invalid branch name", "branch/base names may not start with '-'", "pick a name like pikopod/fix-fp", "")
			}
			if title == "" {
				title = fmt.Sprintf("pikopod: adapt to %s drift on %s %s", ev.Kind, ev.Method, ev.Endpoint)
			}
			body := fmt.Sprintf(
				"## Drift\n\n| | |\n|---|---|\n| Fingerprint | `%s` |\n| Endpoint | `%s %s` |\n| Kind | `%s` |\n| Field | `%s` |\n| Before → After | `%s` → `%s` |\n| Occurrences | %d |\n\n## Fix\n\n%s\n\nFiles changed: %s\n\n---\n_Opened by `pikopod fix`; the drift above is live provider behavior observed by the agent. Review the diff — the patch was drafted by your configured LLM and gated by %s._",
				ev.Fingerprint, ev.Method, ev.Endpoint, ev.Kind, ev.Field, ev.Before, ev.After, ev.Occurrences,
				prop.Summary, strings.Join(changed, ", "), checkDescription(check))
			steps := [][]string{
				{"git", "checkout", "-b", branch},
				append([]string{"git", "add", "--"}, changed...),
				{"git", "commit", "-m", title},
				{"git", "push", "-u", "origin", branch},
			}
			for _, s := range steps {
				if err := runCmdIn(dir, s); err != nil {
					return err
				}
			}
			forge, _, err := forgeForOpen(cmd)
			if err != nil {
				return err
			}
			url, err := forge.OpenPR(branch, base, title, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "PR opened: %s\n", url)
			return nil
		}}
	addForgeFlags(c)
	c.Flags().String("dir", ".", "repository to scan and fix (default: current directory)")
	c.Flags().String("check", "", "command that must pass after the fix (e.g. 'go build ./...'); failure reverts every edit")
	c.Flags().Bool("dry-run", false, "print the proposed edits without writing anything")
	c.Flags().Bool("pr", false, "branch, commit, push, and open a PR carrying the fix")
	c.Flags().String("branch", "", "PR branch name (default pikopod/fix-<fingerprint>)")
	c.Flags().String("base", "", "base branch (default: origin's HEAD, else main)")
	c.Flags().String("title", "", "PR title")
	return c
}

// runCheck executes the user's verification command through their shell,
// streaming its output — a failing check is the user's signal, show it whole.
func runCheck(dir, check string, stderr io.Writer) error {
	cmd := exec.Command("sh", "-c", check)
	cmd.Dir = dir
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}

// runCmdIn is runCmd with a working directory (the fix's --dir repo).
func runCmdIn(dir string, argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return errfmt.New(strings.Join(argv[:2], " ")+" failed", strings.TrimSpace(errBuf.String()), "fix the git state and retry", "")
	}
	return nil
}

func checkDescription(check string) string {
	if check == "" {
		return "no check command (pass --check to gate future runs)"
	}
	return "`" + check + "`"
}

func indentLines(s string) string {
	return strings.ReplaceAll(s, "\n", "\n ")
}
