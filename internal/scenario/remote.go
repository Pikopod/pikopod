// Remote targets: run a scenario's REQUEST steps against a REAL endpoint and
// hold responses to the same assertions. Other steps are refused up front.
package scenario

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/sandbox"
)

type Target interface {
	http.Handler
	VirtualClockMs() int64
	SetVirtualClockMs(ms int64)
	AuthHeader() (name, value string, ok bool)
	ArmFault(sandbox.FaultRule)
	ClearFaults(method, path string) int
	SeedResource(typ, key string, attributes json.RawMessage) error
	LookupResource(typ, key string) (attributes json.RawMessage, found bool, err error)
	DeliveriesDueBy(eventFilter string, horizonMs int64) ([]sandbox.WebhookDelivery, int64)
	JournalCount(method, template string) (count int, evicted bool)
	JournalLast(method, template string) (entry *sandbox.JournalEntry, found, evicted bool)
	JournalEntries(limit int) (entries []sandbox.JournalEntry, evicted int64)
}

// remoteAllowedSteps is the step subset that is meaningful against a real
// endpoint.
var remoteAllowedSteps = map[string]bool{"REQUEST": true, "NOTE": true, "SNAPSHOT": true}

// RefuseUnsupportedSteps rejects a definition containing steps a remote
// target cannot honor — loudly, before anything runs.
func RefuseUnsupportedSteps(def *ScenarioDefinition) error {
	for i := range def.Steps {
		if !remoteAllowedSteps[def.Steps[i].Type] {
			return errfmt.New(
				"step "+strconv.Itoa(i)+" ("+def.Steps[i].Type+") cannot run against a remote target",
				"a real endpoint has no fault arming, seeded state, virtual clock, webhook outbox, or request journal to consult",
				"run this scenario against the local sandbox, or trim it to REQUEST/NOTE/SNAPSHOT steps for the remote check",
				"scenarios/README.md")
		}
	}
	return nil
}

// RemoteTarget drives scenario requests at a real base URL.
type RemoteTarget struct {
	BaseURL string
	// Headers are sent on every request (e.g. Authorization) — values are
	// never logged by the runner.
	Headers map[string]string
	Client  *http.Client

	virtualClockMs int64
}

// NewRemoteTarget validates the base URL and applies defaults.
func NewRemoteTarget(baseURL string, headers map[string]string) (*RemoteTarget, error) {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, errfmt.New("target must be an http(s) URL", strconv.Quote(baseURL)+" is not", "e.g. --target https://api.sandbox.provider.com", "")
	}
	return &RemoteTarget{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Headers: headers,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (t *RemoteTarget) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	req, err := http.NewRequest(r.Method, t.BaseURL+r.URL.RequestURI(), strings.NewReader(string(body)))
	if err != nil {
		writeRemoteError(w, "building the request failed: "+err.Error())
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	started := time.Now()
	resp, err := t.Client.Do(req)
	if err != nil {
		// Transport failure is a real result at a real endpoint — surface
		// it as a 599 the assertions can see rather than aborting the run.
		writeRemoteError(w, "request to "+t.BaseURL+" failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	elapsed := time.Since(started).Milliseconds()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set(sandbox.FaultDelayHeader, strconv.FormatInt(elapsed, 10))
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func writeRemoteError(w http.ResponseWriter, message string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(599)
	raw, _ := json.Marshal(map[string]string{"pikopodRemoteError": message})
	w.Write(raw)
}

// Target methods a real endpoint cannot honor. RefuseUnsupportedSteps keeps
// these unreachable in practice; they answer honestly if reached.

func (t *RemoteTarget) VirtualClockMs() int64      { return t.virtualClockMs }
func (t *RemoteTarget) SetVirtualClockMs(ms int64) { t.virtualClockMs = ms }
func (t *RemoteTarget) AuthHeader() (string, string, bool) {
	return "", "", false // auth comes from --target-header, sent on every request
}
func (t *RemoteTarget) ArmFault(sandbox.FaultRule)          {}
func (t *RemoteTarget) ClearFaults(method, path string) int { return 0 }
func (t *RemoteTarget) SeedResource(typ, key string, attributes json.RawMessage) error {
	return errfmt.New("cannot seed a remote target", "a real endpoint's state is its own", "seed via real API calls in REQUEST steps instead", "")
}
func (t *RemoteTarget) LookupResource(typ, key string) (json.RawMessage, bool, error) {
	return nil, false, errfmt.New("cannot read a remote target's state store", "a real endpoint exposes state only through its API", "assert on responses instead", "")
}
func (t *RemoteTarget) DeliveriesDueBy(string, int64) ([]sandbox.WebhookDelivery, int64) {
	return nil, 0
}
func (t *RemoteTarget) JournalCount(string, string) (int, bool) { return 0, false }

// A real endpoint keeps no journal; VERIFY_SEQUENCE is refused before a run
// reaches here (remoteAllowedSteps).
func (t *RemoteTarget) JournalEntries(int) ([]sandbox.JournalEntry, int64) { return nil, 0 }

func (t *RemoteTarget) JournalLast(string, string) (*sandbox.JournalEntry, bool, bool) {
	return nil, false, false
}
