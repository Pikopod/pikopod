// Package alert turns drift findings into humans being paged: N-in-window
// emission, per-fingerprint dedupe, persisted ack state, sanitized egress.
package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/store"
)

// SchemaVersion of DriftEvent — the versioned protocol.
const SchemaVersion = "1"

// DriftEvent is the one shape emitted everywhere (stdout, Slack, event log).
// schema/drift-event.schema.json is its contract.
type DriftEvent struct {
	SchemaVersion string     `json:"schema_version"`
	Fingerprint   string     `json:"fingerprint"`
	Upstream      string     `json:"upstream"`
	Method        string     `json:"method"`
	Endpoint      string     `json:"endpoint"` // path template, never a concrete path
	StatusClass   string     `json:"status_class"`
	Kind          drift.Kind `json:"kind"`
	Field         string     `json:"field,omitempty"`
	Before        string     `json:"before,omitempty"`
	After         string     `json:"after,omitempty"`
	FirstSeen     time.Time  `json:"first_seen"`
	LastSeen      time.Time  `json:"last_seen"`
	Occurrences   int        `json:"occurrences"`
	// Source is "declared" (spec vs spec) vs observed (traffic vs baselines);
	// empty for compatibility with persisted events.
	Source string `json:"source,omitempty"`
	// Level is the risk-classified severity (ERR/WARN/INFO). Empty on events
	// persisted before it existed.
	Level string `json:"level,omitempty"`
	// Detail is the pre-rendered human sentence for declared findings (their
	// vocabulary is open-ended; observed kinds render from the fields above).
	Detail string `json:"detail,omitempty"`
	// Note carries declared×observed join context (e.g. "documented: the
	// provider's new spec version declares this change").
	Note string `json:"note,omitempty"`
}

// ObservedLevel grades by what breaks consumers: removals/type changes break
// parsers (ERR), new enum values break switches (WARN), additions are INFO.
func ObservedLevel(f drift.Finding) string {
	if f.Documented {
		return "INFO"
	}
	switch f.Kind {
	case drift.FieldAdded:
		return "INFO"
	case drift.EnumValueNew:
		return "WARN"
	default:
		return "ERR"
	}
}

// Options tune emission; zero values take defaults. Demo/eval lower them.
type Options struct {
	MinOccurrences int           // default 3
	Window         time.Duration // default 15m
	SummaryEvery   time.Duration // default 15m; 0 disables the summary loop
	// Retention ages the event log out of disk (0 = size-only rotation).
	// Dedupe/ack state is NOT aged: forgetting it would re-alert old drifts.
	Retention time.Duration
	// MinLevel floors sink DELIVERY by severity ("" delivers all). The event
	// log, dedupe state and digest still see everything.
	MinLevel string
	// MaxTracked overrides the fingerprint-state cap (tests; 0 = the
	// production default maxTrackedFingerprints).
	MaxTracked int
}

// levelRank orders severities for the delivery floor. Events persisted
// before Level existed rank as ERR — the safe default is to deliver.
func levelRank(level string) int {
	switch level {
	case "INFO":
		return 0
	case "WARN":
		return 1
	default:
		return 2
	}
}

// maxTrackedFingerprints / maxDeliveriesPerHour bound alerting state and
// egress under a drift storm.
const (
	maxTrackedFingerprints = 10000
	maxDeliveriesPerHour   = 60
)

type fpState struct {
	First       time.Time   `json:"first"`
	Last        time.Time   `json:"last"`
	Occurrences int         `json:"occurrences"`
	WindowStart time.Time   `json:"window_start"`
	WindowCount int         `json:"window_count"`
	Alerted     bool        `json:"alerted"`
	Acked       bool        `json:"acked"`
	Event       *DriftEvent `json:"event,omitempty"`
}

// Sink delivers a rendered alert. Errors are recorded, never fatal.
type Sink interface {
	Deliver(text string) error
	Name() string
}

