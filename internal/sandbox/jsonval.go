package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type JSONObject struct {
	keys []string
	vals map[string]any
}

func NewJSONObject() *JSONObject {
	return &JSONObject{vals: map[string]any{}}
}

func (o *JSONObject) Set(key string, value any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

func (o *JSONObject) Get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *JSONObject) Has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

func (o *JSONObject) Len() int { return len(o.keys) }

func (o *JSONObject) Keys() []string { return o.keys }

func (o *JSONObject) Clone() *JSONObject {
	out := NewJSONObject()
	for _, k := range o.keys {
		out.Set(k, o.vals[k])
	}
	return out
}

func (o *JSONObject) MarshalJSON() ([]byte, error) {
	return marshalJSValue(o)
}

func marshalJSValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeJSValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeJSValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return writeJSString(buf, t)
	case json.Number:
		buf.WriteString(string(t))
	case json.RawMessage:
		buf.Write(t)
	case *JSONObject:
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeJSValue(buf, t.vals[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]string:

		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSString(buf, k)
			buf.WriteByte(':')
			writeJSString(buf, t[k])
		}
		buf.WriteByte('}')
	default:

		enc, err := encodeNoHTMLEscape(v)
		if err != nil {
			return err
		}
		buf.Write(enc)
	}
	return nil
}

func writeJSString(buf *bytes.Buffer, s string) error {
	enc, err := encodeNoHTMLEscape(s)
	if err != nil {
		return err
	}
	buf.Write(enc)
	return nil
}

func encodeNoHTMLEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func parseJSONValue(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeJSValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

const maxJSONDepth = 512

func decodeJSValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, fmt.Errorf("JSON value nests deeper than %d levels", maxJSONDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewJSONObject()
			for dec.More() {
				kTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kTok.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				val, err := decodeJSValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj.Set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				item, err := decodeJSValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, item)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return tok, nil
	}
}
