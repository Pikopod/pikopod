package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/drift"
	"github.com/pikopod/pikopod/internal/specdiff"
	"github.com/pikopod/pikopod/internal/specwatch"
)

func fam(method, template, class string, samples int) *baseline.Family {
	return &baseline.Family{
		Method: method, Template: template, StatusClass: class,
		Samples: samples, LastSeen: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Fields:      map[string]*baseline.FieldStats{},
		StatusCodes: map[string]int{},
	}
}

func TestEnrichEndpointRemovedWithLiveTraffic(t *testing.T) {
	f := specdiff.Finding{ID: "endpoint-removed", Level: specdiff.Warn,
		Method: "GET", Template: "/tx/{txId}", Detail: "endpoint removed from the spec"}

	fams := []*baseline.Family{fam("GET", "/tx/{id}", "2xx", 120)}
	EnrichDeclaredFinding(&f, fams)
	if f.Level != specdiff.Err {
		t.Fatalf("live traffic must floor the removal at ERR, got %s", f.Level)
	}
	if !strings.Contains(f.Detail, "120 samples") {
		t.Fatalf("detail lacks evidence: %s", f.Detail)
	}
}

func TestEnrichEndpointRemovedNoTrafficUntouched(t *testing.T) {
	f := specdiff.Finding{ID: "endpoint-removed", Level: specdiff.Warn,
		Method: "GET", Template: "/tx/{txId}", Detail: "endpoint removed"}
	EnrichDeclaredFinding(&f, []*baseline.Family{fam("GET", "/other", "2xx", 50)})
	if f.Level != specdiff.Warn || strings.Contains(f.Detail, "samples") {
		t.Fatalf("no evidence, no change: %+v", f)
	}
}

func TestEnrichPropertyRemovedWithPresence(t *testing.T) {
	f := specdiff.Finding{ID: "response-property-removed", Level: specdiff.Warn,
		Method: "GET", Template: "/tx/{id}", Detail: "field removed",
		Args: []string{"200 application/json", "data.fee"}}
	fm := fam("GET", "/tx/{id}", "2xx", 100)

	fm.Fields["data/fee"] = &baseline.FieldStats{Count: 100, Types: map[string]int{"string": 100}}
	EnrichDeclaredFinding(&f, []*baseline.Family{fm})
	if f.Level != specdiff.Err {
		t.Fatalf("~always-present field removal must floor at ERR: %s", f.Level)
	}
	if !strings.Contains(f.Detail, "presence 100%") {
		t.Fatalf("detail: %s", f.Detail)
	}
}

func TestEnrichEnumValueRemovedStillOnWire(t *testing.T) {
	f := specdiff.Finding{ID: "response-enum-value-removed", Level: specdiff.Info,
		Method: "GET", Template: "/tx/{id}", Detail: "value removed",
		Args: []string{"200 application/json", "data.status", "on_hold"}}
	fm := fam("GET", "/tx/{id}", "2xx", 50)
	fm.Fields["data/status"] = &baseline.FieldStats{Count: 50,
		Types: map[string]int{"string": 50}, Values: map[string]int{"active": 40, "on_hold": 10}}
	EnrichDeclaredFinding(&f, []*baseline.Family{fm})
	if f.Level != specdiff.Warn {
		t.Fatalf("wire still carries the removed value — escalate: %s", f.Level)
	}
	if !strings.Contains(f.Detail, "disagrees with the wire") {
		t.Fatalf("detail: %s", f.Detail)
	}
}

func TestEnrichStatusRemovedStillOccurring(t *testing.T) {
	f := specdiff.Finding{ID: "response-status-removed", Level: specdiff.Info,
		Method: "GET", Template: "/tx/{id}", Detail: "status removed", Args: []string{"404"}}
	fm := fam("GET", "/tx/{id}", "4xx", 30)
	fm.StatusCodes["404"] = 12
	EnrichDeclaredFinding(&f, []*baseline.Family{fm})
	if f.Level != specdiff.Err || !strings.Contains(f.Detail, "12 times") {
		t.Fatalf("%+v", f)
	}
}

func TestEnrichRequestSideChecksPassThrough(t *testing.T) {
	f := specdiff.Finding{ID: "param-added-required", Level: specdiff.Err,
		Method: "GET", Template: "/tx/{id}", Detail: "new required param",
		Args: []string{"query", "tenant"}}
	before := f
	EnrichDeclaredFinding(&f, []*baseline.Family{fam("GET", "/tx/{id}", "2xx", 100)})
	if f.Detail != before.Detail || f.Level != before.Level {
		t.Fatalf("baselines are response-side; request checks must pass through untouched: %+v", f)
	}
}

