package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/behaviour"
	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/contract"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/sanitize"
	"github.com/pikopod/pikopod/internal/specwatch"
	"github.com/pikopod/pikopod/internal/store"
	"github.com/pikopod/pikopod/internal/volatile"
)

type Agent struct {
	Cfg      *config.Config
	Metrics  *proxy.Metrics
	Proxy    *proxy.Server
	Recorder *proxy.Recorder
	Alerter  *alert.Alerter

	mu       sync.Mutex
	learners map[string]*baseline.Learner

	classes map[string]map[string]bool

	clientErrs map[string]errCounts

	muted map[string]map[string]bool

	refiners map[string]*contract.Refiner

	behaviours map[string]*behaviour.Tracker

	contracts map[string]*ir.ApiDefinition

	volatile map[string]*volatile.Matcher
	started  time.Time

	watch *specwatch.Watcher

	documented map[string]*documentedCache

	EventsEmitted atomic.Int64
}

type errCounts struct{ total, errors int }

func New(cfg *config.Config, alertOpts alert.Options, extraSinks ...alert.Sink) (*Agent, error) {
	salt, err := store.LoadOrCreateSalt(cfg.SaltPath())
	if err != nil {
		return nil, err
	}
	tok := sanitize.NewTokenizer(salt, "local", 1)
	metrics := &proxy.Metrics{}
	p, err := proxy.New(cfg, metrics, 1024)
	if err != nil {
		return nil, err
	}

	sinks := extraSinks
	if cfg.Slack.WebhookURL != "" {
		sinks = append(sinks, alert.SlackWebhookSink{URL: cfg.Slack.WebhookURL})
	}
	if len(sinks) == 0 {
		sinks = append(sinks, alert.StdoutSink{})
	}
	al, err := alert.New(cfg.DataDir, alertOpts, sinks...)
	if err != nil {
		return nil, err
	}

	muted := map[string]map[string]bool{}
	for name, up := range cfg.Upstreams {
		if len(up.Mute) == 0 {
			continue
		}
		set := map[string]bool{}
		for _, tmpl := range up.Mute {
			set[tmpl] = true
		}
		muted[name] = set
	}
	matchers := map[string]*volatile.Matcher{}
	for name, up := range cfg.Upstreams {
		m, _, err := volatile.Compile(up.VolatileFields)
		if err != nil {
			return nil, err
		}
		matchers[name] = m
	}
	a := &Agent{
		Cfg: cfg, Metrics: metrics, Proxy: p, Alerter: al, volatile: matchers,
		learners:   map[string]*baseline.Learner{},
		classes:    map[string]map[string]bool{},
		clientErrs: map[string]errCounts{},
		muted:      muted,
		refiners:   map[string]*contract.Refiner{},
		behaviours: map[string]*behaviour.Tracker{},
		contracts:  map[string]*ir.ApiDefinition{},
		started:    time.Now(),
	}
	rec := proxy.NewRecorder(cfg.DataDir, tok, metrics)
	rec.SetObserver(a.observe)
	rec.SetSampling(cfg.SampleRate())
	rec.SetRetention(cfg.RetentionTTL())
	a.Recorder = rec
	p.SetHealthz(http.HandlerFunc(a.healthz))
	p.SetAck(http.HandlerFunc(a.ackHandler))
	p.SetAccept(http.HandlerFunc(a.acceptHandler))
	return a, nil
}

func (a *Agent) acceptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	fp := r.URL.Query().Get("fp")
	ev, ok := a.Alerter.EventFor(fp)
	if !ok {
		http.Error(w, "unknown fingerprint "+fp, http.StatusNotFound)
		return
	}
	n := a.learner(ev.Upstream).Refreeze(ev.Method, ev.Endpoint)
	a.Alerter.Ack(fp)
	a.persistAll()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"accepted":%q,"refrozen_families":%d}`, fp, n)
}

func (a *Agent) ackHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	fp := r.URL.Query().Get("fp")
	if fp == "" {
		http.Error(w, "missing fp query parameter", http.StatusBadRequest)
		return
	}
	if !a.Alerter.Ack(fp) {
		http.Error(w, "unknown fingerprint "+fp, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"acked":%q}`, fp)
}

func (a *Agent) learner(upstream string) *baseline.Learner {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.learners[upstream]
	if !ok {
		l = baseline.NewLearner(upstream, a.Cfg.DataDir, baseline.Warmup{
			MinSamples: a.Cfg.Warmup.MinSamples,
			MinAge:     time.Duration(*a.Cfg.Warmup.MinHours) * time.Hour,
		})
		l.SetVolatileMatcher(a.volatile[upstream])

		l.SetValueVolatile(volatile.ResponseFieldNames())
		a.learners[upstream] = l
	}
	return l
}

func (a *Agent) VolatileMatcher(upstream string) *volatile.Matcher { return a.volatile[upstream] }

