package e2e

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDemo exercises the public first-run path through the built binary. The
// in-process demo tests cover the implementation details; this test protects
// the CLI wiring, process exit status, and filesystem boundary users actually
// rely on.
func TestDemo(t *testing.T) {
	dir := t.TempDir()
	out, code := run(t, dir, "demo")
	if code != 0 {
		t.Fatalf("demo must exit cleanly, got %d:\n%s", code, out)
	}

	if got := strings.Count(out, "[ERR] pikopod drift"); got == 0 {
		t.Fatalf("demo must print at least one drift alert:\n%s", out)
	}

	// Fingerprints are generated at runtime. Each one must be replayable and
	// must occur only once in the replay hints, proving dedupe across the ten
	// requests sent after the provider changes.
	replayRE := regexp.MustCompile(`(?m)^replay it: pikopod scenario from-drift (fp_[0-9a-f]{12})$`)
	matches := replayRE.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		t.Fatalf("demo must print a replayable fingerprint:\n%s", out)
	}
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		fp := match[1]
		if seen[fp] {
			t.Fatalf("fingerprint %s was alerted more than once:\n%s", fp, out)
		}
		seen[fp] = true
		if strings.Count(out, fp) != 2 {
			t.Fatalf("fingerprint %s must appear in its alert and replay hint exactly once each:\n%s", fp, out)
		}
	}
	if got := strings.Count(out, "[ERR] pikopod drift"); got != len(matches) {
		t.Fatalf("each drift alert must have exactly one replay hint: %d alerts, %d hints:\n%s", got, len(matches), out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("inspect demo working directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("demo must not write outside its private temporary directory; found %d entries", len(entries))
	}
}
