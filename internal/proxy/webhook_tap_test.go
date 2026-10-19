package proxy

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/config"
)

func tapServer(t *testing.T, receiverURL string, depth int) *Server {
	t.Helper()
	cfg := &config.Config{Upstreams: map[string]config.Upstream{
		"examplepay": {Listen: "/examplepay", Target: "https://api.example.invalid", Webhooks: &config.Webhooks{Receiver: receiverURL}},
	}}
	s, err := New(cfg, &Metrics{}, depth)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhookTapForwardsByteIdenticalAndReturnsTheReceiversAnswer(t *testing.T) {
	var gotBody []byte
	var gotSig, gotPath string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Provider-Signature")
		gotPath = r.URL.Path
		w.Header().Set("X-Receiver", "handled")
		w.WriteHeader(202)
		w.Write([]byte(`{"received":true}`))
	}))
	defer receiver.Close()

	s := tapServer(t, receiver.URL+"/hooks/in", 8)
	front := httptest.NewServer(s)
	defer front.Close()

	payload := []byte(`{"id":"evt_1","type":"charge.succeeded","data":{"id":"ch_1"}}`)
	req, _ := http.NewRequest("POST", front.URL+"/hooks/examplepay", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provider-Signature", "t=1,v1=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 202 || string(body) != `{"received":true}` || resp.Header.Get("X-Receiver") != "handled" {
		t.Fatalf("the provider must see the receiver's answer untouched: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if !bytes.Equal(gotBody, payload) || gotSig != "t=1,v1=abc" || gotPath != "/hooks/in" {
		t.Fatalf("the receiver must get the delivery byte-identical at its own path: %q %q %q", gotBody, gotSig, gotPath)
	}
	if s.Metrics.WebhooksReceived.Load() != 1 {
		t.Fatalf("received=%d", s.Metrics.WebhooksReceived.Load())
	}
	select {
	case ex := <-s.Captures():
		if !ex.Inbound || ex.Upstream != "examplepay" || ex.Status != 202 || !bytes.Equal(ex.ReqBody, payload) {
			t.Fatalf("the delivery is captured as an inbound exchange: %+v", ex)
		}
		ex.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("no capture for the delivery")
	}
}

func TestWebhookTapFailsOpenWithTheRecorderWedged(t *testing.T) {
	payload := make([]byte, 16<<10)
	rand.Read(payload)
	var served atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		served.Add(1)
		w.WriteHeader(200)
		w.Write(payload)
	}))
	defer receiver.Close()

	s := tapServer(t, receiver.URL, 1)
	front := httptest.NewServer(s)
	defer front.Close()

	const want = 25
	delivered, receiverDrops := 0, 0
	for delivered < want {
		if receiverDrops > want {
			t.Fatalf("the receiver dropped mid-response %d times; the environment is too unstable to assert on", receiverDrops)
		}
		before := s.Metrics.UpstreamBodyErrors.Load()
		resp, err := http.Post(front.URL+"/hooks/examplepay", "application/json", bytes.NewReader([]byte(`{"n":1}`)))
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || len(got) != len(payload) {
			if s.Metrics.UpstreamBodyErrors.Load() > before {
				receiverDrops++
				continue
			}
			t.Fatalf("delivery %d truncated at %d/%d bytes with a healthy receiver: %v; pikopod cut the delivery short", delivered, len(got), len(payload), readErr)
		}
		if resp.StatusCode != 200 || !bytes.Equal(got, payload) {
			t.Fatalf("delivery %d altered with nobody draining captures: %d %d bytes", delivered, resp.StatusCode, len(got))
		}
		delivered++
	}
	if receiverDrops > 0 {
		t.Logf("tolerated %d receiver drop(s), each counted by UpstreamBodyErrors", receiverDrops)
	}
	if int(served.Load()) < want {
		t.Fatalf("every delivery must reach the receiver: %d", served.Load())
	}
	if s.Metrics.WebhooksDropped.Load() == 0 {
		t.Fatal("test invalid: expected inbound captures to be dropped")
	}
}

func TestWebhookTapIsAbsentWithoutAReceiver(t *testing.T) {
	s := testServer(t, "https://api.example.invalid", 1)
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/hooks/examplepay", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("no receiver configured means no tap: %d", resp.StatusCode)
	}
}
