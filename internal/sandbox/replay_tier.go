// The engine's FINAL resolution tier, after spec routes and traffic-admitted
// observed endpoints: traffic adds routes, it never hijacks declared ones.
package sandbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/replay"
)

// ReplayTierHeader names the recording tier that matched (exact/shape/sequence).
const ReplayTierHeader = "x-pikopod-replay-tier"

// ReplayMissedOnHeader explains a DEGRADED serve: a lower-fidelity serve always
// says which fields kept the higher tier from matching.
const ReplayMissedOnHeader = "x-pikopod-replay-missed-on"

// ReplayClosestHeader names the nearest RECORDED endpoint on a full
// recordings-tier miss — the tier the spec headers cannot see.
const ReplayClosestHeader = "x-pikopod-replay-closest"

// ReplaySequenceHeader shows exact-tier sequence progression ("2/3", then
// "3/3 (holding last)") when one request was recorded with different responses.
const ReplaySequenceHeader = "x-pikopod-replay-sequence"

// serveRecording answers from the linked upstream's recordings, or nil on miss.
// Sequence-tier consumption lives in the Set.
func (e *Engine) serveRecording(req *ingressRequest, innerPath string) *RawResponse {
	if e.recordings == nil {
		return nil
	}
	// The recordings matcher hashes PLAIN decoded JSON; normalize first, or the
	// exact/shape tiers can never match a request that carries a body.
	reqBody := req.bodyValue
	if reqBody != nil {
		reqBody = plainJSON(reqBody)
	}
	rec, diag := e.recordings.MatchValueDiag(req.method, innerPath, reqBody)
	if rec == nil {
		if diag.Closest != "" {
			e.tracef("recordings", "no recordings for %s %s; closest recorded endpoint: %s (%d recordings)",
				req.method, innerPath, diag.Closest, diag.ClosestN)
		} else {
			e.tracef("recordings", "no recordings for %s %s (nothing recorded nearby either)", req.method, innerPath)
		}
		return nil
	}
	headers := map[string]string{ReplayTierHeader: string(diag.Tier)}
	if diag.SeqLen > 1 {
		// The same request recorded N times replays as an ordered N-step behavior.
		seq := fmt.Sprintf("%d/%d", diag.SeqPos, diag.SeqLen)
		if diag.Held {
			seq += " (holding last)"
		}
		headers[ReplaySequenceHeader] = seq
		e.tracef("recordings", "exact-tier sequence step %s", seq)
	}
	if len(diag.MissedOn) > 0 {
		gap := strings.Join(diag.MissedOn, ",")
		switch diag.Tier {
		case replay.TierShape:
			headers[ReplayMissedOnHeader] = "exact missed on values: " + gap
			e.tracef("recordings", "shape-tier serve — exact missed on values: %s", gap)
		case replay.TierSequence:
			headers[ReplayMissedOnHeader] = "shape missed on fields: " + gap
			e.tracef("recordings", "sequence-tier serve — shape missed on fields: %s", gap)
		}
	}
	if e.effective != nil {
		headers[ContractVersionHeader] = strconv.Itoa(e.effective.Version)
	}
	var body []byte
	if rec.RespBody != nil {
		var err error
		if body, err = json.Marshal(rec.RespBody); err != nil {
			return nil
		}
		headers["content-type"] = jsonContentType
		headers["content-length"] = strconv.Itoa(len(body))
	}
	return &RawResponse{Status: rec.Status, Headers: headers, Body: body}
}
