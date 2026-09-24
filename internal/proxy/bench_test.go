package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func BenchmarkProxyServeObserverWedged(b *testing.B) {
	up := benchUpstream(b)
	defer up.Close()
	s := testServer(b, up.URL, 1)
	front := httptest.NewServer(s)
	defer front.Close()
	benchDrive(b, front)
}
