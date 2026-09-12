package demo

import (
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The demo IS the README's step-1 promise: fake provider, real agent,
// silent change, drift alert in the terminal. This runs the whole scripted
// story in-process and asserts the promise, not the prose. Alerts flow
// through the real StdoutSink, so the test captures stdout — the same
// stream a first-run user reads.
func TestDemoStoryPrintsTheAlert(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := Run(w) // narration and alerts land on one captured stream
	os.Stdout = orig
	w.Close()
	captured, _ := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("demo must complete cleanly: %v", runErr)
	}
	got := string(captured)

	// The silent change produced real, replayable alerts: at least the enum
	// rename must page, with a fingerprint wired into the from-drift loop.
	if !strings.Contains(got, "pikopod drift") {
		t.Fatalf("no drift alert in the demo output:\n%s", got)
	}
	if !strings.Contains(got, `"success`) && !strings.Contains(got, "succeeded") {
		t.Fatalf("the alert must show the actual enum diff:\n%s", got)
	}
	fps := regexp.MustCompile(`from-drift (fp_[0-9a-f]{12})`).FindAllStringSubmatch(got, -1)
	if len(fps) == 0 {
		t.Fatalf("every alert must carry its replay hint:\n%s", got)
	}
	// Dedupe held: 10 drifted requests, but each fingerprint alerted once.
	perFP := map[string]int{}
	for _, m := range fps {
		perFP[m[1]]++
	}
	for fp, n := range perFP {
		if n > 1 {
			t.Fatalf("fingerprint %s alerted %d times — dedupe broke:\n%s", fp, n, got)
		}
	}
}
