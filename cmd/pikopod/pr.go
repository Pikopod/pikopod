// The PR surface, two-step by design: finding commands write a JSON handoff,
// these consume it. Auth comes from the env or gh — tokens NEVER reach argv.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/pr"
	"github.com/spf13/cobra"
)

func newPRCmd() *cobra.Command {
	c := &cobra.Command{Use: "pr", Short: "Post findings to a pull request (comment, update-in-place) or open one carrying a fix"}
	c.AddCommand(newPRCommentCmd(), newPROpenCmd())
	return c
}

func newPRCommentCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "comment",
		Short: "Post ONE marker-tagged comment from a handoff file, updated in place on re-runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			handoffPath, _ := cmd.Flags().GetString("handoff")
			if handoffPath == "" {
				return errfmt.New("no handoff given", "pr comment consumes a JSON handoff written by a finding command", "e.g. pikopod spec-diff old new --format json > h.json && pikopod pr comment --handoff h.json", "docs/config-reference.md#pr")
			}
			raw, err := os.ReadFile(handoffPath)
			if err != nil {
				return errfmt.Newf("cannot read the handoff", "check the path", "", "%v", err)
			}
			source, markdown, err := pr.RenderHandoff(raw)
			if err != nil {
				return errfmt.Newf("handoff not renderable", "the file must come from a pikopod finding command", "", "%v", err)
			}

			forge, sha, err := forgeFromFlags(cmd)
			if err != nil {
				return err
			}
			action, upErr := pr.UpsertComment(forge, source, sha, markdown, pr.GitIsAncestor)
			if upErr == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "comment %s (source %s, %s)\n", action, source, forge.Name())
				return nil
			}

			// Degradation ladder for read-only tokens: comment → job summary
			// → stderr notice with the markdown on stdout.
			if fe, ok := upErr.(*pr.ForgeError); ok && fe.ReadOnly() {
				if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
					f, ferr := os.OpenFile(summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
					if ferr == nil {
						fmt.Fprintln(f, markdown)
						f.Close()
						fmt.Fprintf(cmd.ErrOrStderr(), "token cannot comment (%v) — wrote the report to the job summary instead\n", upErr)
						return nil
					}
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "token cannot comment (%v) — report follows on stdout\n", upErr)
				fmt.Fprintln(cmd.OutOrStdout(), markdown)
				return nil
			}
			return upErr
		}}
	addForgeFlags(c)
	c.Flags().String("handoff", "", "JSON handoff file from a finding command")
	return c
}

func newPROpenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "open",
		Short: "Branch, commit given files, push, and open a PR carrying the evidence",
		RunE: func(cmd *cobra.Command, args []string) error {
			files, _ := cmd.Flags().GetStringArray("commit")
			if len(files) == 0 {
				return errfmt.New("nothing to commit", "pr open carries a fix: a spec-update artifact or a scenario pack", "pass --commit <path> (repeatable) for files you already updated (e.g. via spec-update --out)", "docs/config-reference.md#pr")
			}
			handoffPath, _ := cmd.Flags().GetString("handoff")
			branch, _ := cmd.Flags().GetString("branch")
			base, _ := cmd.Flags().GetString("base")
			title, _ := cmd.Flags().GetString("title")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			out := cmd.OutOrStdout()

			body := "Opened by `pikopod pr open`."
			if handoffPath != "" {
				raw, err := os.ReadFile(handoffPath)
				if err != nil {
					return errfmt.Newf("cannot read the handoff", "check --handoff", "", "%v", err)
				}
				_, markdown, err := pr.RenderHandoff(raw)
				if err != nil {
					return err
				}
				body = markdown + "\n\n---\n_Opened by `pikopod pr open`; the tables above are the evidence for the committed change._"
			}
			if branch == "" {
				branch = "pikopod/update-" + time.Now().UTC().Format("20060102-150405")
			}
			if base == "" {
				base = defaultBaseBranch()
			}
			// Same argv discipline as gitShow: a name that parses as a git option must
			// never reach the git argv, or --branch becomes flag injection.
			if strings.HasPrefix(branch, "-") || strings.HasPrefix(base, "-") {
				return errfmt.New("invalid branch name", "branch/base names may not start with '-'", "pick a name like pikopod/spec-update", "")
			}
			if title == "" {
				title = "pikopod: traffic-evidenced spec update"
			}

			steps := [][]string{
				{"git", "checkout", "-b", branch},
				append([]string{"git", "add", "--"}, files...),
				{"git", "commit", "-m", title},
				{"git", "push", "-u", "origin", branch},
			}
			if dryRun {
				fmt.Fprintf(out, "dry run — would execute:\n")
				for _, s := range steps {
					fmt.Fprintf(out, "  %s\n", strings.Join(s, " "))
				}
				fmt.Fprintf(out, "then open a PR %s → %s titled %q with the evidence body (%d bytes)\n", branch, base, title, len(body))
				return nil
			}
			for _, s := range steps {
				if err := runCmd(s); err != nil {
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
	c.Flags().StringArray("commit", nil, "file to commit on the PR branch (repeatable; already modified in the working tree)")
	c.Flags().String("handoff", "", "JSON handoff whose rendering becomes the PR body (evidence)")
	c.Flags().String("branch", "", "branch name (default pikopod/update-<timestamp>)")
	c.Flags().String("base", "", "base branch (default: origin's HEAD, else main)")
	c.Flags().String("title", "", "PR title")
	c.Flags().Bool("dry-run", false, "print the git/PR plan without executing")
	return c
}

func addForgeFlags(c *cobra.Command) {
	c.Flags().String("platform", "", "github | gitlab (default: detected from CI env)")
	c.Flags().String("repo", "", "owner/name (GitHub) or project path/id (GitLab); default from CI env")
	c.Flags().Int("number", 0, "PR/MR number; default from CI env")
	c.Flags().String("sha", "", "head commit sha for the staleness marker; default from CI env")
	c.Flags().String("api", "", "API base URL override (GHE / self-hosted GitLab)")
}

// forgeFromFlags builds the platform client from flags + CI environment. Tokens
// come from env or `gh auth token`, never argv; it refuses when neither exists.
func forgeFromFlags(cmd *cobra.Command) (pr.Forge, string, error) {
	return forgeFromFlagsOpts(cmd, true)
}

// forgeForOpen is forgeFromFlags without the PR/MR-number requirement: opening a
// PR creates the number; only commenting on an existing one needs coordinates.
func forgeForOpen(cmd *cobra.Command) (pr.Forge, string, error) {
	return forgeFromFlagsOpts(cmd, false)
}

func forgeFromFlagsOpts(cmd *cobra.Command, needNumber bool) (pr.Forge, string, error) {
	platform, _ := cmd.Flags().GetString("platform")
	repo, _ := cmd.Flags().GetString("repo")
	number, _ := cmd.Flags().GetInt("number")
	sha, _ := cmd.Flags().GetString("sha")
	api, _ := cmd.Flags().GetString("api")

	if platform == "" {
		switch {
		case os.Getenv("GITHUB_ACTIONS") == "true":
			platform = "github"
		case os.Getenv("GITLAB_CI") == "true":
			platform = "gitlab"
		default:
			return nil, "", errfmt.New("cannot detect the platform", "no CI environment found", "pass --platform github|gitlab (plus --repo and --number)", "docs/config-reference.md#pr")
		}
	}

	switch platform {
	case "github":
		if repo == "" {
			repo = os.Getenv("GITHUB_REPOSITORY")
		}
		if sha == "" {
			sha = os.Getenv("GITHUB_SHA")
		}
		if number == 0 {
			// refs/pull/123/merge
			if ref := os.Getenv("GITHUB_REF"); strings.HasPrefix(ref, "refs/pull/") {
				number, _ = strconv.Atoi(strings.Split(strings.TrimPrefix(ref, "refs/pull/"), "/")[0])
			}
		}
		token := firstEnv("GITHUB_TOKEN", "GH_TOKEN")
		if token == "" {
			token = ghAuthToken()
		}
		if token == "" {
			return nil, "", errfmt.New("no GitHub credential", "neither GITHUB_TOKEN/GH_TOKEN is set nor is the gh CLI logged in", "export GITHUB_TOKEN (repo scope), or `gh auth login`", "docs/config-reference.md#pr")
		}
		if repo == "" || (needNumber && number == 0) {
			return nil, "", errfmt.New("missing PR coordinates", "repo (and, for comments, the PR number) is required outside GitHub Actions", "pass --repo owner/name (--number <pr> for comments)", "docs/config-reference.md#pr")
		}
		return &pr.GitHub{BaseURL: api, Repo: repo, Number: number, Token: token}, sha, nil

	case "gitlab":
		if repo == "" {
			repo = firstEnv("CI_PROJECT_ID", "CI_PROJECT_PATH")
		}
		if sha == "" {
			sha = os.Getenv("CI_COMMIT_SHA")
		}
		if number == 0 {
			number, _ = strconv.Atoi(os.Getenv("CI_MERGE_REQUEST_IID"))
		}
		if api == "" {
			api = os.Getenv("CI_API_V4_URL")
		}
		token := os.Getenv("GITLAB_TOKEN")
		if token == "" {
			return nil, "", errfmt.New("no GitLab credential", "GITLAB_TOKEN is not set (job tokens usually cannot post MR notes)", "export GITLAB_TOKEN with api scope", "docs/config-reference.md#pr")
		}
		if repo == "" || (needNumber && number == 0) {
			return nil, "", errfmt.New("missing MR coordinates", "project (and, for comments, the MR iid) is required outside GitLab CI", "pass --repo <project-path-or-id> (--number <iid> for comments)", "docs/config-reference.md#pr")
		}
		return &pr.GitLab{BaseURL: api, Project: repo, Number: number, Token: token}, sha, nil
	}
	return nil, "", errfmt.New("unknown platform "+platform, "supported: github, gitlab", "", "")
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// ghAuthToken reads the gh CLI's stored token (stdout capture, never argv).
func ghAuthToken() string {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func defaultBaseBranch() string {
	out, err := exec.Command("git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		if ref := strings.TrimSpace(string(out)); ref != "" {
			return strings.TrimPrefix(ref, "origin/")
		}
	}
	return "main"
}

func runCmd(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errfmt.New(strings.Join(argv[:2], " ")+" failed", strings.TrimSpace(stderr.String()), "fix the git state and retry", "")
	}
	return nil
}
