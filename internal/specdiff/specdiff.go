// Package specdiff is pikopod's DECLARED-drift engine: a typed diff over two
// IRs, keyed on method+CanonicalPath, with severity derived by law.
package specdiff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// Level is a finding's severity: ERR breaks existing consumers, WARN can
// break some or was properly sunset, INFO is additive/compatible.
type Level string

const (
	Info Level = "INFO"
	Warn Level = "WARN"
	Err  Level = "ERR"
)

// Rank orders levels for --fail-on floors (higher = more severe).
func (l Level) Rank() int {
	switch l {
	case Err:
		return 2
	case Warn:
		return 1
	default:
		return 0
	}
}

// Direction is which side of the exchange the changed element sits on.
type Direction string

const (
	Request  Direction = "request"
	Response Direction = "response"
)

// Effect is the change's type-theoretic direction from the consumer's view:
// the contract guarantees less, more, a tolerated subset, or neither.
type Effect string

const (
	Narrows      Effect = "narrows"
	Widens       Effect = "widens"
	Shrinks      Effect = "shrinks"
	Incomparable Effect = "incomparable"
)

// Guards downgrade a derived level when the break is softened by context.
type Guards struct {
	// The old spec already marked it deprecated — a sunset was honored. ERR → WARN.
	DeprecatedHonored bool
	// The narrowing touches something never guaranteed to consumers. ERR → WARN.
	OptionalOnly bool
	// A widening consumers tolerate by dominant convention (an added response
	// property, unlike an added enum value). WARN → INFO.
	Tolerated bool
	// Either side rests on INFERRED/LLM_EXTRACTED provenance — a heuristic
	// must never page as a certain break. ERR → WARN.
	Uncertain bool
}

// DeriveLevel is THE severity law. Every check calls it; none picks a level.
func DeriveLevel(effect Effect, dir Direction, g Guards) Level {
	var l Level
	switch {
	case effect == Incomparable:
		l = Err
	case effect == Narrows:
		l = Err // request: accepts less; response: withdrawn guarantee
	case effect == Widens && dir == Request:
		l = Info // server accepts more — existing clients unaffected
	case effect == Widens: // × response
		l = Warn // new output variety — strict consumers can break
	default: // Shrinks × response
		l = Info
	}
	if l == Err && (g.DeprecatedHonored || g.OptionalOnly || g.Uncertain) {
		l = Warn
	}
	if l == Warn && g.Tolerated {
		l = Info
	}
	return l
}

// Finding is one declared-drift divergence. Args are the check-specific
// identity components that feed the fingerprint.
type Finding struct {
	ID       string   `json:"id"`
	Level    Level    `json:"level"`
	Method   string   `json:"method"`
	Template string   `json:"template"`
	Args     []string `json:"args,omitempty"`
	Detail   string   `json:"detail"`
}

// Fingerprint hashes the CANONICAL template, so a path-parameter rename does
// not re-fire acknowledged findings. NUL-joined so args cannot shift boundaries.
func (f Finding) Fingerprint() string {
	parts := append([]string{f.ID, f.Method, ir.CanonicalPathTemplate(f.Template)}, f.Args...)
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "fp_" + hex.EncodeToString(h[:])[:12]
}

