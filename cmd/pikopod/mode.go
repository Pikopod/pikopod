package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/mode"
	"github.com/pikopod/pikopod/internal/report"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/spf13/cobra"
)

func parseBindFlags(cmd *cobra.Command) (map[string]string, error) {
	kvs, _ := cmd.Flags().GetStringArray("bind")
	out := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, errfmt.New("bad --bind", fmt.Sprintf("%q is not role=operation", kv), "pass bindings as --bind role=operationId", "")
		}
		out[k] = v
	}
	return out, nil
}

func modeReq(cfg *config.Config, method, sandboxName string, body any) (*http.Response, error) {
	return adminReq(cfg, method, sandboxName, "mode", body)
}

func adminReq(cfg *config.Config, method, sandboxName, subpath string, body any) (*http.Response, error) {
	base := fmt.Sprintf("%s://%s/_pikopod/v1/sandboxes/%s/%s", cfg.Scheme(),
		net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.SandboxPort)), url.PathEscape(sandboxName), subpath)
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base, reader)
	if err != nil {
		return nil, err
	}
	if token := cfg.Token(); token != "" {
		req.Header.Set("X-Pikopod-Token", token)
	}
	resp, err := cfg.LocalClient(10 * time.Second).Do(req)
	if err != nil {
		return nil, errfmt.Newf("the sandbox server is not running", "start it with `pikopod up`, then retry", "docs/config-reference.md#ports", "nothing answered on %s (%v)", base, err)
	}
	return resp, nil
}

