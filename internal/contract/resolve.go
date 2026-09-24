package contract

import (
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/ir"
)

const (
	typeSustainRate = 0.98
	typeMinSamples  = 10

	valueMinCount = 3
)

func (r *Refiner) Admit(def *ir.ApiDefinition, preferSpec bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	specView := buildSpecView(def)
	canon := newTemplateCanon(def)
	journaled := map[string]bool{}
	for _, a := range r.overlay.Admissions {
		journaled[admissionKeyOf(a)] = true
	}

	admitted := 0
	admit := func(a Admission) {
		a.At = r.now()
		k := admissionKeyOf(a)
		if journaled[k] || len(r.overlay.Admissions) >= maxAdmissions {
			return
		}
		r.overlay.Version++
		a.Version = r.overlay.Version
		r.overlay.Admissions = append(r.overlay.Admissions, a)
		journaled[k] = true
		admitted++
	}
	contradict := func(c Contradiction) {
		for _, ex := range r.overlay.Contradictions {
			if ex.Method == c.Method && ex.Template == c.Template && ex.StatusClass == c.StatusClass && ex.Field == c.Field {
				return
			}
		}
		if len(r.overlay.Contradictions) < maxContradictions {
			c.FirstSeen = r.now()
			r.overlay.Contradictions = append(r.overlay.Contradictions, c)
		}
	}

	for _, k := range sortedKeys(r.overlay.Endpoints) {
		ep := r.overlay.Endpoints[k]

		if ep.Samples < int64(r.minSamples) || r.now().Sub(ep.FirstSeen) < r.minAge {
			continue
		}

		template := canon.canonical(ep.Method, ep.Template)
		sourceKey := endpointKey(ep.Method, ep.Template, ep.StatusClass)
		specEp, endpointDeclared := specView[endpointKeyOf(ep.Method, template)]
		if !endpointDeclared {
			admit(Admission{Kind: AdmitEndpoint, Method: ep.Method, Template: template, StatusClass: ep.StatusClass})
		}

		for _, fieldPath := range sortedKeys(ep.Fields) {
			f := ep.Fields[fieldPath]
			presence := f.PresenceRate()
			if presence < r.presenceFloor {
				continue
			}
			declaredType := ""
			if endpointDeclared {
				declaredType = specEp.fieldTypes[fieldPath]
			}
			if declaredType == "" {

				admit(Admission{
					Kind: AdmitField, Method: ep.Method, Template: template, StatusClass: ep.StatusClass,
					Field: fieldPath, Type: dominantType(f.Types), Presence: presence, SourceKey: sourceKey,
				})
			} else if dom, share, n := dominantTypeStats(f.Types); dom != "" && dom != declaredType {

				if !preferSpec && share >= typeSustainRate && n >= typeMinSamples {
					admit(Admission{
						Kind: AdmitType, Method: ep.Method, Template: template, StatusClass: ep.StatusClass,
						Field: fieldPath, Type: dom, Presence: presence,
					})
					contradict(Contradiction{
						Method: ep.Method, Template: template, StatusClass: ep.StatusClass,
						Field: fieldPath, SpecClaim: declaredType, Observed: dom, Rate: share, Winner: "traffic",
					})
				} else {
					contradict(Contradiction{
						Method: ep.Method, Template: template, StatusClass: ep.StatusClass,
						Field: fieldPath, SpecClaim: declaredType, Observed: dom, Rate: share, Winner: "spec",
					})
				}
			}

			if !f.HighCardinality {
				declared := map[string]bool{}
				if endpointDeclared {
					for _, v := range specEp.fieldEnums[fieldPath] {
						declared[v] = true
					}
				}
				for _, v := range sortedKeys(f.Values) {
					if f.Values[v] >= valueMinCount && !declared[v] && len(declared) > 0 {
						admit(Admission{
							Kind: AdmitValue, Method: ep.Method, Template: template, StatusClass: ep.StatusClass,
							Field: fieldPath, Value: v, Presence: presence,
						})
					}
				}
			}
		}

		if endpointDeclared {
			for _, code := range sortedKeys(ep.StatusCodes) {
				if ep.StatusCodes[code] >= valueMinCount && !specEp.statuses[code] {
					admit(Admission{Kind: AdmitStatus, Method: ep.Method, Template: template, StatusClass: ep.StatusClass, Value: code})
				}
			}
		}
	}
	return admitted
}

func admissionKeyOf(a Admission) string {
	return strings.Join([]string{string(a.Kind), a.Method, a.Template, a.StatusClass, a.Field, a.Value, a.Type}, "|")
}

func endpointKeyOf(method, template string) string {
	return strings.ToUpper(method) + "|" + template
}

type templateCanon struct {
	byMethod map[string][][]string
	joined   map[string][]string
}

func newTemplateCanon(def *ir.ApiDefinition) *templateCanon {
	c := &templateCanon{byMethod: map[string][][]string{}, joined: map[string][]string{}}
	for i := range def.Endpoints {
		e := &def.Endpoints[i]
		m := strings.ToUpper(e.Method.Value)
		c.byMethod[m] = append(c.byMethod[m], strings.Split(strings.Trim(e.PathTemplate.Value, "/"), "/"))
		c.joined[m] = append(c.joined[m], e.PathTemplate.Value)
	}
	return c
}

func (c *templateCanon) canonical(method, trafficTemplate string) string {
	m := strings.ToUpper(method)
	segs := strings.Split(strings.Trim(trafficTemplate, "/"), "/")
	for i, spec := range c.byMethod[m] {
		if len(spec) != len(segs) {
			continue
		}
		match := true
		for j := range spec {
			if strings.Contains(spec[j], "{") {
				continue
			}
			if spec[j] != segs[j] {
				match = false
				break
			}
		}
		if match {
			return c.joined[m][i]
		}
	}
	return trafficTemplate
}

