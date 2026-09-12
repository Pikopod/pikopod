// The scenario runner's view of the engine. Runs drive it from ONE goroutine, so
// the clock setter is a plain field write — unsafe against a mid-request change.
package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
)

// SetVirtualClockMs advances the virtual clock: WAIT steps move time forward so
// time-dependent synthesis reflects the wait; never a real sleep.
func (e *Engine) SetVirtualClockMs(ms int64) {
	if ms < SandboxBaseEpochMs {
		ms = SandboxBaseEpochMs // never below the base epoch that keeps synthesized dates sane
	}
	e.virtualClockMs = ms
}

// AuthHeader mirrors what a customer would send, from the first enforceable
// scheme. ok=false means no header-carried scheme: the runner sends it bare.
func (e *Engine) AuthHeader() (name, value string, ok bool) {
	for i := range e.def.AuthSchemes {
		s := &e.def.AuthSchemes[i]
		if !isEnforceableScheme(s) {
			continue
		}
		if s.Kind.Value == "apiKey" {
			loc := ""
			if s.Location != nil {
				loc = s.Location.Value
			}
			if loc != "header" {
				continue // query/cookie apiKey: not header-injectable
			}
			return strings.ToLower(s.ParameterName.Value), e.credential, true
		}
		// http bearer / basic
		kind := strings.ToLower(s.Scheme.Value)
		if kind == "bearer" {
			return "authorization", "Bearer " + e.credential, true
		}
		// basic: the enforcer takes the password half after the first colon.
		return "authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+e.credential)), true
	}
	return "", "", false
}

// SeedResource inserts a resource row directly (SEED_STATE), bypassing the
// request pipeline — but never the quotas.
func (e *Engine) SeedResource(typ, key string, attributes json.RawMessage) error {
	if key == "" {
		// Keyset pagination treats "" as the first-page sentinel: an
		// empty-keyed row would count against quotas but never list.
		return errfmt.New("resourceKey must not be empty", "an empty key is unreachable through list endpoints", "omit resourceKey to autogenerate one", "scenarios/README.md")
	}
	size := attributeSize(attributes)
	totals, err := e.store.Totals(e.id)
	if err != nil {
		return err
	}
	if resp := e.quotaGuard(size, totals.Count+1, totals.Bytes+size); resp != nil {
		return errfmt.New("sandbox quota exceeded while seeding", "the seed would push the sandbox past its resource/storage quota", "reset the sandbox or seed fewer/smaller resources", "docs/config-reference.md#quotas")
	}
	_, err = e.store.Insert(e.id, typ, key, attributes, nil, e.virtualClockMs)
	return err
}

// LookupResource reads one resource's attributes (ASSERT_STATE).
// found=false when no row exists; err only on a store fault.
func (e *Engine) LookupResource(typ, key string) (attributes json.RawMessage, found bool, err error) {
	res, err := e.store.GetOne(e.id, typ, key)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return res.Attributes, true, nil
}