type Alerter struct {
	mu        sync.Mutex
	opts      Options
	states    map[string]*fpState
	statePath string
	log       *store.NDJSON
	sinks     []Sink
	now       func() time.Time

	// Delivery rate-ceiling window.
	deliveryWindowStart time.Time
	deliveryWindowCount int

	// Digest window (slack.digest_hours): new-alert counts by source|level
	// since the last digest post, plus how many the min_level floor muted.
	digestStart   time.Time
	digestCounts  map[string]int
	digestFloored int

	// Async delivery: the caller is the recorder's single observe goroutine,
	// so a wedged webhook must never stall it. Bounded queue, drop-and-count.
	deliveries        chan deliveryItem
	deliveryWG        sync.WaitGroup
	DeliveriesDropped int

	// Saturation bookkeeping: the fingerprint-state cap must never become a
	// silent, permanent alert blackout.
	SaturationEvictions int
	saturationNotified  bool
	closed              bool

	// Delivery health for /healthz.
	LastDeliveryOK  bool
	LastDeliveryErr string
	AlertsSent      int
}

type deliveryItem struct {
	text          string
	countsAsAlert bool
}

func New(dataDir string, opts Options, sinks ...Sink) (*Alerter, error) {
	if opts.MinOccurrences <= 0 {
		opts.MinOccurrences = 3
	}
	if opts.Window <= 0 {
		opts.Window = 15 * time.Minute
	}
	if opts.MaxTracked <= 0 {
		opts.MaxTracked = maxTrackedFingerprints
	}
	log, err := store.OpenNDJSON(filepath.Join(dataDir, "events.ndjson"), 64<<20)
	if err != nil {
		return nil, err
	}
	if opts.Retention > 0 {
		log.SetTTL(opts.Retention)
	}
	a := &Alerter{opts: opts, states: map[string]*fpState{}, statePath: filepath.Join(dataDir, "alerts", "state.json"), log: log, sinks: sinks, now: time.Now,
		deliveries: make(chan deliveryItem, 256)}
	a.load()
	go a.deliveryLoop()
	return a, nil
}

// deliveryLoop drains the queue on its own goroutine — the only place sink
// network I/O happens.
func (a *Alerter) deliveryLoop() {
	for item := range a.deliveries {
		a.deliverSync(item.text, item.countsAsAlert)
		a.deliveryWG.Done()
	}
}

// Flush blocks until every queued delivery has been attempted (tests and
// orderly shutdown; a wedged sink still bounds this via its own timeout).
func (a *Alerter) Flush() { a.deliveryWG.Wait() }

func (a *Alerter) SetClock(now func() time.Time) { a.now = now }

// Report ingests one finding occurrence, emitting at most one alert per
// fingerprint once the N-in-window threshold clears.
func (a *Alerter) Report(f drift.Finding) {
	fp := f.Fingerprint()
	now := a.now()

	a.mu.Lock()
	st, ok := a.states[fp]
	if !ok {
		// At the cap the least-valuable state is evicted so a NEW drift is
		// still tracked; only an unevictable storm drops it, and it notifies.
		if len(a.states) >= a.opts.MaxTracked && !a.evictOneLocked() {
			notify := !a.saturationNotified
			a.saturationNotified = true
			a.mu.Unlock()
			if notify {
				a.deliver("pikopod: fingerprint-state SATURATED ("+strconv.Itoa(a.opts.MaxTracked)+" live un-alerted windows) — new drifts are not being tracked; `pikopod baseline reset` or ack standing alerts to recover", false)
			}
			return
		}
		st = &fpState{First: now, WindowStart: now}
		a.states[fp] = st
	}
	st.Last = now
	st.Occurrences++
	if now.Sub(st.WindowStart) > a.opts.Window {
		st.WindowStart, st.WindowCount = now, 0
	}
	st.WindowCount++

	shouldAlert := !st.Alerted && !st.Acked && st.WindowCount >= a.opts.MinOccurrences
	var ev *DriftEvent
	if shouldAlert {
		st.Alerted = true
		ev = &DriftEvent{
			SchemaVersion: SchemaVersion, Fingerprint: fp,
			Upstream: f.Upstream, Method: f.Method, Endpoint: f.Template,
			StatusClass: f.StatusClass, Kind: f.Kind, Field: f.Field,
			Before: f.Before, After: f.After,
			Level: ObservedLevel(f), Note: f.Note,
			FirstSeen: st.First, LastSeen: st.Last, Occurrences: st.Occurrences,
		}
		st.Event = ev
	} else if st.Event != nil {
		st.Event.LastSeen, st.Event.Occurrences = st.Last, st.Occurrences
	}
	a.mu.Unlock()

	if ev != nil {
		a.emit(ev, now)
	}
}

