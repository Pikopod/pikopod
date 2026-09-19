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
	base := fmt.Sprintf("%s://%s/_pikopod/sandboxes/%s/%s", cfg.Scheme(),
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
		return nil, errfmt.New("the sandbox refused this mode", body.Message, "run `pikopod scenario list <sandbox>` to see what binds", "scenarios/README.md")
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

` + "`pikopod scenario run`" + ` drives its own requests against a throwaway engine, so
your application is never in the loop. A mode is the other half: it arms the
scenario's standing conditions on the sandbox ` + "`pikopod up`" + ` is serving, and then
your own tests, your own app, or plain curl meet the failure.

A mode is GLOBAL to the sandbox and stays until cleared. Two test suites running
against one sandbox will see each other's faults, and a fault with a ` + "`times`" + `
window counts down across both. Run one suite at a time against a given sandbox,
or give each its own ` + "`pikopod up`" + ` on a different port.

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
			if spec == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "no mode set")
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), spec.Describe())
			return nil
		}}

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
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			fmt.Fprintf(cmd.OutOrStdout(), "cleared: %s\n", bytes.TrimSpace(raw))
			return nil
		}}

	c.AddCommand(set, show, clear)
	return c
}
