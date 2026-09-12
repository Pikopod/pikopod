// Package replay matches recorded provider traffic through three tiers (exact,
// shape, sequence) and gates CI by diffing recordings against frozen baselines.
package replay

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/pathtmpl"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/volatile"
)

// defaultVolatile: curated field names stripped before tier-1 hashing
// (signatures, trace context, idempotency keys); exact-match, never substring.
var defaultVolatile = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range volatile.RequestFieldNames() {
		m[n] = true
	}
	return m
}()

type Recording struct {
	Record proxy.Record
	// served marks tier-3 sequence consumption.
	served bool
}

type MatchTier string

const (
	TierExact    MatchTier = "exact"
	TierShape    MatchTier = "shape"
	TierSequence MatchTier = "sequence"
	TierMiss     MatchTier = "miss"
)

type ReportLine struct {
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Tier   MatchTier `json:"tier"`
}

// Set is an indexed, replayable set of recordings for one upstream.
type Set struct {
	mu        sync.Mutex
	upstream  string
	volatile  map[string]bool
	exact     map[string][]*Recording
	shape     map[string][]*Recording
	sequence  map[string][]*Recording
	Report    []ReportLine
	Unmatched int
}

// Load reads the current recording generation for an upstream.
func Load(dataDir, upstream string, extraVolatile []string) (*Set, error) {
	path := filepath.Join(dataDir, "recordings", upstream+".ndjson")
	f, err := os.Open(path)
	if err != nil {
		return nil, errfmt.Newf("no recordings for "+upstream, "run `pikopod up` and send traffic through the agent first", "docs/config-reference.md#data_dir", "%v", err)
	}
	defer f.Close()
	s := &Set{
		upstream: upstream,
		volatile: map[string]bool{},
		exact:    map[string][]*Recording{}, shape: map[string][]*Recording{}, sequence: map[string][]*Recording{},
	}
	for k := range defaultVolatile {
		s.volatile[k] = true
	}
	for _, v := range extraVolatile {
		s.volatile[strings.ToLower(v)] = true
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec proxy.Record
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber() // amounts keep the recorder's lossless precision
		if err := dec.Decode(&rec); err != nil {
			continue // recordings are drop-oldest logs; a torn tail line is fine
		}
		r := &Recording{Record: rec}
		s.exact[s.exactKey(rec.Method, rec.Path, rec.ReqBody)] = append(s.exact[s.exactKey(rec.Method, rec.Path, rec.ReqBody)], r)
		s.shape[s.shapeKey(rec.Method, rec.Path, rec.ReqBody)] = append(s.shape[s.shapeKey(rec.Method, rec.Path, rec.ReqBody)], r)
		s.sequence[s.seqKey(rec.Method, rec.Path)] = append(s.sequence[s.seqKey(rec.Method, rec.Path)], r)
	}
	if len(s.sequence) == 0 {
		return nil, errfmt.New("recordings file is empty", "the agent has not recorded any traffic for "+upstream, "send traffic through `pikopod up` first", "docs/config-reference.md#data_dir")
	}
	return s, nil
}

// Match finds the recording for a request and records the tier used.
func (s *Set) Match(method, path string, body []byte) (*proxy.Record, MatchTier) {
	return s.MatchValue(method, path, parseBody(body))
}

// MatchValue is Match over an already-parsed body — the sandbox engine's
// recordings tier hands its parsed request body straight through.
func (s *Set) MatchValue(method, path string, parsed any) (*proxy.Record, MatchTier) {
	rec, diag := s.MatchValueDiag(method, path, parsed)
	return rec, diag.Tier
}

// MatchDiag explains HOW a request resolved: a degraded serve or a miss always
// names what eliminated the higher tier, never just "no match".
type MatchDiag struct {
	Tier MatchTier
	// MissedOn: on a shape serve, fields whose VALUES differed; on a sequence
	// serve, field NAMES (+sent-not-recorded / -recorded-not-sent).
	MissedOn []string
	// Closest/ClosestN: on a full miss, the nearest RECORDED endpoint and
	// how many recordings it holds.
	Closest  string
	ClosestN int
	// SeqPos/SeqLen: exact-tier sequence progression, 1-based; Held marks the
	// exhausted state where the LAST response keeps serving.
	SeqPos int
	SeqLen int
	Held   bool
}

// maxReportLines bounds the report: a sandbox holds its Set for the process
// lifetime, where unbounded per-request appends were a slow leak.
const maxReportLines = 10000

func (s *Set) reportLocked(line ReportLine) {
	if len(s.Report) >= maxReportLines {
		return // counters (Unmatched) stay exact; the report is a sample
	}
	s.Report = append(s.Report, line)
}