// ReportDeclared ingests one DECLARED-drift finding. A spec diff is
// deterministic, so it alerts on FIRST occurrence; dedupe still applies.
func (a *Alerter) ReportDeclared(upstream string, fingerprint string, ev DriftEvent) {
	now := a.now()

	a.mu.Lock()
	st, ok := a.states[fingerprint]
	if !ok {
		if len(a.states) >= a.opts.MaxTracked && !a.evictOneLocked() {
			a.mu.Unlock()
			return
		}
		st = &fpState{First: now, WindowStart: now}
		a.states[fingerprint] = st
	}
	st.Last = now
	st.Occurrences++
	shouldAlert := !st.Alerted && !st.Acked
	var out *DriftEvent
	if shouldAlert {
		st.Alerted = true
		ev.SchemaVersion = SchemaVersion
		ev.Fingerprint = fingerprint
		ev.Upstream = upstream
		ev.Source = "declared"
		ev.FirstSeen, ev.LastSeen, ev.Occurrences = st.First, st.Last, st.Occurrences
		st.Event = &ev
		out = &ev
	} else if st.Event != nil {
		st.Event.LastSeen, st.Event.Occurrences = st.Last, st.Occurrences
	}
	a.mu.Unlock()

	if out != nil {
		a.emit(out, now)
	}
}

// emit logs one first-alert event and delivers it under the global ceiling
// so a drift storm cannot flood Slack; the event log still carries all.
func (a *Alerter) emit(ev *DriftEvent, now time.Time) {
	a.log.Append(ev) // the local log always gets the event

	a.mu.Lock()
	// Digest accounting sees EVERY new alert, delivered or floored.
	if a.digestCounts == nil {
		a.digestCounts = map[string]int{}
	}
	source := ev.Source
	if source == "" {
		source = "observed"
	}
	level := ev.Level
	if level == "" {
		level = "ERR"
	}
	a.digestCounts[source+"|"+level]++

	// An unset floor delivers everything: levelRank's ERR default errs loud
	// for event levels, but the floor itself must err open.
	floor := 0
	if a.opts.MinLevel != "" {
		floor = levelRank(a.opts.MinLevel)
	}
	if levelRank(level) < floor {
		a.digestFloored++
		a.mu.Unlock()
		a.persist()
		return
	}
	if now.Sub(a.deliveryWindowStart) > time.Hour {
		a.deliveryWindowStart, a.deliveryWindowCount = now, 0
	}
	a.deliveryWindowCount++
	deliverable := a.deliveryWindowCount <= maxDeliveriesPerHour
	suppressedNote := a.deliveryWindowCount == maxDeliveriesPerHour+1
	a.mu.Unlock()
	// Persist the Alerted latch BEFORE the network delivery; persisting after
	// would re-alert the same fingerprint on a crash-restart.
	a.persist()
	if deliverable {
		a.deliver(Render(ev), true)
	} else if suppressedNote {
		a.deliver("pikopod: alert delivery ceiling reached ("+strconv.Itoa(maxDeliveriesPerHour)+"/h) — further alerts this hour are in the local event log (`pikopod status`)", false)
	}
}

