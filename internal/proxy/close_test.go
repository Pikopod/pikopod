package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pikopod/pikopod/internal/sanitize"
)

func TestCloseIsSafeUnderConcurrentServing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	s := testServer(t, up.URL, 4)
	rec := NewRecorder(t.TempDir(), sanitize.NewTokenizer("k", "local", 1), s.Metrics)
	go rec.Run(s.Captures())
	front := httptest.NewServer(s)
	defer front.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := http.Get(front.URL + "/examplepay/x")
				if err != nil {
					return
				}
				io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}()
	}
	for i := 0; i < 50; i++ {
		resp, err := http.Get(front.URL + "/examplepay/x")
		if err == nil {
			io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}
	s.Close()
	s.Close()
	select {
	case <-rec.Done():
	default:
		<-rec.Done()
	}
	for i := 0; i < 20; i++ {
		resp, err := http.Get(front.URL + "/examplepay/x")
		if err != nil {
			t.Fatalf("the data plane must keep serving after Close: %v", err)
		}
		io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status after close: %d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	close(stop)
	wg.Wait()
	if s.Metrics.RequestsProxied.Load() < 70 {
		t.Fatalf("requests must have been proxied throughout: %d", s.Metrics.RequestsProxied.Load())
	}
}