// MatchValueDiag is MatchValue plus the fidelity diagnostics.
func (s *Set) MatchValueDiag(method, path string, parsed any) (*proxy.Record, MatchDiag) {
	// Keys depend only on the request, so hash BEFORE taking the lock:
	// concurrent sandbox requests must not serialize on hashing.
	exactK := s.exactKey(method, path, parsed)
	shapeK := s.shapeKey(method, path, parsed)
	seqK := s.seqKey(method, path)

	s.mu.Lock()
	defer s.mu.Unlock()

	if recs := s.exact[exactK]; len(recs) > 0 {
		// One request recorded N times is an ordered N-step behavior: serve in
		// order, then HOLD the last — never degrade to the shape tier.
		diag := MatchDiag{Tier: TierExact, SeqLen: len(recs)}
		r := takeUnserved(recs)
		if r == nil {
			r = recs[len(recs)-1]
			diag.SeqPos, diag.Held = len(recs), true
		} else {
			for i := range recs {
				if recs[i] == r {
					diag.SeqPos = i + 1
					break
				}
			}
		}
		s.reportLocked(ReportLine{method, path, TierExact})
		return &r.Record, diag
	}
	if recs := s.shape[shapeK]; len(recs) > 0 {
		r := takeUnserved(recs)
		if r != nil {
			s.reportLocked(ReportLine{method, path, TierShape})
			return &r.Record, MatchDiag{Tier: TierShape, MissedOn: s.valueGap(parsed, r.Record.ReqBody)}
		}
	}
	if recs := s.sequence[seqK]; len(recs) > 0 {
		r := takeUnserved(recs)
		if r == nil {
			// Everything served: wrap around rather than starve long suites.
			recs[0].served = false
			r = recs[0]
		}
		s.reportLocked(ReportLine{method, path, TierSequence})
		return &r.Record, MatchDiag{Tier: TierSequence, MissedOn: shapeGap(parsed, r.Record.ReqBody)}
	}
	s.Unmatched++
	s.reportLocked(ReportLine{method, path, TierMiss})
	diag := MatchDiag{Tier: TierMiss}
	diag.Closest, diag.ClosestN = s.closestRecordedLocked(method, path)
	return nil, diag
}

// valueGap: fields in both bodies whose values differ. Uses its own flatten
// because baseline.Flatten keeps values for strings only, missing amounts.
func (s *Set) valueGap(reqBody, recBody any) []string {
	reqF := map[string]string{}
	flattenScalars(s.stripVolatile(reqBody), "", reqF)
	recF := map[string]string{}
	flattenScalars(s.stripVolatile(recBody), "", recF)
	var out []string
	for path, rv := range reqF {
		if bv, ok := recF[path]; ok && rv != bv {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return capList(out, 6)
}

func flattenScalars(node any, path string, out map[string]string) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			p := k
			if path != "" {
				p = path + "/" + k
			}
			flattenScalars(v, p, out)
		}
	case []any:
		for _, v := range n {
			flattenScalars(v, path+"[]", out)
		}
	default:
		if path != "" {
			out[path] = fmt.Sprint(n)
		}
	}
}

// shapeGap: symmetric field-name diff — why the shape tier missed while the
// endpoint matched. "+" = sent but never recorded, "-" = recorded but absent.
func shapeGap(reqBody, recBody any) []string {
	reqF := baseline.Flatten(reqBody)
	recF := baseline.Flatten(recBody)
	var out []string
	for path := range reqF {
		if _, ok := recF[path]; !ok {
			out = append(out, "+"+path)
		}
	}
	for path := range recF {
		if _, ok := reqF[path]; !ok {
			out = append(out, "-"+path)
		}
	}
	sort.Strings(out)
	return capList(out, 6)
}

func capList(list []string, n int) []string {
	if len(list) > n {
		list = append(list[:n:n], "…")
	}
	return list
}

// ExplainMiss is the read-only closest-recorded lookup for miss rendering
// (no report lines or counters move — safe to call after a match attempt).
func (s *Set) ExplainMiss(method, path string) (closest string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closestRecordedLocked(method, path)
}

// closestRecordedLocked scores every RECORDED endpoint against the request and
// returns the best plus its recording count, for closest-miss rendering.
func (s *Set) closestRecordedLocked(method, path string) (string, int) {
	segs := strings.Split(strings.Trim(stripQuery(path), "/"), "/")
	best, bestScore, bestN := "", 0.0, 0
	for key, recs := range s.sequence {
		m, tmpl, _ := strings.Cut(key, "|")
		tSegs := strings.Split(strings.Trim(tmpl, "/"), "/")
		score := 0.0
		n := len(segs)
		if len(tSegs) < n {
			n = len(tSegs)
		}
		for j := 0; j < n; j++ {
			if strings.Contains(tSegs[j], "{") {
				score += 0.5
			} else if tSegs[j] == segs[j] {
				score++
			}
		}
		score -= 0.25 * float64(absInt(len(tSegs)-len(segs)))
		if strings.EqualFold(m, method) {
			score++
		}
		if score > bestScore {
			best, bestScore, bestN = m+" "+tmpl, score, len(recs)
		}
	}
	return best, bestN
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func takeUnserved(recs []*Recording) *Recording {
	for _, r := range recs {
		if !r.served {
			r.served = true
			return r
		}
	}
	return nil
}

func (s *Set) exactKey(method, path string, body any) string {
	h := sha256.Sum256([]byte(canonicalJSON(s.stripVolatile(body))))
	return method + "|" + normalizePath(path) + "|" + hex.EncodeToString(h[:6])
}

func (s *Set) shapeKey(method, path string, body any) string {
	fields := baseline.Flatten(body)
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	return method + "|" + pathtmpl.Templatize(stripQuery(path)) + "|" + strings.Join(names, ",")
}

func (s *Set) seqKey(method, path string) string {
	return method + "|" + pathtmpl.Templatize(stripQuery(path))
}

// stripVolatile removes volatile fields recursively before hashing.
func (s *Set) stripVolatile(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			if s.volatile[strings.ToLower(k)] {
				continue
			}
			out[k] = s.stripVolatile(v)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = s.stripVolatile(v)
		}
		return out
	default:
		return node
	}
}

