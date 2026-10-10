package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChaosEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ports := freePorts(t, 2)
	agentPort, sbxPort := ports[0], ports[1]
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(thingsSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("listen: 127.0.0.1\nagent_port: %d\nsandbox_port: %d\ndata_dir: data\nupstreams:\n  examplepay:\n    target: https://api.examplepay.invalid\n", agentPort, sbxPort)
	if err := os.WriteFile(filepath.Join(dir, "pikopod.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := run(t, dir, "import", "examplepay", "--spec", "spec.json", "--config", "pikopod.yaml"); code != 0 {
		t.Fatalf("import failed (%d): %s", code, out)
	}
	up := startUp(t, dir, agentPort)
	defer up.stop(t)

	client := &http.Client{Timeout: 5 * time.Second}
	post := func() int {
		t.Helper()
		resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/examplepay/things", sbxPort), "application/json", strings.NewReader(`{"amount": 5}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	if out, code := run(t, dir, "chaos", "examplepay", "--kind", "error", "--status", "503", "--method", "POST", "--path", "/things", "--config", "pikopod.yaml"); code != 0 {
		t.Fatalf("chaos arm failed (%d): %s", code, out)
	}
	if status := post(); status != http.StatusServiceUnavailable {
		t.Fatalf("POST with the error fault must return 503, got %d", status)
	}
	if out, code := run(t, dir, "chaos", "examplepay", "--clear", "--config", "pikopod.yaml"); code != 0 {
		t.Fatalf("chaos clear failed (%d): %s", code, out)
	}
	if status := post(); status != http.StatusCreated {
		t.Fatalf("POST after clearing the error fault must return 201, got %d", status)
	}
}
