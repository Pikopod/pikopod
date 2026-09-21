package specdiff

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

func gaLines(r *Report, p Positions) []string {
	var buf bytes.Buffer
	r.WriteGitHubActions(&buf, p)
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

func TestGitHubActionsLevelMapping(t *testing.T) {
	r := BuildReport("a", "b", []Finding{
		{ID: "x-err", Level: Err, Method: "GET", Template: "/a", Detail: "d"},
		{ID: "x-warn", Level: Warn, Method: "GET", Template: "/b", Detail: "d"},
		{ID: "x-info", Level: Info, Method: "GET", Template: "/c", Detail: "d"},
	})
	lines := gaLines(r, Positions{})
	want := []string{"::error title=x-err::x-err: GET /a — d", "::warning title=x-warn::x-warn: GET /b — d", "::notice title=x-info::x-info: GET /c — d"}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("line %d:\n got %q\nwant %q", i, lines[i], w)
		}
	}
}

func TestGitHubActionsEscaping(t *testing.T) {
	r := BuildReport("a", "b", []Finding{{ID: "x", Level: Err, Method: "GET", Template: "/a", Detail: "100% sure\r\nof: this, that"}})
	line := gaLines(r, Positions{})[0]
	if strings.ContainsAny(line, "\r\n") || !strings.Contains(line, "100%25 sure%0D%0Aof: this, that") {
		t.Fatalf("message must be escaped per the workflow-command rules: %q", line)
	}
}

func TestGitHubActionsWithPosition(t *testing.T) {
	r := BuildReport("a", "b", []Finding{
		{ID: "x", Level: Err, Method: "GET", Template: "/a", Detail: "d", SourcePointer: "#/paths/~1a/get/responses/200", SourceSide: SideNew},
		{ID: "y", Level: Warn, Method: "GET", Template: "/a", Detail: "gone", SourcePointer: "#/paths/~1a/get/parameters/0", SourceSide: SideOld},
	})
	p := Positions{
		New: ir.Positions{"#/paths/~1a/get": {File: "openapi.yaml", Line: 12, Col: 5}},
		Old: ir.Positions{"#/paths/~1a/get/parameters/0": {File: "old.yaml", Line: 40, Col: 9}},
	}
	lines := gaLines(r, p)
	if lines[0] != "::error file=openapi.yaml,line=12,col=5,title=x::x: GET /a — d" {
		t.Fatalf("new-side finding resolves through its nearest known ancestor: %q", lines[0])
	}
	if lines[1] != "::warning file=old.yaml,line=40,col=9,title=y::y: GET /a — gone" {
		t.Fatalf("old-side finding resolves in the old document: %q", lines[1])
	}
}

func TestGitHubActionsWithoutPosition(t *testing.T) {
	r := BuildReport("a", "b", []Finding{{ID: "x", Level: Err, Method: "GET", Template: "/a", Detail: "d", SourcePointer: "#/nowhere", SourceSide: SideNew}})
	line := gaLines(r, Positions{New: ir.Positions{"#/elsewhere": {Line: 3}}})[0]
	if strings.Contains(line, "file=") || strings.Contains(line, "line=") || !strings.HasPrefix(line, "::error title=x::") {
		t.Fatalf("no invented location: %q", line)
	}
}

func TestGitHubActionsNothingElseOnStdout(t *testing.T) {
	old := def(ep("GET", "/widgets"), ep("GET", "/gadgets"))
	nu := def(ep("GET", "/widgets"))
	r := BuildReport("a", "b", Diff(old, nu))
	if len(r.Items) == 0 {
		t.Fatal("fixture must produce findings")
	}
	for _, line := range gaLines(r, Positions{}) {
		if !strings.HasPrefix(line, "::") {
			t.Fatalf("every stdout line is a workflow command: %q", line)
		}
	}
	var empty bytes.Buffer
	BuildReport("a", "b", nil).WriteGitHubActions(&empty, Positions{})
	if empty.Len() != 0 {
		t.Fatalf("no findings, no output: %q", empty.String())
	}
}

func TestFingerprintIgnoresSourcePointer(t *testing.T) {
	a := Finding{ID: "x", Method: "GET", Template: "/a", Args: []string{"p"}, SourcePointer: "#/one", SourceSide: SideNew}
	b := Finding{ID: "x", Method: "GET", Template: "/a", Args: []string{"p"}, SourcePointer: "#/two", SourceSide: SideOld}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("a pointer moves when unrelated lines move; it must not enter the fingerprint")
	}
}

func TestFindingsCarryTheNewSidePointer(t *testing.T) {
	old := def(ep("GET", "/widgets", func(e *ir.Endpoint) { e.SourcePointer = "#/paths/~1widgets/get" }), ep("GET", "/gadgets", func(e *ir.Endpoint) { e.SourcePointer = "#/paths/~1gadgets/get" }))
	nu := def(ep("GET", "/widgets", func(e *ir.Endpoint) {
		e.SourcePointer = "#/paths/~1widgets/get"
		e.Parameters = []ir.Parameter{{Name: "tenant", Location: "query", Required: ir.Explicit(true, ""), Schema: strSchema(), SourcePointer: "#/paths/~1widgets/get/parameters/0"}}
	}))
	fs := Diff(old, nu)
	if f := find(t, fs, "param-added-required"); f.SourcePointer != "#/paths/~1widgets/get/parameters/0" || f.SourceSide != SideNew {
		t.Fatalf("added param points at the new document: %+v", f)
	}
	if f := find(t, fs, "endpoint-removed"); f.SourcePointer != "#/paths/~1gadgets/get" || f.SourceSide != SideOld {
		t.Fatalf("a removal points at the old document: %+v", f)
	}
}