// GateResult is the offline CI gate's outcome; its findings mirror drift
// findings for reporting.
type GateResult struct {
	Findings []GateFinding `json:"findings"`
	Records  int           `json:"records"`
	Skipped  int           `json:"skipped_unwarmed"`
}

type GateFinding struct {
	Method   string `json:"method"`
	Template string `json:"template"`
	Kind     string `json:"kind"`
	Field    string `json:"field,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Gate diffs an upstream's recordings offline against its FROZEN baselines.
// Exit-code mapping (0 clean / 1 drift / 2 error) belongs to the CLI.
func Gate(dataDir, upstream string, volatileFields []string) (*GateResult, error) {
	volatile := map[string]bool{}
	for _, v := range volatileFields {
		volatile[strings.ToLower(v)] = true
	}
	fams, err := baseline.LoadFamilies(dataDir, upstream)
	if err != nil {
		return nil, errfmt.New("no baselines for "+upstream, "the agent has not completed warmup (or data_dir differs)", "run `pikopod up`, let warmup finish, then gate", "docs/config-reference.md#baselines")
	}
	path := filepath.Join(dataDir, "recordings", upstream+".ndjson")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errfmt.Newf("no recordings for "+upstream, "send traffic through the agent first", "docs/config-reference.md#data_dir", "%v", err)
	}
	res := &GateResult{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec proxy.Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		res.Records++
		template := pathtmpl.Templatize(stripQuery(rec.Path))
		fam := fams.Find(rec.Method, template, baseline.StatusClass(rec.Status))
		if fam == nil || !fam.Frozen {
			res.Skipped++
			continue
		}
		if rec.RespKind != "json" {
			continue
		}
		for _, f := range drift.DiffRecord(fam, rec.Status, rec.RespBody) {
			if volatile[strings.ToLower(lastSegment(f.Field))] {
				continue // config volatile_fields: never gate on churning fields
			}
			res.Findings = append(res.Findings, GateFinding{Method: rec.Method, Template: template, Kind: f.Kind, Field: f.Field, Detail: f.Detail})
		}
	}
	// Dedup identical findings (a drifted field appears in many records).
	seen := map[string]bool{}
	uniq := res.Findings[:0]
	for _, f := range res.Findings {
		k := f.Method + "|" + f.Template + "|" + f.Kind + "|" + f.Field + "|" + f.Detail
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, f)
		}
	}
	res.Findings = uniq
	return res, nil
}

// lastSegment: "a/b[]/ref" → "ref".
func lastSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(path, "[]")
}

func parseBody(body []byte) any {
	if len(body) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		return v
	}
	return string(body)
}

func canonicalJSON(v any) string {
	raw, err := json.Marshal(canonicalizeNumbers(v)) // json sorts map keys — canonical by construction
	if err != nil {
		return ""
	}
	return string(raw)
}

// canonicalizeNumbers normalizes numeric spellings before hashing so a recorded
// "500.00" and a live 500 share a key; large literals keep their digits.
func canonicalizeNumbers(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, c := range n {
			out[k] = canonicalizeNumbers(c)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, c := range n {
			out[i] = canonicalizeNumbers(c)
		}
		return out
	case json.Number:
		// ≤15 significant digits always round-trip; float64's own round-trip
		// check is useless here, confirming the lossy value it just produced.
		if significantDigits(string(n)) <= 15 {
			if f, err := n.Float64(); err == nil {
				return f
			}
		}
		return n
	default:
		return v
	}
}

// significantDigits counts a JSON number literal's significant decimal
// digits (sign, dot, exponent, and leading zeros excluded).
func significantDigits(s string) int {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		s = s[:i]
	}
	digits, seen := 0, false
	for _, r := range s {
		if r < '0' || r > '9' {
			continue
		}
		if r == '0' && !seen {
			continue // leading zeros
		}
		seen = true
		digits++
	}
	return digits
}

func normalizePath(p string) string { return stripQuery(p) }

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
