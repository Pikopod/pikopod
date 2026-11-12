package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/config"
	"github.com/pikopod/pikopod/internal/sandbox"
)

func requestsCLIServer(t *testing.T) string {
	t.Helper()
	spec, err := filepath.Abs(widgetsSpecPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := cliDir(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	yaml := fmt.Sprintf("listen: 127.0.0.1\nsandbox_port: %d\ndata_dir: %s\nupstreams:\n  widgets:\n    target: https://api.example.invalid\n", port, filepath.Join(dir, "data"))
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(yaml), 0o600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	cfg, err := config.Load("")
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if err := sandboxAdd(cfg, "widgets", spec, "requests-json-1", "", "", false, io.Discard); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	sbx, err := newSandboxServer(cfg)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(sbx)
	server.Listener = listener
	server.Start()
	t.Cleanup(func() {
		server.Close()
		if err := sbx.Close(); err != nil {
			t.Error(err)
		}
	})
	return server.URL
}

func controlPlaneRequests(t *testing.T, baseURL string) []sandbox.JournalEntry {
	t.Helper()
	res, err := http.Get(baseURL + "/_pikopod/v1/sandboxes/widgets/requests?limit=50")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("control plane requests = %d, want 200", res.StatusCode)
	}
	var payload struct {
		Requests []sandbox.JournalEntry `json:"requests"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Requests
}

func TestSandboxRequestsJSONFormat(t *testing.T) {
	baseURL := requestsCLIServer(t)
	res, err := http.Post(baseURL+"/widgets/widgets", "application/json", strings.NewReader(`{"name":"gear","size":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create request = %d, want 201", res.StatusCode)
	}
	res.Body.Close()

	want := controlPlaneRequests(t, baseURL)
	defaultText, err := runCLI(t, newRootCmd(), "requests", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	explicitText, err := runCLI(t, newRootCmd(), "requests", "widgets", "--format", "text")
	if err != nil {
		t.Fatal(err)
	}
	if explicitText != defaultText {
		t.Fatalf("explicit text output changed default output:\ndefault: %q\ntext: %q", defaultText, explicitText)
	}

	gotJSON, err := runCLI(t, newRootCmd(), "requests", "widgets", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got []sandbox.JournalEntry
	if err := json.Unmarshal([]byte(gotJSON), &got); err != nil {
		t.Fatalf("JSON output is not a request array: %q: %v", gotJSON, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON output differs from the control plane request array:\ngot:  %#v\nwant: %#v", got, want)
	}

	resetJSON, err := runCLI(t, newRootCmd(), "requests", "widgets", "--format", "json", "--reset")
	if err != nil {
		t.Fatal(err)
	}
	var reset map[string]any
	if err := json.Unmarshal([]byte(resetJSON), &reset); err != nil || reset["reset"] != true {
		t.Fatalf("JSON reset output = %q (err %v), want a reset acknowledgement", resetJSON, err)
	}
	if got := controlPlaneRequests(t, baseURL); len(got) != 0 {
		t.Fatalf("JSON reset left %d request(s) in the journal", len(got))
	}
}
