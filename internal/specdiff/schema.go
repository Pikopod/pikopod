// Schema-node diffing. Every level comes out of DeriveLevel — the walker
// only classifies the effect and states the guards.
package specdiff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

// maxSchemaDepth bounds the walk: refs can cycle and provider specs nest
// deep; past the cap the subtree is simply not compared (never a crash).
const maxSchemaDepth = 32

// deref follows Ref chains into the named-schema table (bounded).
func deref(n *ir.IrSchemaNode, table map[string]*ir.IrSchemaNode) *ir.IrSchemaNode {
	for hops := 0; n != nil && n.Ref != nil && hops < maxSchemaDepth; hops++ {
		next, ok := table[*n.Ref]
		if !ok {
			return n
		}
		n = next
	}
	return n
}

// diffSchemaNodes compares one old/new schema pair in a direction, emitting
// findings prefixed with `where` (human) and keyed on argKey (identity).
func (d *differ) diffSchemaNodes(oldN, newN *ir.IrSchemaNode, dir Direction, where, argKey string, mk func(string, Level, string, ...string) Finding) {
	d.walkSchema(oldN, newN, dir, where, argKey, "", 0, mk)
}

func (d *differ) walkSchema(oldN, newN *ir.IrSchemaNode, dir Direction, where, argKey, path string, depth int, mk func(string, Level, string, ...string) Finding) {
	if depth > maxSchemaDepth || oldN == nil || newN == nil {
		return
	}
	oldN = deref(oldN, d.oldSchemas)
	newN = deref(newN, d.newSchemas)
	if oldN == nil || newN == nil {
		return
	}
	// allOf is an intersection — a merge — so flatten and diff member-wise.
	oldN = ir.FlattenAllOf(oldN, d.oldSchemas)
	newN = ir.FlattenAllOf(newN, d.newSchemas)
	// oneOf/anyOf members are NOT diffed pairwise: identity is positional and
	// a reorder would flood false findings. Only presence/kind/count are certain.
	if (oldN.Composition == nil) != (newN.Composition == nil) {
		d.emit(mk("schema-restructured", DeriveLevel(Incomparable, dir, Guards{Uncertain: oldN.Type.IsUncertain() || newN.Type.IsUncertain()}),
			where+at(path)+" was restructured (composition added/removed)", argKey, path))
		return
	}
	if oldN.Composition != nil {
		oc, nc := oldN.Composition, newN.Composition
		if oc.Kind != nc.Kind {
			d.emit(mk("schema-restructured", DeriveLevel(Incomparable, dir, Guards{}),
				fmt.Sprintf("%s%s changed composition kind %s → %s", where, at(path), oc.Kind, nc.Kind), argKey, path))
			return
		}
		switch {
		case len(nc.Members) < len(oc.Members):
			eff := Narrows
			if dir == Response {
				eff = Shrinks
			}
			d.emit(mk(dirCheckID(dir, "variant-removed"), DeriveLevel(eff, dir, Guards{}),
				fmt.Sprintf("%s%s %s variants %d → %d — a documented shape was removed", where, at(path), oc.Kind, len(oc.Members), len(nc.Members)), argKey, path))
		case len(nc.Members) > len(oc.Members):
			d.emit(mk(dirCheckID(dir, "variant-added"), DeriveLevel(Widens, dir, Guards{Uncertain: dir == Response}),
				fmt.Sprintf("%s%s %s variants %d → %d — consumers switching on shape may not handle the new one", where, at(path), oc.Kind, len(oc.Members), len(nc.Members)), argKey, path))
		}
		return
	}

	uncertain := oldN.Type.IsUncertain() || newN.Type.IsUncertain()
	label := where + at(path)

	if eff, changed := typeEffect(oldN.Type.Value, newN.Type.Value, dir); changed {
		d.emit(mk(typeCheckID(dir), DeriveLevel(eff, dir, Guards{Uncertain: uncertain}),
			fmt.Sprintf("%s type %s → %s", label, oldN.Type.Value, newN.Type.Value),
			argKey, path, oldN.Type.Value, newN.Type.Value))
	}

	// Nullability. Request: forbidding null narrows what's accepted.
	// Response: allowing null means consumers that never handled null break.
	if !oldN.Nullable.Value && newN.Nullable.Value {
		if dir == Request {
			d.emit(mk("request-nullable-added", DeriveLevel(Widens, Request, Guards{}),
				label+" now accepts null", argKey, path))
		} else {
			d.emit(mk("response-nullable-added", DeriveLevel(Widens, Response, Guards{Uncertain: uncertain}),
				label+" may now be null — parsers that never handled null will break", argKey, path))
		}
	}
	if oldN.Nullable.Value && !newN.Nullable.Value {
		if dir == Request {
			d.emit(mk("request-nullable-removed", DeriveLevel(Narrows, Request, Guards{Uncertain: uncertain}),
				label+" no longer accepts null", argKey, path))
		} else {
			d.emit(mk("response-nullable-removed", DeriveLevel(Shrinks, Response, Guards{}),
				label+" is no longer nullable", argKey, path))
		}
	}

	d.diffEnums(oldN, newN, dir, label, argKey, path, uncertain, mk)
	d.diffProperties(oldN, newN, dir, where, argKey, path, depth, mk)

	if oldN.Items != nil && newN.Items != nil {
		d.walkSchema(oldN.Items, newN.Items, dir, where, argKey, path+"[]", depth+1, mk)
	}
}

