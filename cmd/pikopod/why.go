package main

import (
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/spf13/cobra"
)

func newWhyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "why <sandbox> <METHOD> <path>",
		Short: "Replay a request against a fork with decision tracing — see exactly why the sandbox answers what it answers",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			name, method, path := args[0], strings.ToUpper(args[1]), args[2]
			if !strings.HasPrefix(path, "/") {
				return errfmt.New("path must start with /", fmt.Sprintf("%q is not a provider-relative path", path), "e.g. pikopod why "+name+" GET /charges/ch_123", "")
			}
			body, _ := cmd.Flags().GetString("body")
			withAuth, _ := cmd.Flags().GetBool("auth")

			entry, def, err := loadSandboxDef(cfg, name)
			if err != nil {
				return err
			}

			eng, done, err := scenarioEngine(cfg, entry, def, false, 0)
			if err != nil {
				return err
			}
			defer done()

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "replaying %s %s against a fork of %s (seed %s) — no real state is touched\n\n", method, path, name, entry.Seed)
			eng.SetTrace(func(stage, message string) {
				fmt.Fprintf(out, "  %-10s %s\n", stage, message)
			})

			var reqBody *strings.Reader
			if body != "" {
				reqBody = strings.NewReader(body)
			} else {
				reqBody = strings.NewReader("")
			}
			req := httptest.NewRequest(method, path, reqBody)
			if body != "" {
				req.Header.Set("content-type", "application/json")
			}
			if withAuth {
				if hname, hvalue, ok := eng.AuthHeader(); ok {
					req.Header.Set(hname, hvalue)
					fmt.Fprintf(out, "  %-10s sending the issued credential in %s\n", "auth", hname)
				}
			}
			rec := httptest.NewRecorder()
			eng.ServeHTTP(rec, req)

			fmt.Fprintf(out, "\n→ %d\n", rec.Code)
			keys := make([]string, 0, len(rec.Header()))
			for k := range rec.Header() {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(out, "  %s: %s\n", k, rec.Header().Get(k))
			}
			bodyOut := rec.Body.String()
			if len(bodyOut) > 2000 {
				bodyOut = bodyOut[:2000] + "\n…truncated"
			}
			if bodyOut != "" {
				fmt.Fprintf(out, "\n%s\n", bodyOut)
			}
			return nil
		},
	}
	c.Flags().String("body", "", "JSON request body")
	c.Flags().Bool("auth", true, "send the sandbox's issued credential (default: yes — pass --auth=false to see the auth refusal path)")
	return c
}