func (a *Agent) SetWatcher(w *specwatch.Watcher) { a.watch = w }

func (a *Agent) SetSpecRules(rules map[string][]sanitize.Rule) { a.Recorder.SetSpecRules(rules) }

func (a *Agent) SetContracts(m map[string]*ir.ApiDefinition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range m {
		a.contracts[k] = v
	}
}

func (a *Agent) refiner(upstream string) *contract.Refiner {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.refiners[upstream]
	if !ok {
		r = contract.NewRefiner(upstream, a.Cfg.DataDir, a.Cfg.Warmup.MinSamples,
			time.Duration(*a.Cfg.Warmup.MinHours)*time.Hour)
		a.refiners[upstream] = r
	}
	return r
}

func (a *Agent) behaviour(upstream string) *behaviour.Tracker {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.behaviours[upstream]
	if !ok {
		t = behaviour.New(upstream, a.Cfg.DataDir, a.Cfg.Warmup.MinSamples, time.Duration(*a.Cfg.Warmup.MinHours)*time.Hour)
		if m := a.volatile[upstream]; m != nil {
			t.SetVolatile(m)
		}
		t.SetMuted(a.muted[upstream])
		a.behaviours[upstream] = t
	}
	return t
}

func (a *Agent) Behaviour(upstream string) *behaviour.Tracker {
	if !a.Cfg.Behaviour.Enabled {
		return nil
	}
	return a.behaviour(upstream)
}

func (a *Agent) observe(rec *proxy.Record) (notable bool) {
	defer func() {
		if p := recover(); p != nil {
			a.Metrics.ObserverPanics.Add(1)
		}
	}()

	if a.Cfg.Refine.Enabled {
		a.refiner(rec.Upstream).Observe(rec)
	}
	if a.Cfg.Behaviour.Enabled {
		a.behaviour(rec.Upstream).Observe(rec)
	}

	l := a.learner(rec.Upstream)
	pathOnly := rec.Path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	obs := l.Observe(rec.Method, pathOnly, rec.Status, rec.RespBody, rec.TS)

	notable = !obs.Ready

	if a.muted[rec.Upstream][obs.Template] {
		return notable
	}

	if kind, ok := a.incidentKind(rec, obs.Template); ok {
		notable = true
		a.EventsEmitted.Add(1)
		a.Alerter.Report(drift.Finding{
			Upstream: rec.Upstream, Method: rec.Method, Template: obs.Template,
			StatusClass: obs.Family.StatusClass, Kind: kind,
			After: strconv.Itoa(rec.Status),
		})
	}

	classKey := rec.Upstream + "|" + rec.Method + "|" + obs.Template
	a.mu.Lock()
	known := a.classes[classKey]
	if known == nil {
		known = map[string]bool{}
		a.classes[classKey] = known
	}
	newClass := !known[obs.Family.StatusClass]
	known[obs.Family.StatusClass] = true
	a.mu.Unlock()

	hasFrozenSibling := l.HasFrozen(rec.Method, obs.Template)

	if newClass && hasFrozenSibling && !obs.Ready {
		notable = true
		a.EventsEmitted.Add(1)
		fs := []drift.Finding{{
			Upstream: rec.Upstream, Method: rec.Method, Template: obs.Template,
			StatusClass: obs.Family.StatusClass, Kind: drift.StatusNew,
			After: obs.Family.StatusClass,
		}}
		AnnotateDocumented(fs, a.documentedFor(rec.Upstream))
		a.Alerter.Report(fs[0])
	}

	if !obs.Ready || rec.RespKind != "json" {
		return notable
	}
	findings := drift.Diff(rec.Upstream, obs, map[string]bool{obs.Family.StatusClass: true})
	if len(findings) > 0 {
		notable = true
		AnnotateDocumented(findings, a.documentedFor(rec.Upstream))
	}
	for _, f := range findings {
		a.EventsEmitted.Add(1)
		a.Alerter.Report(f)
	}
	return notable
}

func (a *Agent) incidentKind(rec *proxy.Record, template string) (drift.Kind, bool) {
	isClientErr := rec.Status >= 400 && rec.Status < 500
	total, errs := a.countExchange(rec.Upstream, rec.Method, template, isClientErr)

	switch {
	case rec.Status == 429:
		return drift.RateLimited, true
	case rec.Status >= 500:
		if headerHas(rec.RespHeader, "x-pikopod-error", "upstream-unreachable") {
			return drift.UpstreamUnreachable, true
		}
		return drift.UpstreamError, true
	case isClientErr:
		up := a.Cfg.Upstreams[rec.Upstream]
		if !up.Incidents.ClientErrors || total < config.ClientErrorFloor() {
			return "", false
		}
		if float64(errs)/float64(total) > up.ClientErrorRateFor() {
			return drift.ClientError, true
		}
	}
	return "", false
}