// Diff compares the pinned oldDef against the incoming newDef, returning
// findings sorted deterministically by template, method, id, args.
func Diff(oldDef, newDef *ir.ApiDefinition) []Finding {
	d := &differ{
		oldSchemas: namedSchemas(oldDef),
		newSchemas: namedSchemas(newDef),
	}

	oldEps := indexEndpoints(oldDef)
	newEps := indexEndpoints(newDef)

	for k, oe := range oldEps {
		ne, ok := newEps[k]
		if !ok {
			d.emit(Finding{
				ID: "endpoint-removed", Method: oe.Method.Value, Template: oe.PathTemplate.Value,
				Level:  DeriveLevel(Narrows, Request, Guards{DeprecatedHonored: oe.Deprecated.Value, Uncertain: oe.Method.IsUncertain()}),
				Detail: "endpoint removed from the spec",
			})
			continue
		}
		d.diffEndpoint(oe, ne)
	}
	for k, ne := range newEps {
		if _, ok := oldEps[k]; !ok {
			d.emit(Finding{
				ID: "endpoint-added", Method: ne.Method.Value, Template: ne.PathTemplate.Value,
				Level:  DeriveLevel(Widens, Request, Guards{}),
				Detail: "new endpoint",
			})
		}
	}

	d.diffAuthSchemes(oldDef, newDef)

	sort.Slice(d.out, func(i, j int) bool {
		a, b := d.out[i], d.out[j]
		if a.Template != b.Template {
			return a.Template < b.Template
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return strings.Join(a.Args, "\x00") < strings.Join(b.Args, "\x00")
	})
	return d.out
}

// Breaking reports whether any finding is at or above the level floor.
func Breaking(findings []Finding, floor Level) bool {
	for _, f := range findings {
		if f.Level.Rank() >= floor.Rank() {
			return true
		}
	}
	return false
}

type differ struct {
	out        []Finding
	oldSchemas map[string]*ir.IrSchemaNode
	newSchemas map[string]*ir.IrSchemaNode
}

func (d *differ) emit(f Finding) { d.out = append(d.out, f) }

func namedSchemas(def *ir.ApiDefinition) map[string]*ir.IrSchemaNode {
	m := make(map[string]*ir.IrSchemaNode, len(def.Schemas))
	for i := range def.Schemas {
		m[def.Schemas[i].ID] = &def.Schemas[i].Schema
	}
	return m
}

func indexEndpoints(def *ir.ApiDefinition) map[string]*ir.Endpoint {
	m := make(map[string]*ir.Endpoint, len(def.Endpoints))
	for i := range def.Endpoints {
		ep := &def.Endpoints[i]
		m[ep.Method.Value+" "+ep.CanonicalPath] = ep
	}
	return m
}

func (d *differ) diffEndpoint(oe, ne *ir.Endpoint) {
	method, template := ne.Method.Value, ne.PathTemplate.Value
	mk := func(id string, level Level, detail string, args ...string) Finding {
		return Finding{ID: id, Level: level, Method: method, Template: template, Args: args, Detail: detail}
	}

	if !oe.Deprecated.Value && ne.Deprecated.Value {
		d.emit(mk("endpoint-deprecated", Info, "endpoint marked deprecated — plan the migration"))
	}

	d.diffParams(oe, ne, mk)
	d.diffRequestBody(oe.RequestBody, ne.RequestBody, mk)
	d.diffResponses(oe, ne, mk)
	d.diffEndpointSecurity(oe, ne, mk)
}

// paramCounterpart matches path params positionally (rename-tolerant — the
// canonical path already proved the shapes align), everything else by name.
func paramCounterpart(p *ir.Parameter, oldPathOrder []string, side []ir.Parameter, sidePathOrder []string) *ir.Parameter {
	if p.Location == "path" {
		pos := -1
		for i, n := range oldPathOrder {
			if n == p.Name {
				pos = i
				break
			}
		}
		if pos >= 0 && pos < len(sidePathOrder) {
			want := sidePathOrder[pos]
			for i := range side {
				if side[i].Location == "path" && side[i].Name == want {
					return &side[i]
				}
			}
		}
		return nil
	}
	for i := range side {
		if side[i].Location == p.Location && side[i].Name == p.Name {
			return &side[i]
		}
	}
	return nil
}

// pathParamOrder extracts {param} names from a template in order.
func pathParamOrder(template string) []string {
	var out []string
	for _, seg := range strings.Split(template, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"))
		}
	}
	return out
}

func (d *differ) diffParams(oe, ne *ir.Endpoint, mk func(string, Level, string, ...string) Finding) {
	oldOrder := pathParamOrder(oe.PathTemplate.Value)
	newOrder := pathParamOrder(ne.PathTemplate.Value)

	for i := range oe.Parameters {
		op := &oe.Parameters[i]
		np := paramCounterpart(op, oldOrder, ne.Parameters, newOrder)
		label := op.Location + " param `" + op.Name + "`"
		if np == nil {
			// Removed request param: clients still sending it are usually
			// ignored by servers — a narrowing softened by convention.
			d.emit(mk("param-removed",
				DeriveLevel(Narrows, Request, Guards{OptionalOnly: true, DeprecatedHonored: op.Deprecated.Value}),
				label+" removed", op.Location, op.Name))
			continue
		}
		if op.Location == "path" && op.Name != np.Name {
			d.emit(mk("param-renamed", Info,
				"path param `"+op.Name+"` renamed to `"+np.Name+"` (same position — compared, not re-added)",
				op.Name, np.Name))
		}
		if !op.Required.Value && np.Required.Value {
			d.emit(mk("param-became-required",
				DeriveLevel(Narrows, Request, Guards{Uncertain: np.Required.IsUncertain()}),
				label+" became required — clients omitting it will be rejected", op.Location, op.Name))
		}
		if op.Required.Value && !np.Required.Value {
			d.emit(mk("param-became-optional", DeriveLevel(Widens, Request, Guards{}),
				label+" became optional", op.Location, op.Name))
		}
		d.diffSchemaNodes(&op.Schema, &np.Schema, Request, label, "param", mk)
	}
	for i := range ne.Parameters {
		np := &ne.Parameters[i]
		if paramCounterpart(np, newOrder, oe.Parameters, oldOrder) != nil {
			continue
		}
		label := np.Location + " param `" + np.Name + "`"
		if np.Required.Value {
			d.emit(mk("param-added-required",
				DeriveLevel(Narrows, Request, Guards{Uncertain: np.Required.IsUncertain()}),
				"new REQUIRED "+label+" — every existing client omits it", np.Location, np.Name))
		} else {
			d.emit(mk("param-added-optional", DeriveLevel(Widens, Request, Guards{}),
				"new optional "+label, np.Location, np.Name))
		}
	}
}