// MaybeDigest posts the periodic digest when SummaryEvery has elapsed. The
// window is in-memory: a restart starts fresh — the event log is the record.
func (a *Alerter) MaybeDigest() {
	a.mu.Lock()
	if a.opts.SummaryEvery <= 0 {
		a.mu.Unlock()
		return
	}
	now := a.now()
	if a.digestStart.IsZero() {
		a.digestStart = now
		a.mu.Unlock()
		return
	}
	if now.Sub(a.digestStart) < a.opts.SummaryEvery || len(a.digestCounts) == 0 {
		a.mu.Unlock()
		return
	}
	counts, floored := a.digestCounts, a.digestFloored
	a.digestCounts, a.digestFloored, a.digestStart = map[string]int{}, 0, now
	a.mu.Unlock()

	a.deliver(renderDigest(counts, floored), false)
}

// renderDigest summarizes new findings by source and severity.
func renderDigest(counts map[string]int, floored int) string {
	part := func(source string) string {
		total := counts[source+"|ERR"] + counts[source+"|WARN"] + counts[source+"|INFO"]
		if total == 0 {
			return ""
		}
		return fmt.Sprintf("%s drift %d (%d ERR, %d WARN, %d INFO)",
			source, total, counts[source+"|ERR"], counts[source+"|WARN"], counts[source+"|INFO"])
	}
	var parts []string
	for _, s := range []string{"observed", "declared"} {
		if p := part(s); p != "" {
			parts = append(parts, p)
		}
	}
	msg := ":memo: *pikopod digest* — new since the last digest: " + strings.Join(parts, "; ")
	if floored > 0 {
		msg += fmt.Sprintf(" · %d alert(s) below slack.min_level (event log has them)", floored)
	}
	return msg + "\ndetails: `pikopod status`"
}

// SweepLog enforces the event log's retention TTL (the agent's persist
// tick calls it); no-op when retention is off.
func (a *Alerter) SweepLog() { a.log.Sweep() }

// Sent returns how many alerts have been delivered (lock-safe — AlertsSent
// itself is written under the mutex, so concurrent pollers must come here).
func (a *Alerter) Sent() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AlertsSent
}

// QueueStats snapshots the delivery-queue and saturation counters for
// /healthz (written under the mutex, so read under it too).
func (a *Alerter) QueueStats() (deliveriesDropped, saturationEvictions, tracked int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DeliveriesDropped, a.SaturationEvictions, len(a.states)
}

// DeliveryHealth snapshots the delivery-state fields for /healthz — the
// fields are written under the mutex, so concurrent readers must be too.
func (a *Alerter) DeliveryHealth() (sent int, ok bool, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AlertsSent, a.LastDeliveryOK, a.LastDeliveryErr
}

// EventFor returns the alerted event for a fingerprint, if any.
func (a *Alerter) EventFor(fingerprint string) (*DriftEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.states[fingerprint]
	if !ok || st.Event == nil {
		return nil, false
	}
	ev := *st.Event
	return &ev, true
}

// Ack suppresses a fingerprint until its diff changes (a changed diff is a
// new fingerprint by construction).
func (a *Alerter) Ack(fingerprint string) bool {
	a.mu.Lock()
	st, ok := a.states[fingerprint]
	if ok {
		st.Acked = true
	}
	a.mu.Unlock()
	if ok {
		a.persist()
	}
	return ok
}

// Active returns alerted, un-acked events (for status/summary), newest last.
func (a *Alerter) Active() []DriftEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []DriftEvent
	for _, st := range a.states {
		if st.Alerted && !st.Acked && st.Event != nil {
			out = append(out, *st.Event)
		}
	}
	return out
}

