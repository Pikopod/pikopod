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
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/spf13/cobra"
)

func chaosClientReq(cfg *config.Config, method, sandboxName string, query url.Values, body any) (*http.Response, error) {
	base := fmt.Sprintf("%s://%s/_pikopod/sandboxes/%s/faults", cfg.Scheme(), net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.SandboxPort)), url.PathEscape(sandboxName))
	if len(query) > 0 {
		base += "?" + query.Encode()
	}
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
	client := cfg.LocalClient(5 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errfmt.Newf("the sandbox server is not running", "start it with `pikopod up`, then retry", "docs/config-reference.md#ports", "nothing answered on %s (%v)", base, err)
	}
	return resp, nil
}

func newChaosCmd() *cobra.Command {
	c := &cobra.Command{Use: "chaos <sandbox>", Short: "Arm failure conditions (errors, latency, rate limits) on a RUNNING sandbox", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			sandboxName := args[0]

			if list, _ := cmd.Flags().GetBool("list"); list {
				resp, err := chaosClientReq(cfg, http.MethodGet, sandboxName, nil, nil)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				return chaosRelay(resp, out, "standing faults")
			}

			method, _ := cmd.Flags().GetString("method")
			path, _ := cmd.Flags().GetString("path")

			if clear, _ := cmd.Flags().GetBool("clear"); clear {
				q := url.Values{}
				if method != "" {
					q.Set("method", method)
				}
				if path != "" {
					q.Set("path", path)
				}
				resp, err := chaosClientReq(cfg, http.MethodDelete, sandboxName, q, nil)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				return chaosRelay(resp, out, "cleared")
			}

			kind, _ := cmd.Flags().GetString("kind")
			status, _ := cmd.Flags().GetInt("status")
			delayMs, _ := cmd.Flags().GetInt64("delay-ms")
			probability, _ := cmd.Flags().GetFloat64("probability")
			if probability <= 0 {
				return errfmt.New("a fault with probability 0 would never trip",
					"arming it would be indistinguishable from not arming it",
					"pass --probability between 0 and 1 (exclusive of 0), or use --clear to remove a standing fault",
					"docs/config-reference.md#scenarios")
			}
			wallclock, _ := cmd.Flags().GetBool("wallclock")

			if !sandbox.ValidFaultKind(kind) {
				return errfmt.New("unknown fault kind", fmt.Sprintf("%q is not a fault kind", kind), "use one of: "+strings.Join(sandbox.FaultKinds(), ", "), "scenarios/README.md")
			}
			event, _ := cmd.Flags().GetString("event")
			rule := sandbox.FaultRule{Method: method, Path: path, Probability: probability, Wallclock: wallclock, Event: event}
			rule.Kind, rule.Status = sandbox.ResolveFaultKind(kind, status)
			switch kind {
			case "latency", "hang", "slow_body", sandbox.FaultDelayWebhook:
				rule.DelayMs = delayMs
				if rule.DelayMs == 0 && kind == "latency" {
					rule.DelayMs = 30000
				}
			}

			if !sandbox.IsWebhookFaultKind(kind) && (rule.Method == "" || rule.Path == "") {
				return errfmt.New("chaos needs a target operation", "pass --method and --path (the endpoint's path template)", "e.g. pikopod chaos "+sandboxName+" --kind error --status 503 --method POST --path /transaction", "")
			}
			resp, err := chaosClientReq(cfg, http.MethodPost, sandboxName, nil, rule)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if err := chaosRelay(resp, out, "armed"); err != nil {
				return err
			}
			if sandbox.IsWebhookFaultKind(kind) {
				fmt.Fprintf(out, "clear it with: pikopod chaos %s --clear   (webhook faults clear with every standing fault)\n", sandboxName)
			} else {
				fmt.Fprintf(out, "clear it with: pikopod chaos %s --clear --method %s --path %s\n", sandboxName, rule.Method, rule.Path)
			}
			return nil
		}}
	c.Flags().Bool("list", false, "show standing faults")
	c.Flags().Bool("clear", false, "clear matching faults (all when no --method/--path)")
	c.Flags().String("kind", "error", "fault kind: "+strings.Join(sandbox.FaultKinds(), ", "))
	c.Flags().String("event", "", "webhook event a webhook-kind fault matches (default: any)")
	c.Flags().Bool("wallclock", false, "REAL wire delay (default is virtualized/instant; hang and slow_body only act with this or `up --wallclock-faults`)")
	c.Flags().String("method", "", "HTTP method of the target operation")
	c.Flags().String("path", "", "path template of the target operation (as in the spec)")
	c.Flags().Int("status", 0, "error status to answer with (default 500; rate_limit forces 429)")
	c.Flags().Int64("delay-ms", 0, "latency to report (virtualized; default 30000)")
	c.Flags().Float64("probability", 1, "chance each request trips the fault (0..1)")
	return c
}

func chaosRelay(resp *http.Response, out io.Writer, verb string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return errfmt.New("the sandbox server refused", fmt.Sprintf("%d: %s", resp.StatusCode, bytes.TrimSpace(raw)), "check the sandbox name (`pikopod sandbox list`) and flags", "")
	}
	fmt.Fprintf(out, "%s: %s\n", verb, bytes.TrimSpace(raw))
	return nil
}