func TestEnrichNeverLowersLevel(t *testing.T) {
	f := specdiff.Finding{ID: "endpoint-removed", Level: specdiff.Err,
		Method: "GET", Template: "/x", Detail: "d"}
	EnrichDeclaredFinding(&f, nil)
	if f.Level != specdiff.Err {
		t.Fatal("enrichment must never lower a level")
	}
}

func docJournal(changes ...specwatch.DocumentedChange) *specwatch.Documented {
	return &specwatch.Documented{Changes: changes}
}

func TestAnnotateDocumentedFieldAdded(t *testing.T) {
	fs := []drift.Finding{{
		Upstream: "pay", Method: "GET", Template: "/tx/{id}", StatusClass: "2xx",
		Kind: drift.FieldAdded, Field: "data/fee", After: "string",
	}}
	fp := fs[0].Fingerprint()
	AnnotateDocumented(fs, docJournal(specwatch.DocumentedChange{
		ID: "response-property-added", Method: "GET", Template: "/tx/{txId}", Path: "data.fee",
	}))
	if !fs[0].Documented || fs[0].Note == "" {
		t.Fatalf("documented field add must annotate: %+v", fs[0])
	}
	if fs[0].Fingerprint() != fp {
		t.Fatal("annotation must never move the fingerprint")
	}
}

func TestAnnotateDocumentedEnumValue(t *testing.T) {
	fs := []drift.Finding{{
		Method: "GET", Template: "/tx/{id}", Kind: drift.EnumValueNew,
		Field: "data.status", After: "on_hold",
	}}
	AnnotateDocumented(fs, docJournal(specwatch.DocumentedChange{
		ID: "response-enum-value-added", Method: "GET", Template: "/tx/{id}",
		Path: "data.status", Value: "on_hold",
	}))
	if !fs[0].Documented {
		t.Fatalf("%+v", fs[0])
	}

	fs2 := []drift.Finding{{Method: "GET", Template: "/tx/{id}", Kind: drift.EnumValueNew,
		Field: "data.status", After: "other"}}
	AnnotateDocumented(fs2, docJournal(specwatch.DocumentedChange{
		ID: "response-enum-value-added", Method: "GET", Template: "/tx/{id}",
		Path: "data.status", Value: "on_hold",
	}))
	if fs2[0].Documented {
		t.Fatal("undocumented value must stay a live finding")
	}
}

func TestAnnotateDocumentedStatusClass(t *testing.T) {

	fs := []drift.Finding{{Method: "GET", Template: "/tx/{id}", Kind: drift.StatusNew, After: "4xx"}}
	AnnotateDocumented(fs, docJournal(specwatch.DocumentedChange{
		ID: "response-status-added", Method: "GET", Template: "/tx/{id}", Value: "429",
	}))
	if !fs[0].Documented {
		t.Fatalf("%+v", fs[0])
	}

	fs2 := []drift.Finding{{Method: "GET", Template: "/tx/{id}", Kind: drift.StatusCodeChanged, After: "429"}}
	AnnotateDocumented(fs2, docJournal(specwatch.DocumentedChange{
		ID: "response-status-added", Method: "GET", Template: "/tx/{id}", Value: "429",
	}))
	if !fs2[0].Documented {
		t.Fatalf("%+v", fs2[0])
	}

	fs3 := []drift.Finding{{Method: "GET", Template: "/tx/{id}", Kind: drift.StatusNew, After: "5xx"}}
	AnnotateDocumented(fs3, docJournal(specwatch.DocumentedChange{
		ID: "response-status-added", Method: "GET", Template: "/tx/{id}", Value: "429",
	}))
	if fs3[0].Documented {
		t.Fatal("class must match")
	}
}

func TestAnnotateRemovalsNeverDowngraded(t *testing.T) {

	fs := []drift.Finding{{Method: "GET", Template: "/tx/{id}", Kind: drift.FieldRemoved, Field: "data.fee"}}
	AnnotateDocumented(fs, docJournal(specwatch.DocumentedChange{
		ID: "response-property-added", Method: "GET", Template: "/tx/{id}", Path: "data.fee",
	}))
	if fs[0].Documented {
		t.Fatal("removals must never be downgraded")
	}
}