// deliver ENQUEUES one message so the observe path never blocks on sink I/O.
// countsAsAlert excludes housekeeping posts from AlertsSent.
func (a *Alerter) deliver(text string, countsAsAlert bool) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.deliveryWG.Add(1)
	a.mu.Unlock()
	select {
	case a.deliveries <- deliveryItem{text: text, countsAsAlert: countsAsAlert}:
	default:
		a.deliveryWG.Done()
		a.mu.Lock()
		a.DeliveriesDropped++
		a.LastDeliveryOK, a.LastDeliveryErr = false, "delivery queue full — message dropped (event log has it)"
		a.mu.Unlock()
	}
}

// deliverSync pushes one message to every sink (delivery goroutine only).
func (a *Alerter) deliverSync(text string, countsAsAlert bool) {
	ok := true
	var lastErr string
	for _, s := range a.sinks {
		if err := s.Deliver(text); err != nil {
			ok, lastErr = false, s.Name()+": "+err.Error()
		}
	}
	a.mu.Lock()
	if countsAsAlert {
		a.AlertsSent++
	}
	a.LastDeliveryOK, a.LastDeliveryErr = ok, lastErr
	a.mu.Unlock()
}

// evictOneLocked frees one slot: acked first, then oldest alerted. Un-alerted
// live windows are never evicted (pending signal). Requires a.mu.
func (a *Alerter) evictOneLocked() bool {
	victim, victimAcked := "", false
	var victimLast time.Time
	for fp, st := range a.states {
		candAcked := st.Acked
		if !candAcked && !st.Alerted {
			continue // pending window — never evict
		}
		better := false
		switch {
		case victim == "":
			better = true
		case candAcked && !victimAcked:
			better = true
		case candAcked == victimAcked && st.Last.Before(victimLast):
			better = true
		}
		if better {
			victim, victimAcked, victimLast = fp, candAcked, st.Last
		}
	}
	if victim == "" {
		return false
	}
	delete(a.states, victim)
	a.SaturationEvictions++
	return true
}

// Render formats the one loud message (Slack-markdown-compatible plain text).
func Render(ev *DriftEvent) string {
	if ev.Source == "declared" {
		icon := map[string]string{"ERR": ":rotating_light:", "WARN": ":warning:", "INFO": ":memo:"}[ev.Level]
		if icon == "" {
			icon = ":memo:"
		}
		return fmt.Sprintf(
			"%s *pikopod declared drift [%s]* on `%s %s` (%s)\n%s\nfingerprint `%s` · the provider changed their SPEC — this is documented, not observed on the wire\nreview: `pikopod spec-diff` output, then `pikopod import %s --update` to accept",
			icon, ev.Level, ev.Method, ev.Endpoint, ev.Upstream, ev.Detail,
			ev.Fingerprint, ev.Upstream)
	}
	head := map[drift.Kind]string{
		drift.FieldAdded:        "new field",
		drift.FieldRemoved:      "field disappeared",
		drift.TypeChanged:       "type changed",
		drift.EnumValueNew:      "new value",
		drift.StatusNew:         "new status class",
		drift.FieldNullable:     "field went nullable",
		drift.StatusCodeChanged: "status code changed",
		drift.ErrorShapeChanged: "error format changed",
	}[ev.Kind]
	var detail string
	switch ev.Kind {
	case drift.FieldAdded:
		detail = fmt.Sprintf("`%s` (%s) appeared in responses", ev.Field, ev.After)
	case drift.FieldRemoved:
		detail = fmt.Sprintf("`%s` (%s) is gone from responses that always carried it", ev.Field, ev.Before)
	case drift.TypeChanged:
		detail = fmt.Sprintf("`%s`: %s → %s", ev.Field, ev.Before, ev.After)
	case drift.EnumValueNew:
		detail = fmt.Sprintf("`%s`: value %q not in known set [%s]", ev.Field, ev.After, ev.Before)
	case drift.StatusNew:
		detail = fmt.Sprintf("endpoint started returning %s", ev.After)
	case drift.FieldNullable:
		detail = fmt.Sprintf("`%s` (always %s before) arrived null — parsers that never handled null will break", ev.Field, ev.Before)
	case drift.StatusCodeChanged:
		detail = fmt.Sprintf("endpoint answered %s (known: %s) — exact-status matches will break", ev.After, ev.Before)
	case drift.ErrorShapeChanged:
		detail = fmt.Sprintf("error body restructured: [%s] → [%s] — error-handling paths parse the old shape", ev.Before, ev.After)
	}
	note := ""
	if ev.Note != "" {
		note = "\n_" + ev.Note + "_"
	}
	icon := ":rotating_light:"
	if ev.Note != "" && ev.Level == "INFO" {
		icon = ":memo:" // a documented change informs; it does not page
	}
	return fmt.Sprintf(
		"%s *pikopod drift — %s* on `%s %s` (%s)\n%s%s\nfingerprint `%s` · first seen %s · %d occurrence(s)\nreplay it: `pikopod scenario from-drift %s`",
		icon, head, ev.Method, ev.Endpoint, ev.Upstream, detail, note,
		ev.Fingerprint, ev.FirstSeen.Format(time.RFC3339), ev.Occurrences, ev.Fingerprint)
}

