package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/proxy"
)

// FindRecording returns the most recent recording that produced an incident.
// Unlike FindEvent's template, a recording carries the CONCRETE path, so this
// path never needs concretize() and has none of its single-parameter limit.
func FindRecording(dataDir string, ev *alert.DriftEvent) (*proxy.Record, error) {
	path := filepath.Join(dataDir, "recordings", ev.Upstream+".ndjson")
	f, err := os.Open(path)
	if err != nil {
		return nil, errfmt.Newf("no recordings for "+ev.Upstream,
			"run `pikopod up` and let traffic flow, then retry",
			"docs/config-reference.md#data_dir",
			"the incident was alerted but its traffic is not on disk: %v", err)
	}
	defer f.Close()

	want, err := strconv.Atoi(ev.After)
	if err != nil {
		return nil, errUnreproducible(ev, "the event carries no concrete status to reproduce")
	}

	var best *proxy.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec proxy.Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // recordings are drop-oldest logs; a torn tail line is fine
		}
		if rec.Method != ev.Method || rec.Status != want {
			continue
		}
		if !pathFitsTemplate(rec.Path, ev.Endpoint) {
			continue
		}
		if best == nil || rec.TS.After(best.TS) {
			r := rec
			best = &r
		}
	}
	if best == nil {
		return nil, errfmt.New("no recording for this incident",
			fmt.Sprintf("%s was alerted on %s but no matching recording is on disk — retention may have aged it out",
				ev.Fingerprint, ev.FirstSeen.Format("2006-01-02")),
			"raise retention.max_age_hours, or reproduce from a newer occurrence",
			"docs/config-reference.md#retention")
	}
	return best, nil
}

// pathFitsTemplate reports whether a concrete path could have produced a
// template: same segment count, and every literal segment matching.
func pathFitsTemplate(path, template string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	ps := strings.Split(strings.Trim(path, "/"), "/")
	ts := strings.Split(strings.Trim(template, "/"), "/")
	if len(ps) != len(ts) {
		return false
	}
	for i, seg := range ts {
		if strings.Contains(seg, "{") {
			continue // a parameter matches any single segment
		}
		if seg != ps[i] {
			return false
		}
	}
	return true
}

// faultForKind maps an incident to the fault that reproduces it. An unreachable
// upstream is a dead socket, not a status — reproducing it as a 502 would test
// the wrong branch of the caller's error handling.
func faultForKind(kind drift.Kind, status int) map[string]any {
	switch kind {
	case drift.UpstreamUnreachable:
		return map[string]any{"kind": "connection_reset"}
	case drift.RateLimited:
		return map[string]any{"kind": "rate_limit"}
	default:
		return map[string]any{"kind": "error", "status": status}
	}
}

// BuildFromRecord turns a recorded failure into a scenario that arms the same
// failure in the sandbox and replays the same request at it.
func BuildFromRecord(ev *alert.DriftEvent, rec *proxy.Record, contractVersion int) (string, map[string]any, error) {
	if !ev.Kind.IsIncident() {
		return "", nil, errfmt.New("not an incident",
			string(ev.Kind)+" is a shape change, not a failed exchange",
			"use `pikopod scenario from-drift "+ev.Fingerprint+"` to pin the baseline contract instead",
			"docs/exit-codes.md")
	}

	reqPath := rec.Path
	if i := strings.IndexByte(reqPath, '?'); i >= 0 {
		reqPath = reqPath[:i]
	}

	fault := faultForKind(ev.Kind, rec.Status)
	fault["method"] = rec.Method
	fault["path"] = ev.Endpoint // the fault matches on the spec template
	fault["times"] = 1          // fail once, then recover: the shape retry bugs need

	steps := []any{
		map[string]any{
			"key": "context", "type": "NOTE",
			"config": map[string]any{"text": provenanceNote(ev, rec)},
		},
		map[string]any{
			"key": "arm-failure", "type": "INJECT_FAULT", "config": fault,
		},
	}

	request := map[string]any{
		"key": "replay", "type": "REQUEST",
		"config": map[string]any{"method": rec.Method, "path": reqPath},
		"assertions": []any{
			map[string]any{"target": "response.status", "op": "equals", "expected": rec.Status},
		},
	}
	// An unreachable upstream has no status to assert; the transport dies.
	if ev.Kind == drift.UpstreamUnreachable {
		delete(request, "assertions")
	}
	if body, ok := replayBody(rec); ok {
		request["config"].(map[string]any)["body"] = body
	}
	steps = append(steps, request)

	name := "incident-" + strings.TrimPrefix(ev.Fingerprint, "fp_")
	pack := map[string]any{
		"name":     name,
		"provider": ev.Upstream,
		"description": fmt.Sprintf(
			"Reproduces incident %s: %s on %s %s. Arms the same failure in the sandbox and replays the recorded request, so the break happens locally.",
			ev.Fingerprint, ev.Kind, rec.Method, ev.Endpoint),
		"definition": map[string]any{"steps": steps},
	}
	if contractVersion > 0 {
		pack["contractVersion"] = contractVersion
	}
	return name, pack, nil
}

// provenanceNote states where the request came from and what that costs. The
// body is post-sanitizer, so fields the classifier could not place are GONE —
// which matters far more for a 4xx we caused than for a 5xx the upstream threw.
func provenanceNote(ev *alert.DriftEvent, rec *proxy.Record) string {
	base := fmt.Sprintf(
		"incident %s: %s answered %d on %s %s, first seen %s. "+
			"This request was RECONSTRUCTED from a redacted recording — identifiers are "+
			"format-preserving tokens and any field the sanitizer could not classify was dropped "+
			"before it reached disk, so the body is not byte-identical to the one that failed.",
		ev.Fingerprint, ev.Upstream, rec.Status, rec.Method, ev.Endpoint,
		ev.FirstSeen.Format("2006-01-02T15:04:05Z07:00"))
	if ev.Kind == drift.ClientError {
		return base + " THIS MATTERS HERE: a 4xx is usually caused by the request body, and " +
			"the body below is the redacted one. If this scenario does not reproduce the " +
			"rejection, compare it against what your code actually sends."
	}
	return base + " The fault is armed on method and path, so the failure reproduces regardless."
}

// replayBody returns the recorded request body when it is JSON we can replay.
// A binary or dropped body is not fabricated — the scenario goes without.
func replayBody(rec *proxy.Record) (any, bool) {
	if rec.ReqKind != "json" || rec.ReqBody == nil {
		return nil, false
	}
	if m, ok := rec.ReqBody.(map[string]any); ok {
		return m, true
	}
	return nil, false
}

func errUnreproducible(ev *alert.DriftEvent, why string) error {
	return errfmt.New("cannot reproduce this incident", why,
		"reproduce from a newer occurrence, or write the scenario by hand from schema/scenario-pack.schema.json",
		"scenarios/README.md")
}