func (d *differ) diffRequestBody(ob, nb *ir.RequestBody, mk func(string, Level, string, ...string) Finding) {
	switch {
	case ob == nil && nb == nil:
		return
	case ob == nil:
		if nb.Required.Value {
			d.emit(mk("request-body-added-required",
				DeriveLevel(Narrows, Request, Guards{Uncertain: nb.Required.IsUncertain()}),
				"endpoint now REQUIRES a request body"))
		} else {
			d.emit(mk("request-body-added-optional", DeriveLevel(Widens, Request, Guards{}),
				"endpoint now accepts an optional request body"))
		}
		return
	case nb == nil:
		d.emit(mk("request-body-removed",
			DeriveLevel(Narrows, Request, Guards{OptionalOnly: !ob.Required.Value}),
			"request body removed from the spec"))
		return
	}
	if !ob.Required.Value && nb.Required.Value {
		d.emit(mk("request-body-became-required",
			DeriveLevel(Narrows, Request, Guards{Uncertain: nb.Required.IsUncertain()}),
			"request body became required"))
	}
	if ob.Required.Value && !nb.Required.Value {
		d.emit(mk("request-body-became-optional", DeriveLevel(Widens, Request, Guards{}),
			"request body became optional"))
	}
	d.diffContent(ob.Content, nb.Content, Request, "request body", mk)
}

func (d *differ) diffResponses(oe, ne *ir.Endpoint, mk func(string, Level, string, ...string) Finding) {
	oldByStatus := map[string]*ir.ResponseDef{}
	for i := range oe.Responses {
		oldByStatus[oe.Responses[i].StatusCode] = &oe.Responses[i]
	}
	newByStatus := map[string]*ir.ResponseDef{}
	for i := range ne.Responses {
		newByStatus[ne.Responses[i].StatusCode] = &ne.Responses[i]
	}

	for status, or := range oldByStatus {
		nr, ok := newByStatus[status]
		if !ok {
			if isSuccessStatus(status) {
				// The documented happy path disappeared — consumers read it.
				d.emit(mk("response-status-removed",
					DeriveLevel(Narrows, Response, Guards{DeprecatedHonored: oe.Deprecated.Value}),
					"response status "+status+" removed — the documented success shape is gone", status))
			} else {
				// An error status that stops occurring is output the client
				// tolerates by construction.
				d.emit(mk("response-status-removed", DeriveLevel(Shrinks, Response, Guards{}),
					"response status "+status+" removed", status))
			}
			continue
		}
		d.diffContent(or.Content, nr.Content, Response, "response "+status, mk, status)
	}
	for status := range newByStatus {
		if _, ok := oldByStatus[status]; !ok {
			d.emit(mk("response-status-added", DeriveLevel(Widens, Response, Guards{}),
				"endpoint documents a new response status "+status+" — handle it", status))
		}
	}
}

