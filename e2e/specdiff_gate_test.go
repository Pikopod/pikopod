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

// v2 removes the endpoint — an ERR-grade declared break.
const gateSpecV2 = `openapi: 3.0.0
info: {title: T, version: "2"}
paths:
  /other:
    get:
      responses:
        "200": {description: ok}
`

// The spec-diff exit-code contract IS the API CI scripts consume — pinned
// against the built binary because the in-process command os.Exit(1)s.
func TestSpecDiffExitCodes(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.yaml")
	niw := filepath.Join(dir, "new.yaml")
	os.WriteFile(old, []byte(gateSpecV1), 0o600)
	os.WriteFile(niw, []byte(gateSpecV2), 0o600)

	// Identical specs: exit 0.
	out, code := run(t, dir, "spec-diff", old, old)
	if code != 0 || !strings.Contains(out, "no declared changes") {
		t.Fatalf("identical specs: exit %d\n%s", code, out)
	}

	// Breaking change at the default ERR floor: exit 1.
	out, code = run(t, dir, "spec-diff", old, niw)
	if code != 1 || !strings.Contains(out, "endpoint-removed") {
		t.Fatalf("breaking diff: exit %d\n%s", code, out)
	}

	// INFO floor trips on ANY finding (the added endpoint alone).
	_, code = run(t, dir, "spec-diff", old, niw, "--fail-on", "INFO")
	if code != 1 {
		t.Fatalf("--fail-on INFO: exit %d", code)
	}

	// Additive-only change under the ERR floor: exit 0.
	_, code = run(t, dir, "spec-diff", niw, niw, "--fail-on", "ERR")
	if code != 0 {
		t.Fatalf("clean under floor: exit %d", code)
	}

	// Unreadable source is a TOOL error: exit 2, never 1.
	_, code = run(t, dir, "spec-diff", filepath.Join(dir, "missing.yaml"), niw)
	if code != 2 {
		t.Fatalf("missing file must be exit 2: %d", code)
	}

	// --handoff writes the machine-consumable report.
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
