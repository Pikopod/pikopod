package demo

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDemoStoryPrintsTheAlert(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := Run(w)
	os.Stdout = orig
	w.Close()
	captured, _ := io.ReadAll(r)
	r.Close()
	if runErr != nil {
		t.Fatalf("demo must complete cleanly: %v", runErr)
	}
	got := string(captured)

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

func TestDemoTeachesTheLoopNotJustDetection(t *testing.T) {
	var buf bytes.Buffer
	if err := Run(&buf); err != nil {
		t.Fatalf("demo must complete cleanly: %v", err)
	}
	got := buf.String()

	if strings.Contains(got, "That's the product: ONE alert") {
		t.Fatal("the demo calls an alert 'the product' — detection is one stage of the loop")
	}
	for _, want := range []string{
		"the easy part",
		"loop",
		"scenario list",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("demo output never mentions %q — it teaches detection only:\n%s", want, got)
		}
	}
}
