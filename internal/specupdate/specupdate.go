package specupdate

import (
	"fmt"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/conformance"
	"github.com/pikopod/pikopod/internal/contract"
)

type ChangeKind string

const (
	AddStatus    ChangeKind = "add-status"
	EnumUnion    ChangeKind = "enum-union"
	NullableWrap ChangeKind = "nullable-wrap"
	AddProperty  ChangeKind = "add-property"
	Retype       ChangeKind = "retype"
	MakeOptional ChangeKind = "make-optional"
	AddEndpoint  ChangeKind = "add-endpoint"
)

func (k ChangeKind) Additive() bool {
	switch k {
	case AddStatus, EnumUnion, NullableWrap, AddProperty:
		return true
	}
	return false
}

type Evidence struct {
	Occurrences int     `json:"occurrences,omitempty"`
	Presence    float64 `json:"presence,omitempty"`
	Since       string  `json:"since,omitempty"`
}

type Change struct {
	Kind        ChangeKind `json:"kind"`
	Method      string     `json:"method"`
	Template    string     `json:"template"`
	Status      int        `json:"status,omitempty"`
	StatusClass string     `json:"status_class,omitempty"`
	Pointer     string     `json:"pointer,omitempty"`
	Value       string     `json:"value,omitempty"`
	PropType    string     `json:"prop_type,omitempty"`
	Reason      string     `json:"reason"`
	Evidence    Evidence   `json:"evidence"`
}

type Op struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type Patch struct {
	Change
	Ops []Op `json:"ops,omitempty"`
}

func DeriveChanges(rep *conformance.Report, ov *contract.Overlay) []Change {
	var out []Change
	seen := map[string]bool{}
	add := func(c Change) {
		key := string(c.Kind) + "|" + c.Method + "|" + c.Template + "|"
		if c.Kind == AddStatus {

			key += fmt.Sprint(c.Status)
		} else {
			key += fmt.Sprint(c.Status) + "|" + c.StatusClass + "|" + c.Pointer + "|" + c.Value
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}

	if rep != nil {
		for _, v := range rep.Violations {
			base := Change{Method: v.Method, Template: v.Template, Status: v.Status,
				Pointer: v.Pointer, Evidence: Evidence{Occurrences: v.Occurrences}}
			switch v.Code {
			case "status_undeclared":
				base.Kind = AddStatus
				base.Reason = fmt.Sprintf("traffic returns %d (%d×) but the spec does not declare it", v.Status, v.Occurrences)
			case "enum":
				base.Kind, base.Value = EnumUnion, v.Observed
				base.Reason = fmt.Sprintf("traffic carries %q (%d×) outside the documented enum", v.Observed, v.Occurrences)
			case "type":
				if v.Observed == "null" {
					base.Kind = NullableWrap
					base.Reason = fmt.Sprintf("field arrived null (%d×) but is not documented nullable", v.Occurrences)
				} else {
					base.Kind, base.Value = Retype, v.Observed
					base.Reason = fmt.Sprintf("traffic disagrees with the documented type (%s observed, %d×) — retyping narrows; review required", v.Observed, v.Occurrences)
				}
			case "required":
				base.Kind = MakeOptional
				base.Reason = fmt.Sprintf("documented required but absent in traffic (%d×) — dropping `required` withdraws a consumer guarantee; review required", v.Occurrences)
			default:
				continue
			}
			add(base)
		}
	}

	if ov != nil {
		for _, a := range ov.Admissions {
			base := Change{Method: strings.ToUpper(a.Method), Template: a.Template,
				StatusClass: a.StatusClass, Pointer: dottedToPointer(a.Field),
				Evidence: Evidence{Presence: a.Presence, Since: a.At.UTC().Format(time.RFC3339)}}
			switch a.Kind {
			case contract.AdmitField:
				base.Kind, base.Value, base.PropType = AddProperty, lastSegment(a.Field), a.Type
				base.Pointer = dottedToPointer(parentPath(a.Field))
				base.Reason = fmt.Sprintf("undeclared field `%s` sustained in traffic (presence %.0f%%)", a.Field, a.Presence*100)
			case contract.AdmitValue:
				base.Kind, base.Value = EnumUnion, a.Value
				base.Reason = fmt.Sprintf("observed enum value %q sustained in traffic", a.Value)
			case contract.AdmitStatus:
				base.Kind = AddStatus
				fmt.Sscanf(a.Value, "%d", &base.Status)
				base.Pointer = ""
				base.Reason = fmt.Sprintf("undeclared status %s sustained in traffic", a.Value)
			case contract.AdmitType:
				base.Kind, base.Value = Retype, a.Type
				base.Reason = fmt.Sprintf("traffic-sustained type %s contradicts the spec — retyping narrows; review required", a.Type)
			case contract.AdmitEndpoint:
				base.Kind, base.Pointer = AddEndpoint, ""
				base.Reason = "undeclared endpoint sustained in traffic — authoring a whole operation needs review"
			default:
				continue
			}
			if base.Kind == AddStatus && base.Status == 0 {
				continue
			}
			add(base)
		}
	}
	return out
}

func dottedToPointer(field string) string {
	if field == "" {
		return ""
	}

	var segs []string
	for _, s := range strings.Split(field, ".") {
		base := s
		stars := 0
		for strings.HasSuffix(base, "[]") {
			base = strings.TrimSuffix(base, "[]")
			stars++
		}
		if base != "" {
			segs = append(segs, base)
		}
		for i := 0; i < stars; i++ {
			segs = append(segs, "*")
		}
	}
	return "/" + strings.Join(segs, "/")
}

func parentPath(field string) string {
	if i := strings.LastIndex(field, "."); i >= 0 {
		return field[:i]
	}
	return ""
}

func lastSegment(field string) string {
	if i := strings.LastIndex(field, "."); i >= 0 {
		return field[i+1:]
	}
	return field
}
