package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The proxy sits in a production payment path, so its cost per request is a
// product claim, not a curiosity. These benchmarks measure the two states that
// matter: observation keeping up, and observation fully wedged. The fail-open
// contract says the second must not be materially slower than the first — if
// it is, a stalled observer is a latency regression on real traffic.

func benchUpstream(b *testing.B) *httptest.Server {
	b.Helper()
	body := bytes.Repeat([]byte(`{"id":"tx_1","status":"success","amount":1250}`), 8)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(body)
	}))
}

func benchDrive(b *testing.B, front *httptest.Server) {
	b.Helper()
	payload := []byte(`{"amount":1250,"currency":"NGN"}`)
	client := front.Client()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Post(front.URL+"/examplepay/transaction", "application/json", bytes.NewReader(payload))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	b.StopTimer()
}

// Steady state: a reader drains captures as fast as they arrive.
func BenchmarkProxyServe(b *testing.B) {
	up := benchUpstream(b)
	defer up.Close()
	s := testServer(b, up.URL, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range s.Captures() {
		}
	}()
	front := httptest.NewServer(s)
	defer front.Close()
	benchDrive(b, front)
}

// Worst case: capture depth 1 and NOBODY draining, so every request overflows
// the channel and takes the drop path. This is the fail-open guarantee under
// load — compare ns/op against BenchmarkProxyServe.
func BenchmarkProxyServeObserverWedged(b *testing.B) {
	up := benchUpstream(b)
	defer up.Close()
	s := testServer(b, up.URL, 1)
	front := httptest.NewServer(s)
	defer front.Close()
	benchDrive(b, front)
}
