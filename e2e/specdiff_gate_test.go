package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gateSpecV1 = `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /widgets:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
                required: [id]
                properties:
                  id: {type: string}
`

const gateSpecV2 = `openapi: 3.0.0
info: {title: T, version: "2"}
paths:
  /other:
    get:
      responses:
        "200": {description: ok}
`

func TestSpecDiffExitCodes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.yaml")
	niw := filepath.Join(dir, "new.yaml")
	os.WriteFile(old, []byte(gateSpecV1), 0o600)
	os.WriteFile(niw, []byte(gateSpecV2), 0o600)

	out, code := run(t, dir, "spec-diff", old, old)
	if code != 0 || !strings.Contains(out, "no declared changes") {
		t.Fatalf("identical specs: exit %d\n%s", code, out)
	}

	out, code = run(t, dir, "spec-diff", old, niw)
	if code != 1 || !strings.Contains(out, "endpoint-removed") {
		t.Fatalf("breaking diff: exit %d\n%s", code, out)
	}

	_, code = run(t, dir, "spec-diff", old, niw, "--fail-on", "INFO")
	if code != 1 {
		t.Fatalf("--fail-on INFO: exit %d", code)
	}

	_, code = run(t, dir, "spec-diff", niw, niw, "--fail-on", "ERR")
	if code != 0 {
		t.Fatalf("clean under floor: exit %d", code)
	}

	_, code = run(t, dir, "spec-diff", filepath.Join(dir, "missing.yaml"), niw)
	if code != 2 {
		t.Fatalf("missing file must be exit 2: %d", code)
	}

	handoff := filepath.Join(dir, "h.json")
	_, code = run(t, dir, "spec-diff", old, niw, "--handoff", handoff)
	if code != 1 {
		t.Fatalf("handoff run: exit %d", code)
	}
	raw, err := os.ReadFile(handoff)
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Source   string `json:"source"`
		Findings []struct {
			ID          string `json:"id"`
			Fingerprint string `json:"fingerprint"`
		} `json:"findings"`
	}
	if json.Unmarshal(raw, &h) != nil || h.Source != "spec-diff" || len(h.Findings) == 0 || h.Findings[0].Fingerprint == "" {
		t.Fatalf("handoff shape: %s", raw)
	}
}
