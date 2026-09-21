package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/errfmt"
)

const oldSpecYAML = `openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /widgets:
    get:
      responses:
        "200":
          description: ok
  /gadgets:
    get:
      responses:
        "200":
          description: ok
`

const newSpecYAML = `openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /widgets:
    get:
      parameters:
        - name: tenant
          in: query
          required: true
          schema: {type: string}
      responses:
        "200":
          description: ok
`

func specFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSpecDiffExitZeroOnClean(t *testing.T) {
	old := specFile(t, "old.yaml", oldSpecYAML)
	var out bytes.Buffer
	breaking, err := specDiff(old, old, specDiffOptions{Format: "text", FailOn: "ERR"}, &out, &out)
	if err != nil || breaking {
		t.Fatalf("identical specs are clean: breaking=%v err=%v", breaking, err)
	}
}

func TestSpecDiffExitOneOnBreaking(t *testing.T) {
	old, nu := specFile(t, "old.yaml", oldSpecYAML), specFile(t, "new.yaml", newSpecYAML)
	var out, errOut bytes.Buffer
	breaking, err := specDiff(old, nu, specDiffOptions{Format: "text", FailOn: "ERR"}, &out, &errOut)
	if err != nil || !breaking {
		t.Fatalf("a removed endpoint is breaking (exit 1): breaking=%v err=%v", breaking, err)
	}
	if !strings.Contains(errOut.String(), "failing the gate (exit 1)") {
		t.Fatalf("text mode explains the exit on stderr: %q", errOut.String())
	}
}

func TestSpecDiffExitTwoOnUnloadableSpec(t *testing.T) {
	old := specFile(t, "old.yaml", oldSpecYAML)
	bad := specFile(t, "bad.json", `{"openapi": "3.1.0", "paths": {`)
	var out bytes.Buffer
	breaking, err := specDiff(old, bad, specDiffOptions{Format: "text", FailOn: "ERR"}, &out, &out)
	var e *errfmt.E
	if err == nil || !errors.As(err, &e) || breaking {
		t.Fatalf("an unloadable document is an errfmt error (exit 2), never drift: breaking=%v err=%v", breaking, err)
	}
	_, err = specDiff(old, filepath.Join(t.TempDir(), "missing.yaml"), specDiffOptions{Format: "githubactions", FailOn: "ERR"}, &out, &out)
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("a missing document is an errfmt error: %v", err)
	}
}

func TestSpecDiffAnnotationsWrittenBeforeExit(t *testing.T) {
	old, nu := specFile(t, "old.yaml", oldSpecYAML), specFile(t, "new.yaml", newSpecYAML)
	var out, errOut bytes.Buffer
	breaking, err := specDiff(old, nu, specDiffOptions{Format: "githubactions", FailOn: "ERR"}, &out, &errOut)
	if err != nil || !breaking {
		t.Fatalf("breaking=%v err=%v", breaking, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("annotations must be on stdout before the exit: %q", out.String())
	}
	sawFile := false
	for _, l := range lines {
		if !strings.HasPrefix(l, "::") {
			t.Fatalf("only workflow commands on stdout: %q", l)
		}
		if strings.Contains(l, "file="+nu+",line=7,") && strings.Contains(l, "param-added-required") {
			sawFile = true
		}
	}
	if !sawFile {
		t.Fatalf("the added parameter is annotated at its line in the new file:\n%s", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("githubactions mode writes nothing to stderr: %q", errOut.String())
	}
}

func TestSpecDiffUnknownFormatIsErrfmt(t *testing.T) {
	old := specFile(t, "old.yaml", oldSpecYAML)
	var out bytes.Buffer
	_, err := specDiff(old, old, specDiffOptions{Format: "xml", FailOn: "ERR"}, &out, &out)
	var e *errfmt.E
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("want errfmt, got %v", err)
	}
	for _, want := range []string{"text", "json", "markdown", "githubactions"} {
		if !strings.Contains(e.Why, want) {
			t.Fatalf("the error lists every format, missing %q: %s", want, e.Why)
		}
	}
}

func TestSpecDiffResolvesRelativeRefsAtAGitRef(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	root := "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\npaths:\n  /charges:\n    get:\n      responses:\n        \"200\":\n          description: ok\n          content:\n            application/json:\n              schema:\n                $ref: './schemas/charge.yaml#/Charge'\n"
	os.MkdirAll(filepath.Join(dir, "api", "schemas"), 0o755)
	os.WriteFile(filepath.Join(dir, "api", "openapi.yaml"), []byte(root), 0o644)
	os.WriteFile(filepath.Join(dir, "api", "schemas", "charge.yaml"), []byte("Charge:\n  type: object\n  required: [id]\n  properties:\n    id: {type: string}\n"), 0o644)
	run("init", "-q", "-b", "main")
	run("add", ".")
	run("commit", "-q", "-m", "one")
	os.WriteFile(filepath.Join(dir, "api", "schemas", "charge.yaml"), []byte("Charge:\n  type: object\n  properties:\n    id: {type: string}\n"), 0o644)
	run("commit", "-q", "-am", "two")
	t.Chdir(dir)
	var out bytes.Buffer
	breaking, err := specDiff("git:HEAD~1:api/openapi.yaml", "git:HEAD:api/openapi.yaml", specDiffOptions{Format: "text", FailOn: "ERR"}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if breaking || !strings.Contains(out.String(), "response-property-became-optional") {
		t.Fatalf("the change inside the referenced file must be diffed at each ref: breaking=%v\n%s", breaking, out.String())
	}
}