type specEndpoint struct {
	fieldTypes map[string]string
	fieldEnums map[string][]string
	statuses   map[string]bool
}

func buildSpecView(def *ir.ApiDefinition) map[string]*specEndpoint {
	named := map[string]*ir.IrSchemaNode{}
	for i := range def.Schemas {
		named[def.Schemas[i].ID] = &def.Schemas[i].Schema
	}
	out := map[string]*specEndpoint{}
	for i := range def.Endpoints {
		e := &def.Endpoints[i]
		se := &specEndpoint{fieldTypes: map[string]string{}, fieldEnums: map[string][]string{}, statuses: map[string]bool{}}
		for _, resp := range e.Responses {
			se.statuses[strings.ToLower(resp.StatusCode)] = true
			se.statuses[resp.StatusCode] = true
			if !strings.HasPrefix(resp.StatusCode, "2") {
				continue
			}
			for _, c := range resp.Content {
				flattenSchema(&c.Schema, named, "", 0, se)
			}
		}
		out[endpointKeyOf(e.Method.Value, e.PathTemplate.Value)] = se
	}
	return out
}

func flattenSchema(node *ir.IrSchemaNode, named map[string]*ir.IrSchemaNode, path string, depth int, se *specEndpoint) {
	if node == nil || depth > 12 {
		return
	}
	if node.Ref != nil {
		if target, ok := named[*node.Ref]; ok {
			flattenSchema(target, named, path, depth+1, se)
		}
		return
	}
	if path != "" {
		se.fieldTypes[path] = specType(node.Type.Value)
		if node.EnumValues != nil {
			for _, ev := range node.EnumValues.Value {
				if s, ok := ev.(string); ok {
					se.fieldEnums[path] = append(se.fieldEnums[path], s)
				}
			}
		}
	}
	for _, p := range node.Properties {
		child := p.Name
		if path != "" {
			child = path + "." + p.Name
		}
		flattenSchema(&p.Schema, named, child, depth+1, se)
	}
	if node.Items != nil {
		flattenSchema(node.Items, named, path+"[]", depth+1, se)
	}
}

func specType(t string) string {
	switch t {
	case "integer", "number":
		return "number"
	case "string", "boolean", "object", "array", "null":
		if t == "array" {
			return "object"
		}
		return t
	default:
		return ""
	}
}

func dominantType(types map[string]int64) string {
	d, _, _ := dominantTypeStats(types)
	return d
}

func dominantTypeStats(types map[string]int64) (dom string, share float64, n int64) {
	var total, best int64
	for _, t := range sortedKeys(types) {
		c := types[t]
		total += c
		if c > best {
			best, dom = c, t
		}
	}
	if total == 0 {
		return "", 0, 0
	}
	return dom, float64(best) / float64(total), total
}

type Effective struct {
	Version int

	AddedFields map[string]map[string]ObservedFieldSpec

	ValueUnions map[string]map[string][]string

	TypeOverrides map[string]map[string]string

	AddedStatuses map[string][]string

	AddedEndpoints map[string]string
	Contradictions []Contradiction
}

type ObservedFieldSpec struct {
	Type       string
	Presence   float64
	Provenance ir.Prov[string]
	Version    int

	Values []string
}

const ProvenanceObserved = "OBSERVED"

func ResolveAt(ov *Overlay, version int) *Effective {
	eff := &Effective{
		Version:        version,
		AddedFields:    map[string]map[string]ObservedFieldSpec{},
		ValueUnions:    map[string]map[string][]string{},
		TypeOverrides:  map[string]map[string]string{},
		AddedStatuses:  map[string][]string{},
		AddedEndpoints: map[string]string{},
		Contradictions: ov.Contradictions,
	}
	for _, a := range ov.Admissions {
		if a.Version > version {
			continue
		}
		ek := endpointKeyOf(a.Method, a.Template)
		switch a.Kind {
		case AdmitField:
			if eff.AddedFields[ek] == nil {
				eff.AddedFields[ek] = map[string]ObservedFieldSpec{}
			}
			var values []string
			srcKey := a.SourceKey
			if srcKey == "" {
				srcKey = endpointKey(a.Method, a.Template, a.StatusClass)
			}
			if ep := ov.Endpoints[srcKey]; ep != nil {
				if f := ep.Fields[a.Field]; f != nil {
					values = sortedKeys(f.Values)
				}
			}
			eff.AddedFields[ek][a.Field] = ObservedFieldSpec{
				Type: a.Type, Presence: a.Presence, Version: a.Version, Values: values,
				Provenance: ir.Prov[string]{Value: a.Type, Provenance: ProvenanceObserved, Confidence: a.Presence, Evidence: "traffic:" + a.At.UTC().Format(time.RFC3339)},
			}
		case AdmitValue:
			if eff.ValueUnions[ek] == nil {
				eff.ValueUnions[ek] = map[string][]string{}
			}
			eff.ValueUnions[ek][a.Field] = append(eff.ValueUnions[ek][a.Field], a.Value)
		case AdmitType:
			if eff.TypeOverrides[ek] == nil {
				eff.TypeOverrides[ek] = map[string]string{}
			}
			eff.TypeOverrides[ek][a.Field] = a.Type
		case AdmitStatus:
			eff.AddedStatuses[ek] = append(eff.AddedStatuses[ek], a.Value)
		case AdmitEndpoint:
			eff.AddedEndpoints[endpointKey(a.Method, a.Template, a.StatusClass)] = a.StatusClass
		}
	}
	return eff
}
