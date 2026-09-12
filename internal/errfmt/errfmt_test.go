package errfmt

import (
	"strings"
	"testing"
)

func TestContractShape(t *testing.T) {
	err := New("cannot start sandbox", "port 4600 is already in use", "stop the other process or set sandbox.port in pikopod.yaml", "docs/config-reference.md#ports")
	got := err.Error()
	for _, part := range []string{"cannot start sandbox: ", "port 4600 is already in use", " → stop the other process", " → https://github.com/pikopod/pikopod/blob/main/docs/config-reference.md#ports"} {
		if !strings.Contains(got, part) {
			t.Fatalf("contract part missing %q in %q", part, got)
		}
	}
}

func TestDocsOptionalAndAbsolute(t *testing.T) {
	if got := New("x", "y", "z", "").Error(); strings.Count(got, "→") != 1 {
		t.Fatalf("empty docs must omit second arrow: %q", got)
	}
	if got := New("x", "y", "z", "https://example.com/p").Error(); !strings.HasSuffix(got, "https://example.com/p") {
		t.Fatalf("absolute docs URL must pass through: %q", got)
	}
}
