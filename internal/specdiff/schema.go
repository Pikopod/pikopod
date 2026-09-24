package specdiff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

const maxSchemaDepth = 32

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

func (d *differ) diffSchemaNodes(oldN, newN *ir.IrSchemaNode, dir Direction, where, argKey string, mk mkFn) {
	d.walkSchema(oldN, newN, dir, where, argKey, "", 0, mk)
}

func (d *differ) walkSchema(oldN, newN *ir.IrSchemaNode, dir Direction, where, argKey, path string, depth int, mk mkFn) {
	if depth > maxSchemaDepth || oldN == nil || newN == nil {
		return
	}
	oldN = deref(oldN, d.oldSchemas)
	newN = deref(newN, d.newSchemas)
	if oldN == nil || newN == nil {
		return
	}
	mk = mk.at(newN.SourcePointer, oldN.SourcePointer)

	oldN = ir.FlattenAllOf(oldN, d.oldSchemas)
	newN = ir.FlattenAllOf(newN, d.newSchemas)

	if (oldN.Composition == nil) != (newN.Composition == nil) {
		d.emit(mk("schema-restructured", DeriveLevel(Incomparable, dir, Guaranteed, Guards{Uncertain: oldN.Type.IsUncertain() || newN.Type.IsUncertain()}),
			where+at(path)+" was restructured (composition added/removed)", argKey, path))
		return
	}
	if oldN.Composition != nil {
		oc, nc := oldN.Composition, newN.Composition
		if oc.Kind != nc.Kind {
			d.emit(mk("schema-restructured", DeriveLevel(Incomparable, dir, Guaranteed, Guards{}),
				fmt.Sprintf("%s%s changed composition kind %s → %s", where, at(path), oc.Kind, nc.Kind), argKey, path))
			return
		}
		switch {
		case len(nc.Members) < len(oc.Members):
			d.emit(mk(dirCheckID(dir, "variant-removed"), DeriveLevel(Narrows, dir, Guaranteed, Guards{Tolerated: dir == Response}),
				fmt.Sprintf("%s%s %s variants %d → %d — a documented shape was removed", where, at(path), oc.Kind, len(oc.Members), len(nc.Members)), argKey, path))
		case len(nc.Members) > len(oc.Members):
			d.emit(mk(dirCheckID(dir, "variant-added"), DeriveLevel(Widens, dir, Guaranteed, Guards{Uncertain: dir == Response}),
				fmt.Sprintf("%s%s %s variants %d → %d — consumers switching on shape may not handle the new one", where, at(path), oc.Kind, len(oc.Members), len(nc.Members)), argKey, path))
		}
		return
	}

	uncertain := oldN.Type.IsUncertain() || newN.Type.IsUncertain()
	label := where + at(path)

	if eff, tolerated, changed := typeEffect(oldN.Type.Value, newN.Type.Value, dir); changed {
		d.emit(mk(typeCheckID(dir), DeriveLevel(eff, dir, Guaranteed, Guards{Uncertain: uncertain, Tolerated: tolerated}),
			fmt.Sprintf("%s type %s → %s", label, oldN.Type.Value, newN.Type.Value),
			argKey, path, oldN.Type.Value, newN.Type.Value))
	}

	if !oldN.Nullable.Value && newN.Nullable.Value {
		if dir == Request {
			d.emit(mk("request-nullable-added", DeriveLevel(Widens, Request, Guaranteed, Guards{}),
				label+" now accepts null", argKey, path))
		} else {
			d.emit(mk("response-nullable-added", DeriveLevel(Widens, Response, Guaranteed, Guards{Uncertain: uncertain}),
				label+" may now be null — parsers that never handled null will break", argKey, path))
		}
	}
	if oldN.Nullable.Value && !newN.Nullable.Value {
		if dir == Request {
			d.emit(mk("request-nullable-removed", DeriveLevel(Narrows, Request, Guaranteed, Guards{Uncertain: uncertain}),
				label+" no longer accepts null", argKey, path))
		} else {
			d.emit(mk("response-nullable-removed", DeriveLevel(Narrows, Response, Guaranteed, Guards{Tolerated: true}),
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

func typeEffect(from, to string, dir Direction) (effect Effect, tolerated, changed bool) {
	if from == to || from == "unknown" || to == "unknown" || from == "" || to == "" {
		return "", false, false
	}
	widerNew := from == "integer" && to == "number"
	narrowerNew := from == "number" && to == "integer"
	switch {
	case widerNew:
		return Widens, false, true
	case narrowerNew:
		return Narrows, dir == Response, true
	default:
		return Incomparable, false, true
	}
}

func (d *differ) diffEnums(oldN, newN *ir.IrSchemaNode, dir Direction, label, argKey, path string, uncertain bool, mk mkFn) {
	oldVals := enumSet(oldN.EnumValues)
	newVals := enumSet(newN.EnumValues)
	switch {
	case oldVals == nil && newVals == nil:
		return
	case oldVals == nil:
		if dir == Request {
			d.emit(mk("request-enum-closed", DeriveLevel(Narrows, Request, Guaranteed, Guards{Uncertain: uncertain}),
				label+" is now restricted to an enum ["+joinSorted(newVals)+"]", argKey, path))
		} else {
			d.emit(mk("response-enum-closed", DeriveLevel(Narrows, Response, Guaranteed, Guards{Tolerated: true}),
				label+" is now documented as an enum ["+joinSorted(newVals)+"]", argKey, path))
		}
		return
	case newVals == nil:
		if dir == Request {
			d.emit(mk("request-enum-opened", DeriveLevel(Widens, Request, Guaranteed, Guards{}),
				label+" is no longer restricted to an enum", argKey, path))
		} else {
			d.emit(mk("response-enum-opened", DeriveLevel(Widens, Response, Guaranteed, Guards{Uncertain: uncertain}),
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
			d.emit(mk("request-enum-value-removed", DeriveLevel(Narrows, Request, Guaranteed, Guards{Uncertain: uncertain}),
				label+": value "+quote(v)+" no longer accepted", argKey, path, v))
		} else {
			d.emit(mk("response-enum-value-removed", DeriveLevel(Narrows, Response, Guaranteed, Guards{Tolerated: true}),
				label+": value "+quote(v)+" removed from the documented set", argKey, path, v))
		}
	}
	for _, v := range added {
		if dir == Request {
			d.emit(mk("request-enum-value-added", DeriveLevel(Widens, Request, Guaranteed, Guards{}),
				label+": accepts new value "+quote(v), argKey, path, v))
		} else {

			d.emit(mk("response-enum-value-added", DeriveLevel(Widens, Response, Guaranteed, Guards{Uncertain: uncertain}),
				label+": may now return "+quote(v)+" — exhaustive switches break", argKey, path, v))
		}
	}
}

func (d *differ) diffProperties(oldN, newN *ir.IrSchemaNode, dir Direction, where, argKey, path string, depth int, mk mkFn) {
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
			mk := mk.at("", op.Schema.SourcePointer)
			if dir == Request {

				d.emit(mk("request-property-removed",
					DeriveLevel(Narrows, Request, Optional, Guards{Uncertain: op.Required.IsUncertain()}),
					label+" removed from the request schema", argKey, ppath))
			} else if op.Required.Value {
				d.emit(mk("response-required-property-removed",
					DeriveLevel(Widens, Response, Guaranteed, Guards{Uncertain: op.Required.IsUncertain()}),
					label+" (guaranteed) removed — consumers reading it break", argKey, ppath))
			} else {
				d.emit(mk("response-property-removed",
					DeriveLevel(Narrows, Response, Optional, Guards{}),
					label+" (optional) removed", argKey, ppath))
			}
			continue
		}
		mk := mk.at(np.Schema.SourcePointer, op.Schema.SourcePointer)
		if !op.Required.Value && np.Required.Value {
			if dir == Request {
				d.emit(mk("request-property-became-required",
					DeriveLevel(Narrows, Request, Guaranteed, Guards{Uncertain: np.Required.IsUncertain()}),
					label+" became required — clients omitting it will be rejected", argKey, ppath))
			} else {
				d.emit(mk("response-property-became-required", DeriveLevel(Narrows, Response, Guaranteed, Guards{Tolerated: true}),
					label+" is now guaranteed present", argKey, ppath))
			}
		}
		if op.Required.Value && !np.Required.Value {
			if dir == Request {
				d.emit(mk("request-property-became-optional", DeriveLevel(Widens, Request, Guaranteed, Guards{}),
					label+" became optional", argKey, ppath))
			} else {
				d.emit(mk("response-property-became-optional",
					DeriveLevel(Widens, Response, Guaranteed, Guards{Uncertain: np.Required.IsUncertain()}),
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
		mk := mk.at(np.Schema.SourcePointer, "")
		if dir == Request {
			if np.Required.Value {
				d.emit(mk("request-property-added-required",
					DeriveLevel(Narrows, Request, Guaranteed, Guards{Uncertain: np.Required.IsUncertain()}),
					"new REQUIRED "+label+" — every existing client omits it", argKey, ppath))
			} else {
				d.emit(mk("request-property-added-optional", DeriveLevel(Widens, Request, Optional, Guards{}),
					"new optional "+label, argKey, ppath))
			}
		} else {

			d.emit(mk("response-property-added", DeriveLevel(Widens, Response, Guaranteed, Guards{Tolerated: true}),
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