func decodeMode(resp *http.Response) (*mode.Spec, error) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var body struct {
			Message string `json:"message"`
		}
		json.Unmarshal(raw, &body)
		if body.Message == "" {
			body.Message = string(bytes.TrimSpace(raw))
		}
		why, _, _ := strings.Cut(body.Message, " → ")
		return nil, errfmt.New("the sandbox refused this mode", why, "run `pikopod scenario list <sandbox>` to see what binds", "scenarios/README.md")
	}
	var body struct {
		Mode *mode.Spec `json:"mode"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errfmt.Newf("the sandbox answered strangely", "retry", "", "%v", err)
	}
	return body.Mode, nil
}

func newModeCmd() *cobra.Command {
	c := &cobra.Command{Use: "mode", Short: "Put a RUNNING sandbox into a scenario's failure state, and run your own tests against it",
		Long: `Put a RUNNING sandbox into a scenario's failure state.

` + "`pikopod scenario check`" + ` drives its own requests against a throwaway engine, so
your application is never in the loop. A mode is the other half: it arms the
scenario's standing conditions on the sandbox ` + "`pikopod up`" + ` is serving, and then
your own tests, your own app, or plain curl meet the failure.

A mode is set on the sandbox and stays until cleared. Fault windows, the request
journal and the recordings cursor are partitioned by the ` + "`X-Pikopod-Scope`" + `
request header: tests that send a scope each get their own ` + "`times`" + ` window and
their own journal, and ` + "`mode verify --scope <token>`" + ` judges one partition.
Requests without the header share the unscoped partition.

The control plane that serves this listens on the sandbox port and is
unauthenticated on loopback, like the rest of ` + "`/_pikopod/`" + `. It controls a fake,
never a provider. Binding a non-loopback address requires a token.`}

	set := &cobra.Command{Use: "set <sandbox> <scenario>", Short: "Enter a scenario's standing state (stays until cleared)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			binds, err := parseBindFlags(cmd)
			if err != nil {
				return err
			}
			resp, err := modeReq(cfg, http.MethodPost, args[0], map[string]any{"name": args[1], "bind": binds})
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			spec, err := decodeMode(resp)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprint(out, spec.Describe())
			fmt.Fprintf(out, "point your app at the sandbox and run your own tests; clear it with `pikopod mode clear %s`\n", args[0])
			return nil
		}}
	set.Flags().StringArray("bind", nil, "override a role binding as role=operationId (repeatable)")

	show := &cobra.Command{Use: "show <sandbox>", Short: "Show the standing state, if any", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, _ := cmd.Flags().GetString("format")
			if format != "text" && format != "json" {
				return errfmt.New("unknown --format", fmt.Sprintf("%q is not text or json", format), "pass --format text or --format json", "scenarios/README.md")
			}
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			resp, err := modeReq(cfg, http.MethodGet, args[0], nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			spec, err := decodeMode(resp)
			if err != nil {
				return err
			}
			if format == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(spec)
			}
			if spec == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "no mode set")
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), spec.Describe())
			return nil
		}}
	show.Flags().String("format", "text", "output format: text | json")

	clear := &cobra.Command{Use: "clear <sandbox>", Short: "Leave the standing state", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			resp, err := modeReq(cfg, http.MethodDelete, args[0], nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			return printCleared(resp, cmd.OutOrStdout())
		}}

	verify := &cobra.Command{Use: "verify <sandbox>", Short: "Check what your tests sent against the standing scenario's verification steps (exit 0 pass / 1 fail / 2 error)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			scope, _ := cmd.Flags().GetString("scope")
			formatFlag, _ := cmd.Flags().GetString("format")
			format, err := report.ParseFormat(formatFlag)
			if err != nil {
				return err
			}
			subpath := "mode/verify"
			if scope != "" {
				subpath += "?scope=" + url.QueryEscape(scope)
			}
			resp, err := adminReq(cfg, http.MethodPost, args[0], subpath, nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if resp.StatusCode >= 400 {
				var body struct {
					Message string `json:"message"`
				}
				json.Unmarshal(raw, &body)
				if body.Message == "" {
					body.Message = string(bytes.TrimSpace(raw))
				}
				if resp.StatusCode == http.StatusConflict {
					return errfmt.New("no mode set on "+args[0], "nothing is armed, so there is nothing to verify against", "enter one with `pikopod mode set "+args[0]+" <scenario>`, run your tests, then verify", "scenarios/README.md")
				}
				why, _, _ := strings.Cut(body.Message, " → ")
				return errfmt.New("the sandbox could not verify", why, "run `pikopod mode show "+args[0]+"` to see what is armed", "scenarios/README.md")
			}
			var body struct {
				Mode   string             `json:"mode"`
				Result scenario.RunResult `json:"result"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				return errfmt.Newf("the sandbox answered strangely", "retry", "", "%v", err)
			}
			out := cmd.OutOrStdout()
			notes := out
			if format != report.FormatText {
				notes = cmd.ErrOrStderr()
			}
			if scope != "" {
				fmt.Fprintf(notes, "scope %q: judging only the requests sent with X-Pikopod-Scope: %s\n", scope, scope)
			}
			doc := &report.Document{}
			doc.Add(body.Mode, "", &body.Result)
			switch format {
			case report.FormatText:
				writeRunResult(out, body.Mode, &body.Result)
			case report.FormatJSON:
				if err := doc.WriteJSON(out); err != nil {
					return err
				}
			case report.FormatJUnit:
				if err := doc.WriteJUnit(out); err != nil {
					return err
				}
			case report.FormatGitHubActions:
				doc.WriteGitHubActions(out)
			}
			switch body.Result.Status {
			case scenario.RunErrored:
				return errfmt.New("mode verify errored", "a verification step could not run (see the ! line above)", "fix the scenario or sandbox and retry", "docs/exit-codes.md")
			case scenario.RunFailed:
				fmt.Fprintln(notes, "\nyour client did not do what the scenario expects — exit 1")
				exit(1)
			}
			return nil
		}}

	verify.Flags().String("scope", "", "judge only the requests sent with this X-Pikopod-Scope header")
	verify.Flags().String("format", "text", "output format: text | json | junit | githubactions")
	c.AddCommand(set, show, clear, verify)
	return c
}
