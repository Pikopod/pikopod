package fix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/alert"
	"github.com/pikopod/pikopod/internal/drift"
)

func TestTermsPrefersFieldLeaf(t *testing.T) {
	ev := &alert.DriftEvent{Field: "data/customer/account_number", Endpoint: "/transaction/verify/{id}"}
	got := Terms(ev)
	if len(got) != 1 || got[0] != "account_number" {
		t.Fatalf("terms: %v", got)
	}
}

func TestTermsFallsBackToEndpointSegments(t *testing.T) {
	ev := &alert.DriftEvent{Endpoint: "/transaction/verify/{id}"}
	got := Terms(ev)
	if len(got) != 2 || got[0] != "transaction" || got[1] != "verify" {
		t.Fatalf("terms: %v", got)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanFindsReferencesAndSkipsNoise(t *testing.T) {
	root := writeTree(t, map[string]string{
		"pay/client.go":           "package pay\n\nfunc Verify() {\n\tamount := resp.AccountNumber // account_number\n\t_ = amount\n}\n",
		"docs/readme.txt":         "account_number mentioned in prose",
		"node_modules/x/index.js": "account_number",
		".hidden/secret.go":       "account_number",
		"unrelated/other.go":      "package other\n",
	})
	impacts, err := Scan(root, []string{"account_number"})
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) != 1 || impacts[0].File != "pay/client.go" {
		t.Fatalf("impacts: %+v", impacts)
	}
	if !strings.Contains(impacts[0].Excerpt, "account_number") {
		t.Fatalf("excerpt missing match: %q", impacts[0].Excerpt)
	}
	if !strings.Contains(impacts[0].Excerpt, "4\t") {
		t.Fatalf("excerpt must carry line numbers: %q", impacts[0].Excerpt)
	}
}

func TestParseProposalRefusesFileOutsideImpactSet(t *testing.T) {
	impacts := []Impact{{File: "pay/client.go"}}
	raw := map[string]any{"edits": []any{
		map[string]any{"file": "../../etc/passwd", "find": "a", "replace": "b"},
	}}
	if _, err := ParseProposal(raw, impacts); err == nil {
		t.Fatal("a file outside the impact set must be refused")
	}
}

func TestParseProposalRefusesNoOpEdit(t *testing.T) {
	impacts := []Impact{{File: "a.go"}}
	raw := map[string]any{"edits": []any{
		map[string]any{"file": "a.go", "find": "x", "replace": "x"},
	}}
	if _, err := ParseProposal(raw, impacts); err == nil {
		t.Fatal("a no-op edit must be refused")
	}
}

func TestParseProposalAcceptsValidEdits(t *testing.T) {
	impacts := []Impact{{File: "a.go"}}
	raw := map[string]any{
		"summary": "rename field",
		"edits": []any{
			map[string]any{"file": "a.go", "find": "old_name", "replace": "new_name"},
		},
	}
	p, err := ParseProposal(raw, impacts)
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary != "rename field" || len(p.Edits) != 1 || p.Edits[0].Replace != "new_name" {
		t.Fatalf("proposal: %+v", p)
	}
}

func TestApplyAndRevert(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go": "package a\n\nvar Field = \"account_number\"\n",
	})
	changed, revert, err := Apply(root, []Edit{{File: "a.go", Find: "account_number", Replace: "accountNumber"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != "a.go" {
		t.Fatalf("changed: %v", changed)
	}
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(after), "accountNumber") {
		t.Fatalf("edit not applied: %s", after)
	}
	if err := revert(); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(orig), `"account_number"`) {
		t.Fatalf("revert did not restore: %s", orig)
	}
}

func TestApplyRefusesAmbiguousFind(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go": "x = 1\nx = 1\n",
	})
	if _, _, err := Apply(root, []Edit{{File: "a.go", Find: "x = 1", Replace: "x = 2"}}); err == nil {
		t.Fatal("a non-unique find must be refused, never 'first occurrence wins'")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(raw) != "x = 1\nx = 1\n" {
		t.Fatalf("refusal must leave the file untouched: %q", raw)
	}
}

func TestApplyRefusesPathEscape(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Apply(root, []Edit{{File: "../outside.go", Find: "a", Replace: "b"}}); err == nil {
		t.Fatal("path escape must be refused")
	}
}

func TestApplyTwoEditsSameFileSequential(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.go": "alpha\nbeta\n",
	})
	changed, _, err := Apply(root, []Edit{
		{File: "a.go", Find: "alpha", Replace: "ALPHA"},
		{File: "a.go", Find: "beta", Replace: "BETA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Fatalf("one file changed, got %v", changed)
	}
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(after) != "ALPHA\nBETA\n" {
		t.Fatalf("after: %q", after)
	}
}

var _ = drift.FieldRemoved

func TestTermsShortLeafIgnored(t *testing.T) {

	ev := &alert.DriftEvent{Field: "id", Endpoint: "/charges/{id}", Kind: drift.FieldRemoved}
	got := Terms(ev)
	if len(got) != 1 || got[0] != "charges" {
		t.Fatalf("terms: %v", got)
	}
}
