package importer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const multiFileRoot = `openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /charges:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: './schemas/charge.yaml#/Charge'
`

const chargeFile = `Charge:
  type: object
  required: [id, amount]
  properties:
    id: {type: string}
    amount:
      $ref: 'money.yaml#/Money'
`

const moneyFile = `Money:
  type: object
  properties:
    value: {type: integer}
    currency: {type: string, enum: [NGN, USD]}
`

func gitRepo(t *testing.T, files map[string]string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	run("add", ".")
	run("commit", "-q", "-m", "spec")
	return dir
}

func gitLoader(dir, ref string) func(string) ([]byte, error) {
	return func(rel string) ([]byte, error) {
		cmd := exec.Command("git", "show", ref+":"+rel)
		cmd.Dir = dir
		return cmd.Output()
	}
}

func refuses(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), string(SpecRefUnresolvable)) {
		t.Fatalf("want SPEC_REF_UNRESOLVABLE, got %v", err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q in %q", want, err.Error())
	}
}

func TestRelativeRefResolvedFromGit(t *testing.T) {
	dir := gitRepo(t, map[string]string{"api/openapi.yaml": multiFileRoot, "api/schemas/charge.yaml": chargeFile, "api/schemas/money.yaml": moneyFile})
	raw, err := gitLoader(dir, "main")("api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	def, _, err := NormalizeOpenAPIFrom(raw, &Source{File: "api/openapi.yaml", Dir: "api", Load: gitLoader(dir, "main")})
	if err != nil {
		t.Fatal(err)
	}
	schema := def.Endpoints[0].Responses[0].Content[0].Schema
	names := map[string]string{}
	for _, p := range schema.Properties {
		names[p.Name] = p.Schema.Type.Value
		for _, q := range p.Schema.Properties {
			names[p.Name+"."+q.Name] = q.Schema.Type.Value
		}
	}
	if names["id"] != "string" || names["amount"] != "object" || names["amount.currency"] != "string" {
		t.Fatalf("the referenced schema and its own relative ref must be inlined: %v", names)
	}
}

func TestRelativeRefEscapingRepoRefused(t *testing.T) {
	for _, ref := range []string{"../../etc/passwd", "/etc/passwd", "schemas/../../outside.yaml"} {
		spec := strings.Replace(multiFileRoot, "./schemas/charge.yaml#/Charge", ref+"#/X", 1)
		_, _, err := NormalizeOpenAPIFrom([]byte(spec), &Source{File: "api/openapi.yaml", Dir: "api", Load: func(string) ([]byte, error) { return nil, fmt.Errorf("must not be called") }})
		refuses(t, err, "")
	}
}

func TestRemoteRefStillRefused(t *testing.T) {
	for _, ref := range []string{"https://example.com/s.yaml#/X", "//evil.com/s.yaml", "file:///etc/passwd#/X"} {
		spec := strings.Replace(multiFileRoot, "./schemas/charge.yaml#/Charge", ref, 1)
		_, _, err := NormalizeOpenAPIFrom([]byte(spec), &Source{File: "openapi.yaml", Load: func(string) ([]byte, error) { return nil, fmt.Errorf("must not be called") }})
		refuses(t, err, "not permitted")
		if _, err := NormalizeOpenAPI([]byte(spec)); err == nil || !strings.Contains(err.Error(), "SPEC_REF_UNRESOLVABLE") {
			t.Fatalf("without a source the refusal stands: %v", err)
		}
	}
}

func TestCrossFileCycleRefused(t *testing.T) {
	files := map[string][]byte{
		"a.yaml": []byte("A:\n  $ref: 'b.yaml#/B'\n"),
		"b.yaml": []byte("B:\n  $ref: 'a.yaml#/A'\n"),
	}
	spec := strings.Replace(multiFileRoot, "./schemas/charge.yaml#/Charge", "a.yaml#/A", 1)
	_, _, err := NormalizeOpenAPIFrom([]byte(spec), &Source{File: "openapi.yaml", Load: func(p string) ([]byte, error) { return files[p], nil }})
	refuses(t, err, "cyclic")
}

func TestFileBudgetEnforced(t *testing.T) {
	files := map[string][]byte{}
	var parts []string
	for i := 0; i <= maxRefFiles; i++ {
		name := fmt.Sprintf("s%d.yaml", i)
		files[name] = []byte("X:\n  type: string\n")
		parts = append(parts, fmt.Sprintf("            p%d:\n              $ref: '%s#/X'\n", i, name))
	}
	spec := "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\npaths:\n  /x:\n    get:\n      responses:\n        \"200\":\n          description: ok\n          content:\n            application/json:\n              schema:\n                type: object\n                properties:\n" +
		strings.ReplaceAll(strings.Join(parts, ""), "            p", "                  p")
	spec = strings.ReplaceAll(spec, "              $ref", "                    $ref")
	_, _, err := NormalizeOpenAPIFrom([]byte(spec), &Source{File: "openapi.yaml", Load: func(p string) ([]byte, error) { return files[p], nil }})
	refuses(t, err, "file budget")
}

func TestExternalRefWithoutSourceIsRefused(t *testing.T) {
	_, err := NormalizeOpenAPI([]byte(multiFileRoot))
	if err == nil || !strings.Contains(err.Error(), "SPEC_REF_UNRESOLVABLE") {
		t.Fatalf("a URL-origin or sourceless document keeps refusing relative refs: %v", err)
	}
}
