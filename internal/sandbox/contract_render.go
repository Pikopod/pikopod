package sandbox

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/contract"
)

const ContractVersionHeader = "x-pikopod-contract-version"

const presenceAlwaysFloor = 0.98

func (e *Engine) applyContract(resp *RawResponse, method, template, innerPath string) {
	if e.effective == nil || resp == nil {
		return
	}
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	resp.Headers[ContractVersionHeader] = strconv.Itoa(e.effective.Version)

	if resp.Status < 200 || resp.Status >= 300 || len(resp.Body) == 0 {
		return
	}
	ek := strings.ToUpper(method) + "|" + template
	added := e.effective.AddedFields[ek]
	overrides := e.effective.TypeOverrides[ek]
	if len(added) == 0 && len(overrides) == 0 {
		return
	}
	doc, err := parseJSONValue(string(resp.Body))
	if err != nil {
		return
	}
	obj, ok := doc.(*JSONObject)
	if !ok {
		return
	}

	anchor := innerPath
	if id, ok := obj.Get("id"); ok {
		if s, isStr := id.(string); isStr {
			anchor = s
		}
	}

	if len(added) > 0 {
		e.tracef("overlay", "contract v%d admits %d traffic-learned field(s) for this endpoint", e.effective.Version, len(added))
	}
	for _, fieldPath := range sortedStrings(added) {
		spec := added[fieldPath]
		if strings.Contains(fieldPath, "[]") {
			continue
		}

		if spec.Presence < presenceAlwaysFloor {
			roll := NewPrng(e.seed + ":presence:" + template + ":" + fieldPath + ":" + anchor).Next()
			if roll >= spec.Presence {
				continue
			}
		}
		setDotted(obj, fieldPath, e.observedValue(spec, template, fieldPath, anchor))
	}
	for _, fieldPath := range sortedStrings(overrides) {
		overrideSynthesizedField(obj, fieldPath, overrides[fieldPath], e, template, anchor)
	}

	if body, err := marshalJSValue(obj); err == nil {
		resp.Body = body
		resp.Headers["content-length"] = strconv.Itoa(len(body))
	}
}

func (e *Engine) observedValue(spec contract.ObservedFieldSpec, template, fieldPath, anchor string) any {
	prng := NewPrng(e.seed + ":observed:" + template + ":" + fieldPath + ":" + anchor)
	if len(spec.Values) > 0 {
		return spec.Values[prng.Int(0, len(spec.Values)-1)]
	}
	switch spec.Type {
	case "number":
		return float64(prng.Int(1, 1000))
	case "boolean":
		return prng.Bool()
	case "null":
		return nil
	case "object":
		return NewJSONObject()
	default:
		return prng.Token(8)
	}
}

func overrideSynthesizedField(obj *JSONObject, fieldPath, wantType string, e *Engine, template, anchor string) {
	parent, last := walkToParent(obj, fieldPath)
	if parent == nil {
		return
	}
	cur, ok := parent.Get(last)
	if !ok || jsonTypeName(cur) == wantType {
		return
	}
	prng := NewPrng(e.seed + ":override:" + template + ":" + fieldPath + ":" + anchor)
	switch wantType {
	case "string":

		switch n := cur.(type) {
		case float64:
			parent.Set(last, strconv.FormatFloat(n, 'f', -1, 64))
			return
		case json.Number:
			parent.Set(last, n.String())
			return
		}
		parent.Set(last, prng.Token(8))
	case "number":
		if s, isStr := cur.(string); isStr {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				parent.Set(last, f)
				return
			}
		}
		parent.Set(last, float64(prng.Int(1, 1000)))
	case "boolean":
		parent.Set(last, prng.Bool())
	}
}

func (e *Engine) serveObserved(method, innerPath string) *RawResponse {
	if e.effective == nil {
		return nil
	}
	template := e.observedTemplateFor(method, innerPath)
	if template == "" {
		return nil
	}
	ek := strings.ToUpper(method) + "|" + template
	obj := NewJSONObject()
	added := e.effective.AddedFields[ek]
	for _, fieldPath := range sortedStrings(added) {
		if strings.Contains(fieldPath, "[]") {
			continue
		}
		spec := added[fieldPath]
		if spec.Presence < presenceAlwaysFloor {
			if NewPrng(e.seed+":presence:"+template+":"+fieldPath+":"+innerPath).Next() >= spec.Presence {
				continue
			}
		}
		setDotted(obj, fieldPath, e.observedValue(spec, template, fieldPath, innerPath))
	}
	body, err := marshalJSValue(obj)
	if err != nil {
		return nil
	}
	resp := &RawResponse{Status: 200, Headers: map[string]string{
		"content-type":        jsonContentType,
		ContractVersionHeader: strconv.Itoa(e.effective.Version),
		"x-pikopod-contract":  "observed-endpoint",
	}, Body: body}
	return resp
}

func (e *Engine) observedTemplateFor(method, innerPath string) string {
	for key := range e.effective.AddedEndpoints {
		parts := strings.SplitN(key, "|", 3)
		if len(parts) < 2 || !strings.EqualFold(parts[0], method) {
			continue
		}
		if templateMatchesPath(parts[1], innerPath) {
			return parts[1]
		}
	}
	return ""
}

func templateMatchesPath(template, path string) bool {
	t := strings.Split(strings.Trim(template, "/"), "/")
	p := strings.Split(strings.Trim(path, "/"), "/")
	if len(t) != len(p) {
		return false
	}
	for i := range t {
		if strings.Contains(t[i], "{") {
			continue
		}
		if t[i] != p[i] {
			return false
		}
	}
	return true
}

func setDotted(obj *JSONObject, dotted string, value any) {
	parent, last := ensureParent(obj, dotted)
	if parent == nil {
		return
	}
	if _, exists := parent.Get(last); exists {
		return
	}
	parent.Set(last, value)
}

func ensureParent(obj *JSONObject, dotted string) (*JSONObject, string) {
	segs := strings.Split(dotted, ".")
	cur := obj
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur.Get(seg)
		if !ok {
			child := NewJSONObject()
			cur.Set(seg, child)
			cur = child
			continue
		}
		child, isObj := next.(*JSONObject)
		if !isObj {
			return nil, ""
		}
		cur = child
	}
	return cur, segs[len(segs)-1]
}

func walkToParent(obj *JSONObject, dotted string) (*JSONObject, string) {
	segs := strings.Split(dotted, ".")
	cur := obj
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur.Get(seg)
		if !ok {
			return nil, ""
		}
		child, isObj := next.(*JSONObject)
		if !isObj {
			return nil, ""
		}
		cur = child
	}
	return cur, segs[len(segs)-1]
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "object"
	}
}

func sortedStrings[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