func dirCheckID(dir Direction, suffix string) string {
	if dir == Request {
		return "request-" + suffix
	}
	return "response-" + suffix
}

func typeCheckID(dir Direction) string {
	if dir == Request {
		return "request-type-changed"
	}
	return "response-type-changed"
}

// typeEffect classifies a scalar-type transition through the subtype lattice
// (integer ⊂ number). "unknown" on either side abstains — never guess.
func typeEffect(from, to string, dir Direction) (Effect, bool) {
	if from == to || from == "unknown" || to == "unknown" || from == "" || to == "" {
		return "", false
	}
	widerNew := from == "integer" && to == "number"    // new type accepts/produces MORE
	narrowerNew := from == "number" && to == "integer" // new type accepts/produces LESS
	switch {
	case dir == Request && widerNew:
		return Widens, true // server accepts more — old clients fine
	case dir == Request && narrowerNew:
		return Narrows, true // old clients sending floats now rejected
	case dir == Response && widerNew:
		return Widens, true // consumers typed int may now receive floats
	case dir == Response && narrowerNew:
		return Shrinks, true // outputs are a subset of what consumers parse
	default:
		return Incomparable, true
	}
}

func (d *differ) diffEnums(oldN, newN *ir.IrSchemaNode, dir Direction, label, argKey, path string, uncertain bool, mk func(string, Level, string, ...string) Finding) {
	oldVals := enumSet(oldN.EnumValues)
	newVals := enumSet(newN.EnumValues)
	switch {
	case oldVals == nil && newVals == nil:
		return
	case oldVals == nil: // enum introduced where the value space was open
		if dir == Request {
			d.emit(mk("request-enum-closed", DeriveLevel(Narrows, Request, Guards{Uncertain: uncertain}),
				label+" is now restricted to an enum ["+joinSorted(newVals)+"]", argKey, path))
		} else {
			d.emit(mk("response-enum-closed", DeriveLevel(Shrinks, Response, Guards{}),
				label+" is now documented as an enum ["+joinSorted(newVals)+"]", argKey, path))
		}
		return
	case newVals == nil: // enum removed — value space opened
		if dir == Request {
			d.emit(mk("request-enum-opened", DeriveLevel(Widens, Request, Guards{}),
				label+" is no longer restricted to an enum", argKey, path))
		} else {
			d.emit(mk("response-enum-opened", DeriveLevel(Widens, Response, Guards{Uncertain: uncertain}),
				label+" is no longer a closed enum — any value may now appear", argKey, path))
		}
		return
	}
	var removed, added []string
	for v := range oldVals {
		if !newVals[v] {
			removed = append(removed, v)
		}
	}
	for v := range newVals {
		if !oldVals[v] {
			added = append(added, v)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	for _, v := range removed {
		if dir == Request {
			d.emit(mk("request-enum-value-removed", DeriveLevel(Narrows, Request, Guards{Uncertain: uncertain}),
				label+": value "+quote(v)+" no longer accepted", argKey, path, v))
		} else {
			d.emit(mk("response-enum-value-removed", DeriveLevel(Shrinks, Response, Guards{}),
				label+": value "+quote(v)+" removed from the documented set", argKey, path, v))
		}
	}
	for _, v := range added {
		if dir == Request {
			d.emit(mk("request-enum-value-added", DeriveLevel(Widens, Request, Guards{}),
				label+": accepts new value "+quote(v), argKey, path, v))
		} else {
			// The exhaustive-switch hazard: NOT Tolerated by convention.
			d.emit(mk("response-enum-value-added", DeriveLevel(Widens, Response, Guards{Uncertain: uncertain}),
				label+": may now return "+quote(v)+" — exhaustive switches break", argKey, path, v))
		}
	}
}

func (d *differ) diffProperties(oldN, newN *ir.IrSchemaNode, dir Direction, where, argKey, path string, depth int, mk func(string, Level, string, ...string) Finding) {
	if len(oldN.Properties) == 0 && len(newN.Properties) == 0 {
		return
	}
	oldBy := map[string]*ir.PropertySchema{}
	for i := range oldN.Properties {
		oldBy[oldN.Properties[i].Name] = &oldN.Properties[i]
	}
	newBy := map[string]*ir.PropertySchema{}
	for i := range newN.Properties {
		newBy[newN.Properties[i].Name] = &newN.Properties[i]
	}

	names := make([]string, 0, len(oldBy))
	for n := range oldBy {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		op := oldBy[name]
		ppath := joinPath(path, name)
		label := where + at(ppath)
		np, ok := newBy[name]
		if !ok {
			if dir == Request {
				// Clients still sending it are usually ignored by servers.
				d.emit(mk("request-property-removed",
					DeriveLevel(Narrows, Request, Guards{OptionalOnly: true, Uncertain: op.Required.IsUncertain()}),
					label+" removed from the request schema", argKey, ppath))
			} else if op.Required.Value {
				d.emit(mk("response-required-property-removed",
					DeriveLevel(Narrows, Response, Guards{Uncertain: op.Required.IsUncertain()}),
					label+" (guaranteed) removed — consumers reading it break", argKey, ppath))
			} else {
				d.emit(mk("response-property-removed",
					DeriveLevel(Narrows, Response, Guards{OptionalOnly: true}),
					label+" (optional) removed", argKey, ppath))
			}
			continue
		}
		if !op.Required.Value && np.Required.Value {
			if dir == Request {
				d.emit(mk("request-property-became-required",
					DeriveLevel(Narrows, Request, Guards{Uncertain: np.Required.IsUncertain()}),
					label+" became required — clients omitting it will be rejected", argKey, ppath))
			} else {
				d.emit(mk("response-property-became-required", DeriveLevel(Shrinks, Response, Guards{}),
					label+" is now guaranteed present", argKey, ppath))
			}
		}
		if op.Required.Value && !np.Required.Value {
			if dir == Request {
				d.emit(mk("request-property-became-optional", DeriveLevel(Widens, Request, Guards{}),
					label+" became optional", argKey, ppath))
			} else {
				d.emit(mk("response-property-became-optional",
					DeriveLevel(Widens, Response, Guards{Uncertain: np.Required.IsUncertain()}),
					label+" is no longer guaranteed — consumers assuming presence break", argKey, ppath))
			}
		}
		d.walkSchema(&op.Schema, &np.Schema, dir, where, argKey, ppath, depth+1, mk)
	}

	addNames := make([]string, 0, len(newBy))
	for n := range newBy {
		if _, ok := oldBy[n]; !ok {
			addNames = append(addNames, n)
		}
	}
	sort.Strings(addNames)
	for _, name := range addNames {
		np := newBy[name]
		ppath := joinPath(path, name)
		label := where + at(ppath)
		if dir == Request {
			if np.Required.Value {
				d.emit(mk("request-property-added-required",
					DeriveLevel(Narrows, Request, Guards{Uncertain: np.Required.IsUncertain()}),
					"new REQUIRED "+label+" — every existing client omits it", argKey, ppath))
			} else {
				d.emit(mk("request-property-added-optional", DeriveLevel(Widens, Request, Guards{}),
					"new optional "+label, argKey, ppath))
			}
		} else {
			// Additive response field: consumers ignore unknown fields by
			// dominant convention (the Tolerated guard) — INFO, not WARN.
			d.emit(mk("response-property-added", DeriveLevel(Widens, Response, Guards{Tolerated: true}),
				"new "+label+" in responses", argKey, ppath))
		}
	}
}

func enumSet(p *ir.Prov[[]any]) map[string]bool {
	if p == nil || p.Value == nil {
		return nil
	}
	m := make(map[string]bool, len(p.Value))
	for _, v := range p.Value {
		m[fmt.Sprint(v)] = true
	}
	return m
}

func joinSorted(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) > 8 {
		out = append(out[:8], "…")
	}
	return strings.Join(out, ",")
}

func quote(v string) string { return "`" + v + "`" }

func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}

func at(path string) string {
	if path == "" {
		return ""
	}
	return " field `" + path + "`"
}
