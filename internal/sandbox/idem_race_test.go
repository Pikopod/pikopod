package sandbox

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentIdempotentCreatesYieldOneResource(t *testing.T) {
	e := newEngine(t, loadWidgets(t), Config{ID: "sbx_idem", Seed: "idem-1"})

	const workers = 8
	body := `{"name":"gadget"}`
	statuses := make([]int, workers)
	ids := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/widgets", strings.NewReader(body))
			req.Header.Set("content-type", "application/json")
			req.Header.Set("idempotency-key", "storm-key-1")
			req.Header.Set("authorization", "Bearer "+IssuedCredential("idem-1"))
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			statuses[i] = w.Code
			var parsed map[string]any
			if json.Unmarshal(w.Body.Bytes(), &parsed) == nil {
				if id, ok := parsed["id"].(string); ok {
					ids[i] = id
				}
			}
		}(i)
	}
	wg.Wait()

	distinct := map[string]bool{}
	for i, id := range ids {
		if statuses[i] >= 400 {
			t.Fatalf("worker %d failed: %d", i, statuses[i])
		}
		if id != "" {
			distinct[id] = true
		}
	}
	if len(distinct) != 1 {
		t.Fatalf("same idempotency key must yield ONE resource, got %d distinct ids: %v", len(distinct), distinct)
	}
}