func (a *Agent) countExchange(upstream, method, template string, isClientErr bool) (total, errs int) {
	key := upstream + "|" + method + "|" + template
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.clientErrs[key]
	c.total++
	if isClientErr {
		c.errors++
	}
	a.clientErrs[key] = c
	return c.total, c.errors
}

func headerHas(h map[string]any, key, want string) bool {
	for k, v := range h {
		if !strings.EqualFold(k, key) {
			continue
		}
		switch t := v.(type) {
		case string:
			return strings.EqualFold(t, want)
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok && strings.EqualFold(s, want) {
					return true
				}
			}
		}
	}
	return false
}

func (a *Agent) Run(ctx context.Context) error {
	go a.Recorder.Run(a.Proxy.Captures())

	addr := net.JoinHostPort(a.Cfg.Listen, fmt.Sprint(a.Cfg.AgentPort))
	srv := &http.Server{Addr: addr, Handler: a.Proxy, ReadHeaderTimeout: 20 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		if a.Cfg.TLS.Enabled() {
			errCh <- srv.ListenAndServeTLS(a.Cfg.TLS.CertFile, a.Cfg.TLS.KeyFile)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.persistAll()

			a.Alerter.Close()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {

				srv.Close()
			}
			return nil
		case err := <-errCh:
			return err
		case <-tick.C:
			a.persistAll()
			if a.watch != nil {
				go func() {
					defer func() {
						if p := recover(); p != nil {
							a.Metrics.ObserverPanics.Add(1)
						}
					}()
					a.watch.Check()
				}()
			}
		}
	}
}

func (a *Agent) persistAll() {
	if a.Cfg.Refine.Enabled {
		a.mu.Lock()
		refiners := make(map[string]*contract.Refiner, len(a.refiners))
		for k, v := range a.refiners {
			refiners[k] = v
		}
		contracts := a.contracts
		preferSpec := a.Cfg.Refine.PreferSpec
		a.mu.Unlock()
		for upstream, r := range refiners {
			if def := contracts[upstream]; def != nil {
				r.Admit(def, preferSpec)
			}
			r.Persist()
		}
	}
	a.mu.Lock()
	ls := make([]*baseline.Learner, 0, len(a.learners))
	for _, l := range a.learners {
		ls = append(ls, l)
	}
	trackers := make([]*behaviour.Tracker, 0, len(a.behaviours))
	for _, t := range a.behaviours {
		trackers = append(trackers, t)
	}
	a.mu.Unlock()
	for _, t := range trackers {
		t.Persist()
	}
	for _, l := range ls {
		l.Persist()
	}

	a.Recorder.Sweep()
	a.Alerter.SweepLog()

	a.Alerter.MaybeDigest()
}

func (a *Agent) Learners() map[string]*baseline.Learner {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]*baseline.Learner, len(a.learners))
	for k, v := range a.learners {
		out[k] = v
	}
	return out
}

func (a *Agent) healthz(w http.ResponseWriter, r *http.Request) {
	type famView struct {
		Endpoint    string `json:"endpoint"`
		StatusClass string `json:"status_class"`
		Samples     int    `json:"samples"`
		Frozen      bool   `json:"warmed_up"`
	}
	fams := []famView{}
	for upstream, l := range a.Learners() {
		for _, f := range l.Families() {
			fams = append(fams, famView{Endpoint: upstream + " " + f.Method + " " + f.Template, StatusClass: f.StatusClass, Samples: f.Samples, Frozen: f.Frozen})
		}
	}
	alertsSent, deliveryOK, deliveryErr := a.Alerter.DeliveryHealth()
	deliveriesDropped, saturationEvictions, trackedFPs := a.Alerter.QueueStats()
	out := map[string]any{
		"status":                 "ok",
		"uptime_seconds":         int(time.Since(a.started).Seconds()),
		"requests_proxied":       a.Metrics.RequestsProxied.Load(),
		"upstream_errors":        a.Metrics.UpstreamErrors.Load(),
		"upstream_body_errors":   a.Metrics.UpstreamBodyErrors.Load(),
		"recordings_written":     a.Metrics.RecordingsWritten.Load(),
		"recordings_dropped":     a.Metrics.CapturesDropped.Load(),
		"recordings_sampled_out": a.Metrics.RecordingsSampledOut.Load(),
		"observer_panics":        a.Metrics.ObserverPanics.Load(),
		"events_emitted":         a.EventsEmitted.Load(),
		"alerts_sent":            alertsSent,
		"last_delivery_ok":       deliveryOK,
		"last_delivery_err":      deliveryErr,
		"deliveries_dropped":     deliveriesDropped,
		"saturation_evictions":   saturationEvictions,
		"tracked_fingerprints":   trackedFPs,
		"baselines":              fams,
	}
	if a.watch != nil {
		out["spec_watch"] = a.watch.Health()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
