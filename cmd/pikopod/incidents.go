package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/bridge"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/spf13/cobra"
)

// defaultIncidentLimit bounds output. A silently truncated list is the same bug
// class as a silently empty one, so truncation is always reported.
const defaultIncidentLimit = 50

func newIncidentsCmd() *cobra.Command {
	c := &cobra.Command{Use: "incidents",
		Short: "List recorded incidents and drift events (exit 0 always; this is a reader)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			since, _ := cmd.Flags().GetDuration("since")
			kind, _ := cmd.Flags().GetString("kind")
			upstream, _ := cmd.Flags().GetString("upstream")
			format, _ := cmd.Flags().GetString("format")
			limit, _ := cmd.Flags().GetInt("limit")
			only, _ := cmd.Flags().GetString("only")
			if limit <= 0 {
				limit = defaultIncidentLimit
			}
			if only != "" && only != "incidents" && only != "drift" {
				return errfmt.New("unknown class", only+" is not a class of event",
					"use --only incidents or --only drift", "docs/exit-codes.md")
			}
			if format != "" && format != "text" && format != "json" {
				return errfmt.New("unknown format", format+" is not a supported format",
					"use --format text (default) or --format json", "docs/exit-codes.md")
			}

			evs, err := loadEvents(cfg.DataDir)
			if err != nil {
				return err
			}
			cutoff := time.Time{}
			if since > 0 {
				cutoff = time.Now().Add(-since)
			}
			var kept []alert.DriftEvent
			for _, ev := range evs {
				if only == "incidents" && !ev.Kind.IsIncident() {
					continue
				}
				if only == "drift" && ev.Kind.IsIncident() {
					continue
				}
				if kind != "" && string(ev.Kind) != kind {
					continue
				}
				if upstream != "" && ev.Upstream != upstream {
					continue
				}
				if !cutoff.IsZero() && ev.LastSeen.Before(cutoff) {
					continue
				}
				kept = append(kept, ev)
			}
			sort.Slice(kept, func(i, j int) bool { return kept[i].LastSeen.After(kept[j].LastSeen) })

			total := len(kept)
			truncated := false
			if total > limit {
				kept, truncated = kept[:limit], true
			}
			return renderIncidents(cmd.OutOrStdout(), kept, total, truncated, format, cfg.RetentionTTL())
		}}
	c.AddCommand(newIncidentsExportCmd())
	c.Flags().Duration("since", 0, "only events last seen within this window, e.g. 24h")
	c.Flags().String("kind", "", "filter by kind, e.g. upstream_error")
	c.Flags().String("upstream", "", "filter by upstream")
	c.Flags().String("format", "text", "output format: text | json")
	c.Flags().Int("limit", defaultIncidentLimit, "maximum events to show")
	c.Flags().String("only", "", "narrow to one class: incidents (failed exchanges) | drift (shape changes)")
	return c
}

// incidentReport is the JSON projection. total_matching and truncated are
// mandatory: a silently shortened list reads as a clean bill of health.
type incidentReport struct {
	SchemaVersion string             `json:"schema_version"`
	TotalMatching int                `json:"total_matching"`
	Truncated     bool               `json:"truncated"`
	Events        []alert.DriftEvent `json:"events"`
}

func newIncidentsExportCmd() *cobra.Command {
	c := &cobra.Command{Use: "export [fingerprint]",
		Short: "Write a self-contained incident bundle (event + redacted recording) for reproduction on another machine",
		Long: `Write an incident bundle to stdout: the event, the already-redacted recording
and the contract version, everything scenario reproduce and fix need. It carries
no salt, no token and no configuration, and it keeps working after retention has
aged the incident out of this host.

Pass a fingerprint for one bundle, or --since for a JSON array of every incident
in the window.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			since, _ := cmd.Flags().GetDuration("since")
			if len(args) == 0 && since <= 0 {
				return errfmt.New("nothing to export", "pass a fingerprint or --since <window>", "e.g. pikopod incidents export fp_14835fa32dfb, or --since 24h", "docs/config-reference.md#retention")
			}
			host, _ := os.Hostname()
			now := time.Now()
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if len(args) == 1 {
				ev, err := bridge.FindEvent(cfg.DataDir, args[0])
				if err != nil {
					return err
				}
				b, err := bridge.Export(cfg.DataDir, ev, cfg.RetentionTTL(), host, now)
				if err != nil {
					return err
				}
				return enc.Encode(b)
			}
			evs, err := loadEvents(cfg.DataDir)
			if err != nil {
				return err
			}
			cutoff := now.Add(-since)
			bundles := []*bridge.Bundle{}
			for i := range evs {
				ev := evs[i]
				if !ev.Kind.IsIncident() || ev.LastSeen.Before(cutoff) {
					continue
				}
				b, err := bridge.Export(cfg.DataDir, &ev, cfg.RetentionTTL(), host, now)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "skipping %s: %v\n", ev.Fingerprint, err)
					continue
				}
				bundles = append(bundles, b)
			}
			return enc.Encode(bundles)
		}}
	c.Flags().Duration("since", 0, "export every incident last seen within this window as a JSON array, e.g. 24h")
	return c
}

func renderIncidents(w io.Writer, evs []alert.DriftEvent, total int, truncated bool, format string, retention time.Duration) error {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		out := incidentReport{SchemaVersion: alert.SchemaVersion, TotalMatching: total, Truncated: truncated, Events: evs}
		if out.Events == nil {
			out.Events = []alert.DriftEvent{}
		}
		return enc.Encode(out)
	}
	if total == 0 {
		fmt.Fprintln(w, "no events recorded yet — run `pikopod up` and send traffic through the agent")
		return nil
	}
	for _, ev := range evs {
		marker := "drift"
		if ev.Kind.IsIncident() {
			marker = "incident"
		}
		fmt.Fprintf(w, "[%s] %-8s %-22s %s %s (%s) · %d occurrence(s) · last %s\n  %s\n",
			ev.Level, marker, ev.Kind, ev.Method, ev.Endpoint, ev.Upstream,
			ev.Occurrences, ev.LastSeen.Format(time.RFC3339), ev.Fingerprint)
		if ev.Kind.IsIncident() {
			if until := bridge.ExpiresAt(&ev, retention); until != nil {
				fmt.Fprintf(w, "  reproducible until %s\n", until.UTC().Format(time.RFC3339))
			}
			fmt.Fprintf(w, "  reproduce: pikopod scenario reproduce %s\n  export: pikopod incidents export %s\n", ev.Fingerprint, ev.Fingerprint)
		}
	}
	if truncated {
		fmt.Fprintf(w, "\nshowing %d of %d matching events — raise --limit to see more\n", len(evs), total)
	}
	return nil
}

func loadEvents(dataDir string) ([]alert.DriftEvent, error) {
	path := filepath.Join(dataDir, "events.ndjson")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no events yet is not an error
		}
		return nil, errfmt.Newf("cannot read the event log", "check permissions on "+path,
			"docs/config-reference.md#data_dir", "%v", err)
	}
	defer f.Close()

	// Last write wins per fingerprint: occurrence counters only grow.
	latest := map[string]alert.DriftEvent{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev alert.DriftEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if _, seen := latest[ev.Fingerprint]; !seen {
			order = append(order, ev.Fingerprint)
		}
		latest[ev.Fingerprint] = ev
	}
	out := make([]alert.DriftEvent, 0, len(order))
	for _, fp := range order {
		out = append(out, latest[fp])
	}
	return out, nil
}
