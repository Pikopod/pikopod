// `pikopod ack <fingerprint>` — silences a drift alert until its diff changes
// (a changed diff is a new fingerprint by construction).
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/store"
	"github.com/spf13/cobra"
)

func newAcceptCmd() *cobra.Command {
	return &cobra.Command{Use: "accept <fingerprint>", Short: "Accept a drift: the current provider behavior becomes the new baseline (refreeze + ack)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			fp := args[0]
			addr := fmt.Sprintf("%s://%s/accept?fp=%s", cfg.Scheme(), net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.AgentPort)), url.QueryEscape(fp))
			req, _ := http.NewRequest(http.MethodPost, addr, nil)
			if token := cfg.Token(); token != "" {
				req.Header.Set("X-Pikopod-Token", token)
			}
			client := cfg.LocalClient(2 * time.Second)
			resp, err := client.Do(req)
			if err != nil {
				return errfmt.New("the agent is not running", "accept refreezes the LIVE baseline, so it needs the running agent", "start `pikopod up`, then retry (use `pikopod ack` to merely silence while offline)", "docs/config-reference.md#alerts")
			}
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				fmt.Fprintf(cmd.OutOrStdout(), "accepted %s — the drifted behavior is the baseline now; findings for it stop entirely\n", fp)
				return nil
			case http.StatusNotFound:
				return errfmt.New("unknown fingerprint", fp+" is not in the alerter's state", "copy the fp_… from the alert message", "docs/config-reference.md#alerts")
			default:
				return errfmt.New("agent refused the accept", fmt.Sprintf("it answered %d", resp.StatusCode), "check the token configuration", "docs/config-reference.md#listen")
			}
		}}
}

func newAckCmd() *cobra.Command {
	return &cobra.Command{Use: "ack <fingerprint>", Short: "Acknowledge a drift alert (silences it until the diff changes)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			fp := args[0]
			out := cmd.OutOrStdout()

			// Live path: the running agent owns the state.
			addr := fmt.Sprintf("%s://%s/ack?fp=%s", cfg.Scheme(), net.JoinHostPort(cfg.Listen, fmt.Sprint(cfg.AgentPort)), url.QueryEscape(fp))
			req, _ := http.NewRequest(http.MethodPost, addr, nil)
			if token := cfg.Token(); token != "" {
				req.Header.Set("X-Pikopod-Token", token)
			}
			client := cfg.LocalClient(2 * time.Second)
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					fmt.Fprintf(out, "acked %s — silenced until its diff changes (a changed diff is a new fingerprint)\n", fp)
					return nil
				case http.StatusNotFound:
					return errfmt.New("unknown fingerprint", fp+" is not in the alerter's state", "copy the fp_… from the alert message, or see `pikopod status`", "docs/config-reference.md#alerts")
				default:
					return errfmt.New("agent refused the ack", fmt.Sprintf("it answered %d", resp.StatusCode), "check the token configuration", "docs/config-reference.md#listen")
				}
			}

			// Offline path: mutate the persisted state; the agent loads it on next start.
			statePath := filepath.Join(cfg.DataDir, "alerts", "state.json")
			raw, rerr := os.ReadFile(statePath)
			if rerr != nil {
				return errfmt.New("nothing to ack", "the agent is not running and no alert state exists under "+cfg.DataDir, "start `pikopod up`, let it alert, then ack the printed fingerprint", "docs/config-reference.md#alerts")
			}
			var states map[string]map[string]any
			if err := json.Unmarshal(raw, &states); err != nil {
				return errfmt.Newf("alert state is unreadable", "fix or remove "+statePath, "docs/config-reference.md#alerts", "%v", err)
			}
			st, ok := states[fp]
			if !ok {
				return errfmt.New("unknown fingerprint", fp+" is not in the persisted alert state", "copy the fp_… from the alert message", "docs/config-reference.md#alerts")
			}
			st["acked"] = true
			updated, err := json.MarshalIndent(states, "", " ")
			if err != nil {
				return err
			}
			if err := store.WriteFileAtomic(statePath, updated); err != nil {
				return errfmt.Newf("cannot write alert state", "check permissions on "+statePath, "", "%v", err)
			}
			fmt.Fprintf(out, "acked %s (agent offline — applied to persisted state; takes effect on next `pikopod up`)\n", fp)
			return nil
		}}
}
