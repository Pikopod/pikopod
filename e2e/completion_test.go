package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestDocumentedShellCompletion(t *testing.T) {
	raw, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "### Shell completion\n")
	if !ok {
		t.Fatal("installation instructions are missing the Shell completion section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	for _, tc := range []struct {
		shell  string
		line   string
		marker string
	}{
		{"bash", "source <(pikopod completion bash)", "complete -o default -F __start_pikopod pikopod"},
		{"zsh", "source <(pikopod completion zsh)", "compdef _pikopod pikopod"},
		{"fish", "pikopod completion fish | source", "complete -c pikopod"},
		{"powershell", "pikopod completion powershell | Out-String | Invoke-Expression", "Register-ArgumentCompleter -CommandName 'pikopod'"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			if !strings.Contains(section, "\n"+tc.line+"\n") {
				t.Errorf("Shell completion section is missing one-liner %q", tc.line)
			}
			out, code := run(t, t.TempDir(), "completion", tc.shell)
			if code != 0 {
				t.Fatalf("completion %s exited %d:\n%s", tc.shell, code, out)
			}
			if !strings.Contains(out, tc.marker) {
				t.Errorf("completion %s did not register pikopod completion:\n%s", tc.shell, out)
			}
		})
	}
}
