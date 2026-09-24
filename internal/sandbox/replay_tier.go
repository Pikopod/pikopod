package sandbox

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/replay"
)

const ReplayTierHeader = "x-pikopod-replay-tier"

const ReplayMissedOnHeader = "x-pikopod-replay-missed-on"

const ReplayClosestHeader = "x-pikopod-replay-closest"

const ReplaySequenceHeader = "x-pikopod-replay-sequence"

func (e *Engine) serveRecording(req *ingressRequest, innerPath string) *RawResponse {
	if e.recordings == nil {
		return nil
	}

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
