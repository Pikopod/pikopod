package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/baseline"
	"github.com/pikopod/pikopod/internal/specdiff"
)

var benchmarkEnrichedFinding specdiff.Finding

func benchmarkFamily(method, template, statusClass string, samples int) *baseline.Family {
	return &baseline.Family{
		Method:      method,
		Template:    template,
		StatusClass: statusClass,
		Samples:     samples,
		LastSeen:    time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Fields:      map[string]*baseline.FieldStats{},
		StatusCodes: map[string]int{},
	}
}

func benchmarkFamilies() []*baseline.Family {
	fams := make([]*baseline.Family, 0, 32)
	for i := 0; i < 24; i++ {
		fams = append(fams, benchmarkFamily(
			"GET",
			fmt.Sprintf("/unrelated/%d", i),
			"2xx",
			50+i,
		))
	}
	for i := 0; i < 6; i++ {
		fams = append(fams, benchmarkFamily(
			"POST",
			fmt.Sprintf("/payments/%d/refund", i),
			"2xx",
			80+i,
		))
	}
	fams = append(fams,
		benchmarkFamily("GET", "/payments/{paymentId}", "2xx", 240),
		benchmarkFamily("GET", "/payments/{id}", "2xx", 180),
	)
	return fams
}

func BenchmarkEnrichDeclaredFinding(b *testing.B) {
	base := specdiff.Finding{
		ID:       "endpoint-removed",
		Level:    specdiff.Warn,
		Method:   "GET",
		Template: "/payments/{id}",
		Detail:   "endpoint removed from the spec",
	}
	fams := benchmarkFamilies()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := base
		EnrichDeclaredFinding(&f, fams)
		benchmarkEnrichedFinding = f
	}
}
