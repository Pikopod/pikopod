package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
)

func (e *Engine) SetVirtualClockMs(ms int64) {
	if ms < SandboxBaseEpochMs {
		ms = SandboxBaseEpochMs
	}
	e.virtualClockMs = ms
}

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
				continue
			}
			return strings.ToLower(s.ParameterName.Value), e.credential, true
		}

		kind := strings.ToLower(s.Scheme.Value)
		if kind == "bearer" {
			return "authorization", "Bearer " + e.credential, true
		}

		return "authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+e.credential)), true
	}
	return "", "", false
}

func (e *Engine) SeedResource(typ, key string, attributes json.RawMessage) error {
	if key == "" {

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