func (a *Alerter) persist() {
	// Snapshot under the lock (cheap value copies), marshal + write OUTSIDE
	// it: a multi-MB MarshalIndent under a.mu stalled the observe path.
	a.mu.Lock()
	snap := make(map[string]fpState, len(a.states))
	for fp, st := range a.states {
		c := *st
		if st.Event != nil {
			ev := *st.Event
			c.Event = &ev
		}
		snap[fp] = c
	}
	a.mu.Unlock()
	raw, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return
	}
	_ = store.WriteFileAtomic(a.statePath, raw)
}

func (a *Alerter) load() {
	raw, err := os.ReadFile(a.statePath)
	if err != nil {
		return
	}
	var states map[string]*fpState
	if json.Unmarshal(raw, &states) == nil && states != nil {
		a.states = states
	}
}

func (a *Alerter) Close() {
	a.mu.Lock()
	alreadyClosed := a.closed
	a.closed = true
	a.mu.Unlock()
	if !alreadyClosed {
		a.Flush() // every accepted delivery attempted before the loop stops
		close(a.deliveries)
	}
	a.log.Close()
}

// StdoutSink prints alerts (demo + default when Slack is unconfigured).
type StdoutSink struct{ W *os.File }

func (s StdoutSink) Name() string { return "stdout" }
func (s StdoutSink) Deliver(text string) error {
	w := s.W
	if w == nil {
		w = os.Stdout
	}
	_, err := fmt.Fprintln(w, "\n"+PlainText(text))
	return err
}

var (
	reBold   = regexp.MustCompile(`\*([^*\n]+)\*`)
	reItalic = regexp.MustCompile(`(?m)^_(.*)_$`)
)

// PlainText renders a Slack mrkdwn alert body for a terminal. Italics match
// whole-line only — field names carry underscores that are not markup.
func PlainText(s string) string {
	s = strings.NewReplacer(
		":rotating_light:", "[ERR]",
		":warning:", "[WARN]",
		":memo:", "[INFO]",
		"`", "",
	).Replace(s)
	s = reBold.ReplaceAllString(s, "$1")
	s = reItalic.ReplaceAllString(s, "$1")
	return strings.TrimLeft(s, " ")
}

// SlackWebhookSink posts to an incoming webhook (no edit capability —
// summaries arrive as separate posts).
type SlackWebhookSink struct {
	URL    string
	Client *http.Client
}

func (s SlackWebhookSink) Name() string { return "slack-webhook" }
func (s SlackWebhookSink) Deliver(text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Post(s.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack returned %d", resp.StatusCode)
	}
	return nil
}
