package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRCommentEndToEnd(t *testing.T) {
	var comments []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "GET":
			out := []map[string]any{}
			for i, c := range comments {
				out = append(out, map[string]any{"id": i + 1, "body": c["body"], "user": map[string]any{"id": 1}})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == "POST":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			comments = append(comments, in)
			w.WriteHeader(201)
			w.Write([]byte("{}"))
		default:
			http.Error(w, r.URL.Path, 500)
		}
	}))
	defer srv.Close()

	handoff := filepath.Join(t.TempDir(), "h.json")
	os.WriteFile(handoff, []byte(`{"source":"replay-ci","records":3,"findings":[{"upstream":"pay","method":"GET","template":"/tx","kind":"field_removed","field":"fee","detail":"gone"}]}`), 0o600)

	t.Setenv("GITHUB_TOKEN", "test-token")
	cmd := newPRCommentCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	cmd.Flags().Set("handoff", handoff)
	cmd.Flags().Set("platform", "github")
	cmd.Flags().Set("repo", "o/r")
	cmd.Flags().Set("number", "5")
	cmd.Flags().Set("sha", "abc")
	cmd.Flags().Set("api", srv.URL)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments: %d", len(comments))
	}
	body := comments[0]["body"].(string)
	if !strings.Contains(body, "pikopod:comment source=replay-ci sha=abc") || !strings.Contains(body, "field_removed") {
		t.Fatalf("body: %s", body)
	}
	if !strings.Contains(out.String(), "created") {
		t.Fatalf("out: %s", out.String())
	}
}

func TestPRCommentDegradesToStepSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}
		if r.Method == "GET" {
			w.Write([]byte("[]"))
			return
		}
		http.Error(w, "read-only token", http.StatusForbidden)
	}))
	defer srv.Close()

	dir := t.TempDir()
	handoff := filepath.Join(dir, "h.json")
	os.WriteFile(handoff, []byte(`{"source":"replay-ci","records":0,"findings":[]}`), 0o600)
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_TOKEN", "t")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)

	cmd := newPRCommentCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	for k, v := range map[string]string{"handoff": handoff, "platform": "github", "repo": "o/r", "number": "5", "api": srv.URL} {
		cmd.Flags().Set(k, v)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("degradation must not error: %v", err)
	}
	raw, err := os.ReadFile(summary)
	if err != nil || !strings.Contains(string(raw), "replay --ci") {
		t.Fatalf("summary fallback: %v %s", err, raw)
	}
}

func TestPROpenDryRun(t *testing.T) {
	handoff := filepath.Join(t.TempDir(), "h.json")
	os.WriteFile(handoff, []byte(`{"source":"spec-update","upstream":"pay","applied":[{"kind":"enum-union","method":"GET","template":"/tx","reason":"r","evidence":{"occurrences":2}}]}`), 0o600)

	cmd := newPROpenCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	for k, v := range map[string]string{"handoff": handoff, "branch": "pikopod/test", "base": "main", "title": "t", "dry-run": "true"} {
		cmd.Flags().Set(k, v)
	}
	cmd.Flags().Set("commit", "openapi.yaml")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"git checkout -b pikopod/test", "git add -- openapi.yaml", "git push -u origin pikopod/test", "pikopod/test → main"} {
		if !strings.Contains(s, want) {
			t.Fatalf("dry-run plan missing %q:\n%s", want, s)
		}
	}
}

// The LAST rung of the degradation ladder: read-only token AND no job
// summary — the report lands on stdout, exit stays 0.
func TestPRCommentDegradesToStdout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}
		if r.Method == "GET" {
			w.Write([]byte("[]"))
			return
		}
		http.Error(w, "read-only token", http.StatusForbidden)
	}))
	defer srv.Close()

	handoff := filepath.Join(t.TempDir(), "h.json")
	os.WriteFile(handoff, []byte(`{"source":"replay-ci","records":2,"findings":[]}`), 0o600)
	t.Setenv("GITHUB_TOKEN", "t")
	t.Setenv("GITHUB_STEP_SUMMARY", "") // no job summary available

	cmd := newPRCommentCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	for k, v := range map[string]string{"handoff": handoff, "platform": "github", "repo": "o/r", "number": "5", "api": srv.URL} {
		cmd.Flags().Set(k, v)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("last rung must not error: %v", err)
	}
	if !strings.Contains(out.String(), "replay --ci") {
		t.Fatalf("report must land on stdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "token cannot comment") {
		t.Fatalf("stderr must carry the notice: %s", errOut.String())
	}
}

// argv discipline on pr open: option-like branch names never reach git.
func TestPROpenRefusesDashBranch(t *testing.T) {
	handoff := filepath.Join(t.TempDir(), "h.json")
	os.WriteFile(handoff, []byte(`{"source":"replay-ci","records":0,"findings":[]}`), 0o600)
	cmd := newPROpenCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	for k, v := range map[string]string{"handoff": handoff, "branch": "--upload-pack=/tmp/x", "base": "main", "dry-run": "true"} {
		cmd.Flags().Set(k, v)
	}
	cmd.Flags().Set("commit", "x.yaml")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("dash-prefixed branch must be refused before any git argv")
	}
}
