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

const SchemaVersion = "2"

type DriftEvent struct {
	SchemaVersion string     `json:"schema_version"`
	Fingerprint   string     `json:"fingerprint"`
	Upstream      string     `json:"upstream"`
	Method        string     `json:"method"`
	Endpoint      string     `json:"endpoint"`
	StatusClass   string     `json:"status_class"`
	Kind          drift.Kind `json:"kind"`
	Field         string     `json:"field,omitempty"`
	Before        string     `json:"before,omitempty"`
	After         string     `json:"after,omitempty"`
	FirstSeen     time.Time  `json:"first_seen"`
	LastSeen      time.Time  `json:"last_seen"`
	Occurrences   int        `json:"occurrences"`

	Source string `json:"source,omitempty"`

	Level string `json:"level,omitempty"`

	Detail string `json:"detail,omitempty"`

	Note string `json:"note,omitempty"`
}

func ObservedLevel(f drift.Finding) string {
	if f.Documented {
		return "INFO"
	}
	switch f.Kind {
	case drift.FieldAdded:
		return "INFO"
	case drift.EnumValueNew:
		return "WARN"

	case drift.UpstreamError, drift.UpstreamUnreachable:
		return "ERR"
	case drift.RateLimited, drift.ClientError:
		return "WARN"
	default:
		return "ERR"
	}
}

type Options struct {
	MinOccurrences int
	Window         time.Duration
	SummaryEvery   time.Duration

	Retention time.Duration

	MinLevel string

	MaxTracked int
}

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

	deliveryWindowStart time.Time
	deliveryWindowCount int

	digestStart   time.Time
	digestCounts  map[string]int
	digestFloored int

	deliveries        chan deliveryItem
	deliveryWG        sync.WaitGroup
	DeliveriesDropped int

	SaturationEvictions int
	saturationNotified  bool
	closed              bool

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

func (a *Alerter) deliveryLoop() {
	for item := range a.deliveries {
		a.deliverSync(item.text, item.countsAsAlert)
		a.deliveryWG.Done()
	}
}

func (a *Alerter) Flush() { a.deliveryWG.Wait() }

func (a *Alerter) SetClock(now func() time.Time) { a.now = now }

func (a *Alerter) Report(f drift.Finding) {
	fp := f.Fingerprint()
	now := a.now()

	a.mu.Lock()
	st, ok := a.states[fp]
	if !ok {

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

func (a *Alerter) emit(ev *DriftEvent, now time.Time) {
	a.log.Append(ev)

	a.mu.Lock()

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

	a.persist()
	if deliverable {
		a.deliver(RenderWithRetention(ev, a.opts.Retention), true)
	} else if suppressedNote {
		a.deliver("pikopod: alert delivery ceiling reached ("+strconv.Itoa(maxDeliveriesPerHour)+"/h) — further alerts this hour are in the local event log (`pikopod status`)", false)
	}
}

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

func (a *Alerter) SweepLog() { a.log.Sweep() }

func (a *Alerter) Sent() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AlertsSent
}

func (a *Alerter) QueueStats() (deliveriesDropped, saturationEvictions, tracked int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DeliveriesDropped, a.SaturationEvictions, len(a.states)
}

func (a *Alerter) DeliveryHealth() (sent int, ok bool, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AlertsSent, a.LastDeliveryOK, a.LastDeliveryErr
}

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

func (a *Alerter) evictOneLocked() bool {
	victim, victimAcked := "", false
	var victimLast time.Time
	for fp, st := range a.states {
		candAcked := st.Acked
		if !candAcked && !st.Alerted {
			continue
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

func Render(ev *DriftEvent) string { return RenderWithRetention(ev, 0) }

func RenderWithRetention(ev *DriftEvent, retention time.Duration) string {
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
		drift.FieldAdded:          "new field",
		drift.FieldRemoved:        "field disappeared",
		drift.TypeChanged:         "type changed",
		drift.EnumValueNew:        "new value",
		drift.StatusNew:           "new status class",
		drift.FieldNullable:       "field went nullable",
		drift.StatusCodeChanged:   "status code changed",
		drift.ErrorShapeChanged:   "error format changed",
		drift.UpstreamError:       "upstream failed",
		drift.UpstreamUnreachable: "upstream unreachable",
		drift.RateLimited:         "rate limited",
		drift.ClientError:         "requests rejected",
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
	case drift.UpstreamError:
		detail = fmt.Sprintf("upstream answered %s — reproduce it locally: `pikopod scenario reproduce %s`", ev.After, ev.Fingerprint)
	case drift.UpstreamUnreachable:
		detail = fmt.Sprintf("pikopod could not reach the upstream (answered %s itself) — no retry was attempted", ev.After)
	case drift.RateLimited:
		detail = fmt.Sprintf("upstream answered %s — reproduce the backoff path: `pikopod scenario reproduce %s`", ev.After, ev.Fingerprint)
	case drift.ClientError:
		detail = fmt.Sprintf("upstream rejected our requests with %s above the configured rate — usually our own payload", ev.After)
	}
	note := ""
	if ev.Note != "" {
		note = "\n_" + ev.Note + "_"
	}
	icon := ":rotating_light:"
	if ev.Note != "" && ev.Level == "INFO" {
		icon = ":memo:"
	}

	word, replay, portable := "drift", "from-drift", ""
	if ev.Kind.IsIncident() {
		word, replay = "incident", "reproduce"
		if retention > 0 {
			portable = " · reproducible until " + ev.LastSeen.Add(retention).UTC().Format(time.RFC3339)
		}
		portable += "\nexport: `pikopod incidents export " + ev.Fingerprint + "`"
	}
	return fmt.Sprintf(
		"%s *pikopod %s — %s* on `%s %s` (%s)\n%s%s\nfingerprint `%s` · first seen %s · %d occurrence(s)%s\nreplay it: `pikopod scenario %s %s`",
		icon, word, head, ev.Method, ev.Endpoint, ev.Upstream, detail, note,
		ev.Fingerprint, ev.FirstSeen.Format(time.RFC3339), ev.Occurrences, portable, replay, ev.Fingerprint)
}

func (a *Alerter) persist() {

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
		a.Flush()
		close(a.deliveries)
	}
	a.log.Close()
}

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
