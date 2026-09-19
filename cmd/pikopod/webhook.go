package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/spf13/cobra"
)

type emitRequest struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

func (s *sandboxServer) serveWebhookEmit(w http.ResponseWriter, r *http.Request, engine *sandbox.Engine) {
	if r.Method != http.MethodPost {
		writeSandboxJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	var req emitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Event == "" {
		writeSandboxJSONError(w, http.StatusBadRequest, "body must be {\"event\": \"<declared event>\", \"data\": {...}}")
		return
	}
	if err := engine.EmitWebhook(req.Event, req.Data); err != nil {
		writeSandboxJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"emitted": req.Event})
}

// readEmitData accepts inline JSON or @path, and insists on an object.
func readEmitData(arg string) (json.RawMessage, error) {
	if arg == "" {
		return nil, nil
	}
	raw := []byte(arg)
	if strings.HasPrefix(arg, "@") {
		b, err := os.ReadFile(strings.TrimPrefix(arg, "@"))
		if err != nil {
			return nil, errfmt.Newf("cannot read --data file", "check the path after @", "scenarios/README.md", "%v", err)
		}
		raw = b
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errfmt.Newf("--data must be a JSON object", "pass inline JSON or @path to a file holding one", "scenarios/README.md", "%v", err)
	}
	return json.RawMessage(bytes.TrimSpace(raw)), nil
}

func newWebhookCmd() *cobra.Command {
	c := &cobra.Command{Use: "webhook", Short: "Fire a declared webhook event on a RUNNING sandbox"}

	emit := &cobra.Command{Use: "emit <sandbox> <event>", Short: "Emit a declared event on demand (for events no API call causes)", Args: cobra.ExactArgs(2),
		Long: `Emit a declared webhook event on demand.

Some events follow no API call: money landing in an account, a chargeback, a
KYC decision. Declare them in the spec with x-pikopod-emit-only: true and fire
them here. Only DECLARED events can be emitted; pikopod never invents one, since
at your handler it would be indistinguishable from a real delivery.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			dataArg, _ := cmd.Flags().GetString("data")
			data, err := readEmitData(dataArg)
			if err != nil {
				return err
			}
			resp, err := adminReq(cfg, http.MethodPost, args[0], "webhooks/emit", emitRequest{Event: args[1], Data: data})
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode >= 400 {
				var body struct {
					Message string `json:"message"`
				}
				json.Unmarshal(raw, &body)
				return errfmt.New("the sandbox refused to emit "+args[1], body.Message, "declare the event in the spec; see `pikopod sandbox list` for what is declared", "scenarios/README.md")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "emitted %s on %s (signed delivery queued)\n", args[1], args[0])
			return nil
		}}
	emit.Flags().String("data", "", "JSON object to overlay onto the documented payload, inline or @file")

	c.AddCommand(emit)
	return c
}