func (d *differ) diffContent(oldC, newC []ir.MediaType, dir Direction, where string, mk func(string, Level, string, ...string) Finding, argPrefix ...string) {
	oldBy := map[string]*ir.MediaType{}
	for i := range oldC {
		oldBy[oldC[i].MediaType] = &oldC[i]
	}
	newBy := map[string]*ir.MediaType{}
	for i := range newC {
		newBy[newC[i].MediaType] = &newC[i]
	}
	for mt, om := range oldBy {
		nm, ok := newBy[mt]
		if !ok {
			id, detail := "response-media-type-removed", where+" no longer documents "+mt
			if dir == Request {
				id, detail = "request-media-type-removed", where+" no longer accepts "+mt
			}
			d.emit(mk(id, DeriveLevel(Narrows, dir, Guards{}), detail, append(argPrefix, mt)...))
			continue
		}
		d.diffSchemaNodes(&om.Schema, &nm.Schema, dir, where+" ("+mt+")", strings.Join(append(argPrefix, mt), " "), mk)
	}
	for mt := range newBy {
		if _, ok := oldBy[mt]; !ok {
			id := "response-media-type-added"
			guards := Guards{Tolerated: true} // an extra representation breaks nobody
			if dir == Request {
				id = "request-media-type-added"
			}
			d.emit(mk(id, DeriveLevel(Widens, dir, guards), where+" adds "+mt, append(argPrefix, mt)...))
		}
	}
}

func (d *differ) diffEndpointSecurity(oe, ne *ir.Endpoint, mk func(string, Level, string, ...string) Finding) {
	oldIDs := map[string]bool{}
	for _, s := range oe.Security {
		oldIDs[s.SchemeID] = true
	}
	newIDs := map[string]bool{}
	for _, s := range ne.Security {
		newIDs[s.SchemeID] = true
	}
	if len(oldIDs) == 0 && len(newIDs) > 0 {
		d.emit(mk("endpoint-security-added", DeriveLevel(Narrows, Request, Guards{}),
			"endpoint now requires authentication — unauthenticated clients will be rejected"))
	}
	for id := range oldIDs {
		if !newIDs[id] && len(newIDs) > 0 {
			// One accepted auth option withdrawn (others remain).
			d.emit(mk("endpoint-security-scheme-removed",
				DeriveLevel(Narrows, Request, Guards{}),
				"auth scheme `"+id+"` no longer accepted on this endpoint", id))
		}
	}
	if len(oldIDs) > 0 && len(newIDs) == 0 {
		d.emit(mk("endpoint-security-removed", DeriveLevel(Widens, Request, Guards{}),
			"endpoint no longer requires authentication"))
	}
}

func (d *differ) diffAuthSchemes(oldDef, newDef *ir.ApiDefinition) {
	oldBy := map[string]*ir.AuthScheme{}
	for i := range oldDef.AuthSchemes {
		oldBy[oldDef.AuthSchemes[i].Name] = &oldDef.AuthSchemes[i]
	}
	newBy := map[string]*ir.AuthScheme{}
	for i := range newDef.AuthSchemes {
		newBy[newDef.AuthSchemes[i].Name] = &newDef.AuthSchemes[i]
	}
	mk := func(id string, level Level, detail string, args ...string) Finding {
		return Finding{ID: id, Level: level, Method: "*", Template: "(auth)", Args: args, Detail: detail}
	}
	for name, os := range oldBy {
		ns, ok := newBy[name]
		if !ok {
			d.emit(mk("auth-scheme-removed", DeriveLevel(Narrows, Request, Guards{}),
				"auth scheme `"+name+"` removed — clients authenticating with it break", name))
			continue
		}
		if os.Kind.Value != ns.Kind.Value ||
			strval(os.Scheme) != strval(ns.Scheme) ||
			strval(os.ParameterName) != strval(ns.ParameterName) ||
			strval(os.Location) != strval(ns.Location) {
			d.emit(mk("auth-scheme-changed", DeriveLevel(Incomparable, Request, Guards{}),
				fmt.Sprintf("auth scheme `%s` changed: %s → %s — existing credentials/headers stop working",
					name, describeScheme(os), describeScheme(ns)), name))
		}
	}
	for name := range newBy {
		if _, ok := oldBy[name]; !ok {
			d.emit(mk("auth-scheme-added", DeriveLevel(Widens, Request, Guards{}),
				"new auth scheme `"+name+"`", name))
		}
	}
}

func describeScheme(s *ir.AuthScheme) string {
	parts := []string{s.Kind.Value}
	if v := strval(s.Scheme); v != "" {
		parts = append(parts, v)
	}
	if v := strval(s.ParameterName); v != "" {
		parts = append(parts, v)
		if l := strval(s.Location); l != "" {
			parts = append(parts, "in "+l)
		}
	}
	return strings.Join(parts, " ")
}

func strval(p *ir.Prov[string]) string {
	if p == nil {
		return ""
	}
	return p.Value
}

func isSuccessStatus(status string) bool {
	return strings.HasPrefix(status, "2") || status == "default"
}
